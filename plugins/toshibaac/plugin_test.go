package toshibaac

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
	"github.com/harrymitchinson/oikade/internal/testkit"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

const (
	waitLimit = 20 * time.Second
	username  = "someone@example.com"
	password  = "correct horse"

	// The units, by the unique ID their messages carry, and the device IDs it gives.
	bedroomUnique = "A1B2C3D4E5F6"
	bedroom       = "a1b2c3d4e5f6"
	studyUnique   = "0A0B0C0D0E0F"
	study         = "0a0b0c0d0e0f"
)

var root = contractv1.NewRoot("toshiba")

// fast keeps the real timing's order, in less time. The hub's keep-alive is whole seconds, and mochi
// allows one of 1 s only 1 s, so a ping can come too late.
var fast = timing{
	reload: time.Hour, keepAlive: 2 * time.Second, retry: 10 * time.Millisecond, retryMax: 40 * time.Millisecond,
	limited: 50 * time.Millisecond, refused: time.Hour, secret: 10 * time.Millisecond, request: 2 * time.Second, set: 2 * time.Second, intent: time.Minute,
}

// home is an account with a heating bedroom and a study that's off, in two groups.
func home(c *fakeCloud) {
	c.groups = [][]unit{
		{{ID: "ac-1", UniqueID: bedroomUnique, Name: "Bedroom", State: hexState(0x30, 0x43, 21, 0, 19)}},
		{{ID: "ac-2", UniqueID: studyUnique, Name: "Study", State: hexState(0x31, 0x42, 24, 0, 23)}},
	}
	c.states = map[string]string{"ac-1": c.groups[0][0].State, "ac-2": c.groups[1][0].State}
}

// logs is a log the tests can read.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logs) count(msg string) int {
	return strings.Count(l.String(), `"msg":"`+msg+`"`)
}

// bench is the plugin under a broker, with every value each of its topics was published with.
type bench struct {
	t       *testing.T
	cloud   *fakeCloud
	secrets string
	logs    *logs
	broker  string
	handler *bridge
	// waits are the backoffs the plugin waited.
	waits chan time.Duration

	mu     sync.Mutex
	topics map[string][]string
}

func start(t *testing.T, cloud *fakeCloud, secret string, timings timing, declared ...string) *bench {
	t.Helper()
	broker := mqtttest.StartBroker(t)
	b := &bench{
		t: t, cloud: cloud, secrets: t.TempDir(), logs: &logs{}, broker: broker,
		topics: map[string][]string{}, waits: make(chan time.Duration, 100),
	}
	if secret != "" {
		b.secret(secret)
	}
	timings.backoff = func(ctx context.Context, d time.Duration) {
		select {
		case b.waits <- d:
		default:
		}
		sleep(ctx, min(d, 50*time.Millisecond))
	}
	listening, stopListening := context.WithCancel(t.Context())
	t.Cleanup(stopListening)
	bus, err := mqtt.Connect(listening, broker, "toshiba-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe(listening, root.Filter(), func(m mqtt.Message) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.topics[m.Topic] = append(b.topics[m.Topic], string(m.Payload))
	}); err != nil {
		t.Fatal(err)
	}

	env := plugin.Environment{MQTTURL: broker, Root: root, SecretsDir: b.secrets}
	devices := make([]contractv1.DeclaredDevice, 0, len(declared))
	for _, id := range declared {
		devices = append(devices, contractv1.DeclaredDevice{ID: id, Name: id, DisplayName: id})
	}
	handler, err := newBridge(cloud.config(), env, devices, timings, slog.New(slog.NewJSONHandler(b.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	b.handler = handler
	ctx, cancel := context.WithCancel(t.Context())
	started, startCancel := context.WithTimeout(ctx, waitLimit)
	defer startCancel()
	p, err := plugin.Start(started, plugin.Options{Environment: env, OnSet: b.handler.Set, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := b.handler.Serve(ctx, p); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = p.Close(context.Background()) //nolint:usetesting // t.Context() is done once cleanups run
	})
	return b
}

func (b *bench) secret(value string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.secrets, "toshiba"), []byte(value+"\n"), 0o600); err != nil {
		b.t.Fatal(err)
	}
}

// topic is a topic's last value.
func (b *bench) topic(name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v := b.topics[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// history is every value a topic was published with.
func (b *bench) history(name string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.topics[name])
}

func (b *bench) eventually(what string, ok func() bool) {
	b.t.Helper()
	defer func() {
		if b.t.Failed() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.t.Logf("Topics: %v\nLogs: %s", b.topics, b.logs.String())
		}
	}()
	testkit.Eventually(b.t, what, ok)
}

func (b *bench) devices() contractv1.Devices {
	b.t.Helper()
	var d contractv1.Devices
	if raw := b.topic(root.Devices()); raw != "" {
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			b.t.Fatal(err)
		}
	}
	return d
}

func (b *bench) status() contractv1.Status {
	b.t.Helper()
	var s contractv1.Status
	if raw := b.topic(root.Status()); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			b.t.Fatal(err)
		}
	}
	return s
}

func (b *bench) waitAvailability(id, want string) {
	b.t.Helper()
	b.eventually(fmt.Sprintf("%s %s", id, want), func() bool { return b.topic(root.Availability(id)) == want })
}

func (b *bench) waitState(id, want string) {
	b.t.Helper()
	b.eventually(fmt.Sprintf("%s state %s", id, want), func() bool { return b.topic(root.State(id)) == want })
}

func (b *bench) waitProblem(part string) {
	b.t.Helper()
	b.eventually("the problem "+part, func() bool { return strings.Contains(b.status().Error, part) })
}

func TestTheAccountsUnitsAreReportedByTheirUniqueID(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, password, fast, "ffffffffffff")
	b.waitState(bedroom, `{"mode":"heat","on":true,"setpoint":21,"temperature":19}`)
	b.waitState(study, `{"mode":"cool","on":false,"setpoint":24,"temperature":23}`)
	b.waitAvailability(bedroom, "online")
	b.waitAvailability(study, "online")

	caps := []string{"mode", "on", "setpoint", "temperature"}
	want := []contractv1.DeviceReport{
		{ID: study, Found: true, Manufacturer: "Toshiba", Capabilities: caps},
		{ID: bedroom, Found: true, Manufacturer: "Toshiba", Capabilities: caps},
		// On record and not in the account: not found, and kept.
		{ID: "ffffffffffff"},
	}
	got := b.devices()
	if !reflect.DeepEqual(got.Devices, want) {
		t.Errorf("devices:\n got %+v\nwant %+v", got.Devices, want)
	}
	if len(got.Discovered) != 2 || got.Discovered[0].Name != "Study" || got.Discovered[1].Name != "Bedroom" {
		t.Errorf("discovered: %+v", got.Discovered)
	}
	if s := b.status(); s.State != contractv1.Online || s.Error != "" {
		t.Errorf("status: %+v", s)
	}

	// It registers as one mobile device, which it connects to the hub as.
	client := cloud.config().clientID()
	if got, want := cloud.posted(registerPath), map[string]string{"DeviceID": client, "DeviceType": "1", "Username": username}; !reflect.DeepEqual(got, want) {
		t.Errorf("registered %v, want %v", got, want)
	}
	if got := cloud.hubLogins(); len(got) != 1 || got[0][1] != client {
		t.Errorf("hub logins %v, want one as %s", got, client)
	}
}

func TestTheHubsCallsChangeAUnit(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, password, fast)
	b.waitAvailability(bedroom, "online")

	// Someone switches the bedroom to cooling at 25 in the app.
	rid := cloud.method(methodName, cmdState, bedroomUnique, map[string]string{"data": partial(0xff, 0x42, 25)})
	b.waitState(bedroom, `{"mode":"cool","on":true,"setpoint":25,"temperature":19}`)
	b.eventually("the call answered", func() bool {
		return slices.Contains(cloud.answered(), "$iothub/methods/res/0/?$rid="+rid+" null")
	})

	// A heartbeat carries the room's temperature, among fields of any type.
	cloud.method(methodName, cmdHeartbeat, bedroomUnique, map[string]any{"iTemp": "17", "oTemp": "0c", "rssi": -60})
	b.waitState(bedroom, `{"mode":"cool","on":true,"setpoint":25,"temperature":23}`)

	// Another unit's, another method's and a report it can't read change nothing, and are answered.
	cloud.method(methodName, cmdState, "not-a-unit", map[string]string{"data": partial(0x31, 0x42, 25)})
	other := cloud.method("other", cmdState, bedroomUnique, map[string]string{"data": partial(0x31, 0x42, 25)})
	cloud.method(methodName, cmdState, bedroomUnique, map[string]string{"data": "not hex"})
	b.eventually("the other method answered", func() bool {
		return slices.Contains(cloud.answered(), "$iothub/methods/res/0/?$rid="+other+" null")
	})
	cloud.method(methodName, cmdState, studyUnique, map[string]string{"data": partial(0x30, 0xff, -1)})
	b.waitState(study, `{"mode":"cool","on":true,"setpoint":24,"temperature":23}`)
	if got := b.topic(root.State(bedroom)); got != `{"mode":"cool","on":true,"setpoint":25,"temperature":23}` {
		t.Errorf("bedroom after calls that weren't its own: %s", got)
	}
	if b.logs.count("Could not read Toshiba unit's report") != 1 {
		t.Errorf("the unreadable report wasn't logged once:\n%s", b.logs)
	}
}

func TestAModeThePluginDoesntKnowIsLoggedOnce(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(func(c *fakeCloud) {
		c.groups = [][]unit{{{ID: "ac-1", UniqueID: bedroomUnique, Name: "Bedroom", State: hexState(0x30, 0x47, 21, 0, 19)}}}
	})
	b := start(t, cloud, password, fast)
	b.waitState(bedroom, `{"on":true,"setpoint":21,"temperature":19}`)
	cloud.method(methodName, cmdState, bedroomUnique, map[string]string{"data": partial(0xff, 0x47, 22)})
	b.waitState(bedroom, `{"on":true,"setpoint":22,"temperature":19}`)
	if n := b.logs.count("Skipped Toshiba mode the plugin doesn't know"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, b.logs)
	}
}

func TestTheCloudGoingAwayLeavesEveryUnitOfflineUntilItsBack(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, password, fast)
	b.waitAvailability(bedroom, "online")

	cloud.set(func(c *fakeCloud) { c.down, c.refuseHub = true, true })
	cloud.kick()
	b.waitAvailability(bedroom, "offline")
	b.waitAvailability(study, "offline")
	b.waitProblem("could not reach Toshiba's cloud")
	if s := b.status(); s.State != contractv1.Online {
		t.Errorf("the plugin went %s with the cloud", s.State)
	}
	// Its units stay on record, and it says it can't see the account's list rather than that it's empty.
	if d := b.devices(); len(d.Devices) != 2 || !d.Devices[0].Found || d.Discovered != nil {
		t.Errorf("devices while the cloud is down: %+v", d)
	}

	cloud.set(func(c *fakeCloud) { c.down, c.refuseHub = false, false })
	b.waitAvailability(bedroom, "online")
	b.eventually("the problem to clear", func() bool { return b.status().Error == "" })
}

func TestTheHubDroppingTheConnectionLogsInAgain(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, password, fast)
	b.waitAvailability(bedroom, "online")

	// The hub refuses the token while it's away, so the plugin backs off.
	cloud.set(func(c *fakeCloud) { c.refuseHub = true })
	cloud.kick()
	b.waitAvailability(bedroom, "offline")
	b.waitProblem("refused the plugin's credentials")
	cloud.set(func(c *fakeCloud) { c.refuseHub = false })
	b.waitAvailability(bedroom, "online")

	// Calls reach it on the new connection.
	cloud.method(methodName, cmdState, studyUnique, map[string]string{"data": partial(0x30, 0xff, 20)})
	b.waitState(study, `{"mode":"cool","on":true,"setpoint":20,"temperature":23}`)
}

func TestARefusedPasswordWaitsForANewOne(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, "not the password", fast, bedroom)
	b.waitProblem(`check the secret "toshiba"`)
	if d := b.devices(); len(d.Devices) != 1 || d.Devices[0].Found {
		t.Errorf("devices before a login: %+v", d)
	}
	// It doesn't try the same password again, which could lock the account.
	time.Sleep(10 * fast.secret)
	if n := cloud.count(); n != 1 {
		t.Errorf("tried the refused password %d times", n)
	}
	b.secret(password)
	b.waitAvailability(bedroom, "online")
	b.eventually("the problem to clear", func() bool { return b.status().Error == "" })
}

func TestNoSecretWaitsForOne(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, "", fast)
	b.waitProblem("waiting for the secret toshiba")
	if n := cloud.count(); n != 0 {
		t.Errorf("logged in %d times without a password", n)
	}
	b.secret(password)
	b.waitAvailability(bedroom, "online")
}

// TestTooManyRequestsWaitsLonger is the firewall's 403 and the login's 429.
func TestTooManyRequestsWaitsLonger(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			cloud := newFakeCloud(t)
			cloud.set(home)
			cloud.set(func(c *fakeCloud) { c.limited = status })
			b := start(t, cloud, password, fast)
			b.waitProblem("too many requests")
			select {
			case d := <-b.waits:
				if d != fast.limited {
					t.Errorf("waited %s, not %s", d, fast.limited)
				}
			case <-time.After(waitLimit):
				t.Fatal("no wait")
			}
			cloud.set(func(c *fakeCloud) { c.limited = 0 })
			b.waitAvailability(bedroom, "online")
		})
	}
}

func TestEveryUnitIsReadAgain(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	timings := fast
	timings.reload = 20 * time.Millisecond
	b := start(t, cloud, password, timings)
	b.waitAvailability(bedroom, "online")
	// A change the hub didn't pass on.
	cloud.set(func(c *fakeCloud) { c.states["ac-2"] = hexState(0x30, 0x44, 23, 0, 22) })
	b.waitState(study, `{"mode":"dry","on":true,"setpoint":23,"temperature":22}`)
}

func TestAnAccessTokenThatRunsOutLogsInAgainWithItsUnitsOnline(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	timings := fast
	timings.reload = 20 * time.Millisecond
	b := start(t, cloud, password, timings)
	b.waitAvailability(bedroom, "online")
	// Long enough that it isn't a session ending as it starts.
	time.Sleep(2 * fast.retryMax)

	// The access token runs out, as the next read finds.
	cloud.set(func(c *fakeCloud) { c.access = "another" })
	b.eventually("a second login", func() bool { return cloud.count() >= 2 })
	b.eventually("the second hub login", func() bool { return len(cloud.hubLogins()) >= 2 })
	b.waitAvailability(bedroom, "online")
	if h := b.history(root.Availability(bedroom)); slices.Contains(h, "offline") {
		t.Errorf("the bedroom went offline while the session was renewed: %v\n%s", h, b.logs)
	}
	if b.logs.count("Toshiba session ran out, so the plugin is logging in again") == 0 {
		t.Errorf("no session was renewed:\n%s", b.logs)
	}
}

func TestASASTokenNearItsEndStartsTheSessionAgainWithItsUnitsOnline(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	// se is whole seconds, so this one lasts one to two.
	cloud.set(func(c *fakeCloud) { c.lifetime = 2 * time.Second })
	b := start(t, cloud, password, fast)
	b.waitAvailability(bedroom, "online")
	b.eventually("a login once the hub's token is near its end", func() bool { return cloud.count() >= 2 })
	b.eventually("the second hub login", func() bool { return len(cloud.hubLogins()) >= 2 })
	b.waitAvailability(bedroom, "online")
	if h := b.history(root.Availability(bedroom)); slices.Contains(h, "offline") {
		t.Errorf("the bedroom went offline while the session was renewed: %v\n%s", h, b.logs)
	}
}

// set writes as the host does, on the set topic.
func (b *bench) set(id string, payload string) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	c, err := mqtt.Connect(ctx, b.broker, "toshiba-host")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = c.Disconnect(context.Background()) }()
	if err := c.Publish(ctx, root.Set(id), []byte(payload), false); err != nil {
		b.t.Fatal(err)
	}
}

func TestAWriteIsACommandTheHubTakes(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, password, fast)
	// The plugin reports a unit online before its state, so wait for both.
	b.waitAvailability(study, "online")
	b.waitState(study, `{"mode":"cool","on":false,"setpoint":24,"temperature":23}`)

	client := cloud.config().clientID()
	topic := "devices/" + client + "/messages/events/$.ct=application%2Fjson&$.ce=utf-8&type=mob"
	message := func(state string) string {
		return topic + " 1 {'sourceId': '" + client + "', 'messageId': '0000000', 'targetId': ['" + studyUnique +
			"'], 'cmd': 'CMD_FCU_TO_AC', 'payload': {'data': '" + state + "'}, 'timeStamp': '0000000'}"
	}
	on := newCommand()
	on.status = statusOn
	b.set(study, `{"on":true}`)
	b.eventually("the command sent", func() bool { return slices.Contains(cloud.messages(), message(on.encode())) })
	// The plugin reports what the unit says, not what it was told.
	if got := b.topic(root.State(study)); got != `{"mode":"cool","on":false,"setpoint":24,"temperature":23}` {
		t.Errorf("state after the write: %s", got)
	}
	cloud.method(methodName, cmdState, studyUnique, map[string]string{"data": partial(0x30, 0xff, -1)})
	b.waitState(study, `{"mode":"cool","on":true,"setpoint":24,"temperature":23}`)

	// Set returns once the hub has it.
	setpoint := newCommand()
	setpoint.setpoint = 26
	if err := b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: study, Capability: contractv1.Setpoint, Value: 26.0}); err != nil {
		t.Fatal(err)
	}
	if got := cloud.messages(); !slices.Contains(got, message(setpoint.encode())) {
		t.Errorf("sent %v", got)
	}
}

func TestAWriteTheUnitCantHoldFails(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	// The bedroom only heats.
	cloud.set(func(c *fakeCloud) { c.groups[0][0].Merit = "0100" })
	b := start(t, cloud, password, fast)
	b.waitAvailability(bedroom, "online")
	for _, cmd := range []struct {
		capability string
		value      any
		says       string
	}{
		{"mode", "cool", "could not set mode on " + bedroom + ": the unit doesn't do cool: it does heat"},
		{"setpoint", 21.5, "whole degrees"},
	} {
		err := b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: bedroom, Capability: cmd.capability, Value: cmd.value})
		if err == nil || !strings.Contains(err.Error(), cmd.says) {
			t.Errorf("%s %v: %v", cmd.capability, cmd.value, err)
		}
	}
	if err := b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: "nothing", Capability: contractv1.On, Value: false}); err == nil {
		t.Error("Set on no unit: no error")
	}
	if got := cloud.messages(); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
}

func TestAWriteWhileTheCloudIsDownFailsAtOnce(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, password, fast)
	b.waitAvailability(bedroom, "online")
	cloud.set(func(c *fakeCloud) { c.down, c.refuseHub = true, true })
	cloud.kick()
	b.waitAvailability(bedroom, "offline")

	began := time.Now()
	err := b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: bedroom, Capability: contractv1.On, Value: false})
	if err == nil || !strings.Contains(err.Error(), "can't reach Toshiba's cloud") {
		t.Errorf("Set: %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("Set took %s", took)
	}
	// Nor is it sent once the cloud is back.
	cloud.set(func(c *fakeCloud) { c.down, c.refuseHub = false, false })
	b.waitAvailability(bedroom, "online")
	if got := cloud.messages(); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
}

func TestNewChecksTheConfig(t *testing.T) {
	for _, c := range []Config{
		{Password: plugin.NewSecret("toshiba")},
		{Username: username},
		{Username: username, Password: plugin.NewSecret("toshiba"), OutdoorUnits: [][]string{{bedroom, study}, {study}}},
		{Username: username, Password: plugin.NewSecret("toshiba"), OutdoorUnits: [][]string{{bedroom, bedroom}}},
		{Username: username, Password: plugin.NewSecret("toshiba"), OutdoorUnits: [][]string{{bedroom}}},
		{Username: username, Password: plugin.NewSecret("toshiba"), OutdoorUnits: [][]string{{bedroom, ""}}},
		// The same unit, as the app writes it.
		{Username: username, Password: plugin.NewSecret("toshiba"), OutdoorUnits: [][]string{{bedroom, bedroomUnique}}},
	} {
		if _, err := New(c)(plugin.Environment{}, nil); err == nil {
			t.Errorf("%+v: no error", c)
		}
	}
	if _, err := New(Config{Username: username, Password: plugin.NewSecret("toshiba")})(plugin.Environment{}, nil); err != nil {
		t.Error(err)
	}
}

func TestAUnitWithNoUsableIDOrAnothersIsSkipped(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(func(c *fakeCloud) {
		c.groups = [][]unit{{
			{ID: "ac-1", UniqueID: bedroomUnique, Name: "Bedroom", State: hexState(0x30, 0x43, 21, 0, 19)},
			{ID: "ac-2", UniqueID: "::", Name: "Nameless"},
			{ID: "ac-3", UniqueID: strings.ToLower(bedroomUnique), Name: "Twin"},
		}}
	})
	b := start(t, cloud, password, fast)
	b.waitAvailability(bedroom, "online")
	if d := b.devices(); len(d.Devices) != 1 || len(d.Discovered) != 1 || d.Discovered[0].Name != "Bedroom" {
		t.Errorf("devices: %+v", d)
	}
	for _, msg := range []string{"Skipped Toshiba unit with no usable ID", "Skipped Toshiba unit with the same ID as another"} {
		if b.logs.count(msg) != 1 {
			t.Errorf("%q not logged once:\n%s", msg, b.logs)
		}
	}
}

func TestAReadThatFailsKeepsTheSession(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	timings := fast
	timings.reload = 20 * time.Millisecond
	b := start(t, cloud, password, timings)
	b.waitAvailability(bedroom, "online")
	// The study can't be read: the cloud answers 404.
	cloud.set(func(c *fakeCloud) { delete(c.states, "ac-2") })
	b.eventually("the failed read logged", func() bool { return b.logs.count("Could not read Toshiba units' state") > 0 })
	// The hub still carries changes, and the plugin didn't log in again.
	cloud.method(methodName, cmdState, bedroomUnique, map[string]string{"data": partial(0x31, 0xff, -1)})
	b.waitState(bedroom, `{"mode":"heat","on":false,"setpoint":21,"temperature":19}`)
	if n := cloud.count(); n != 1 {
		t.Errorf("logged in %d times", n)
	}
	if h := b.history(root.Availability(bedroom)); slices.Contains(h, "offline") {
		t.Errorf("the bedroom went offline: %v", h)
	}
}

func TestWritesSentTogetherSeeEachOther(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	// The bedroom is in 8 °C heating.
	cloud.set(func(c *fakeCloud) { c.groups[0][0].State = hexState(0x30, 0x43, 24, meritHeating8C, 9) })
	b := start(t, cloud, password, fast)
	b.waitState(bedroom, `{"mode":"heat","on":true,"setpoint":8,"temperature":9}`)

	// The Home app switches it to cooling at 24, before it has reported.
	for _, cmd := range []contractv1.SetCommand{
		{ID: bedroom, Capability: contractv1.Mode, Value: "cool"},
		{ID: bedroom, Capability: contractv1.Setpoint, Value: 24.0},
	} {
		if err := b.handler.Set(t.Context(), nil, cmd); err != nil {
			t.Fatal(err)
		}
	}
	cool, setpoint := newCommand(), newCommand()
	cool.mode, cool.meritA = 0x42, meritOff
	// Not 40: it's no longer heating.
	setpoint.setpoint = 24
	sent := cloud.messages()
	if len(sent) != 2 || !strings.Contains(sent[0], cool.encode()) || !strings.Contains(sent[1], setpoint.encode()) {
		t.Errorf("sent %v, want %s then %s", sent, cool.encode(), setpoint.encode())
	}

	// Its report says what it did, which the next write starts from: here, it stayed heating.
	cloud.method(methodName, cmdState, bedroomUnique, map[string]string{"data": hexState(0x30, 0x43, 24, meritHeating8C, 9)})
	b.eventually("the report taken", func() bool {
		b.handler.mu.Lock()
		defer b.handler.mu.Unlock()
		return b.handler.devices[bedroom].intent == nil
	})
	if err := b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: bedroom, Capability: contractv1.Setpoint, Value: 9.0}); err != nil {
		t.Fatal(err)
	}
	again := newCommand()
	again.setpoint = 25
	if sent := cloud.messages(); len(sent) != 3 || !strings.Contains(sent[2], again.encode()) {
		t.Errorf("sent %v, want %s last", sent, again.encode())
	}
}

func TestAModeThatClashesOnASharedOutdoorUnitIsRefused(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	// The bedroom heats and the study, off, last cooled. They share an outdoor unit, with one the
	// account doesn't have.
	// The bedroom's ID is as the app writes it.
	cloud.set(func(c *fakeCloud) { c.outdoor = [][]string{{bedroomUnique, study, "loft"}} })
	b := start(t, cloud, password, fast)
	b.waitAvailability(study, "online")
	if b.logs.count("Found no Toshiba unit for an ID in OutdoorUnits") != 1 {
		t.Errorf("the missing unit isn't logged: %s", b.logs)
	}
	set := func(id, capability string, value any) error {
		return b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: id, Capability: capability, Value: value})
	}

	// Turning the study on would cool while the bedroom heats.
	err := set(study, contractv1.On, true)
	if want := "could not set on on " + study + ": it shares its outdoor unit with " + bedroom +
		", which is running heat, and cool and heat can't run at once"; err == nil || err.Error() != want {
		t.Errorf("on: %v, want %s", err, want)
	}
	// What doesn't make a clash is sent: a setpoint, and a mode while it's off.
	for _, w := range []struct {
		capability string
		value      any
	}{{contractv1.Setpoint, 20.0}, {contractv1.Mode, "fan"}} {
		if err := set(study, w.capability, w.value); err != nil {
			t.Errorf("%s %v: %v", w.capability, w.value, err)
		}
	}
	// Fan runs beside heating.
	for _, on := range []bool{true, false} {
		if err := set(study, contractv1.On, on); err != nil {
			t.Errorf("on %v in fan while the bedroom heats: %v", on, err)
		}
	}
	// Auto may pick either.
	if err := set(study, contractv1.Mode, "auto"); err != nil {
		t.Fatal(err)
	}
	if err := set(study, contractv1.On, true); err == nil {
		t.Error("on in auto while the bedroom heats: no error")
	}
	// Heating with it is fine, and then the bedroom can't cool.
	for _, w := range []struct {
		id, capability string
		value          any
	}{{study, contractv1.Mode, "heat"}, {study, contractv1.On, true}} {
		if err := set(w.id, w.capability, w.value); err != nil {
			t.Errorf("%s %s %v: %v", w.id, w.capability, w.value, err)
		}
	}
	if err := set(bedroom, contractv1.Mode, "cool"); err == nil || !strings.Contains(err.Error(), "with "+study+", which is running heat") {
		t.Errorf("bedroom cooling while the study heats: %v", err)
	}
	// Once the study is off, it can.
	for _, w := range []struct {
		id, capability string
		value          any
	}{{study, contractv1.On, false}, {bedroom, contractv1.Mode, "cool"}} {
		if err := set(w.id, w.capability, w.value); err != nil {
			t.Errorf("%s %s %v: %v", w.id, w.capability, w.value, err)
		}
	}
	if got := len(cloud.messages()); got != 9 {
		t.Errorf("sent %d messages, want 9: %v", got, cloud.messages())
	}
}

func TestUnitsNotOnASharedOutdoorUnitDontClash(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	b := start(t, cloud, password, fast)
	b.waitAvailability(study, "online")
	if err := b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: study, Capability: contractv1.On, Value: true}); err != nil {
		t.Error(err)
	}
}

func TestAWriteTheUnitNeverReportsStopsCountingForItsOutdoorUnit(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	cloud.set(func(c *fakeCloud) {
		c.outdoor = [][]string{{bedroom, study}}
		// The bedroom is off.
		c.groups[0][0].State = hexState(0x31, 0x43, 21, 0, 19)
	})
	timings := fast
	timings.intent = 300 * time.Millisecond
	b := start(t, cloud, password, timings)
	b.waitAvailability(study, "online")
	set := func(id, capability string, value any) error {
		return b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: id, Capability: capability, Value: value})
	}
	// The bedroom is turned on, heating, and loses its WiFi before it reports.
	if err := set(bedroom, contractv1.On, true); err != nil {
		t.Fatal(err)
	}
	if err := set(study, contractv1.On, true); err == nil {
		t.Error("the study cooling while the bedroom's write counts: no error")
	}
	// Once it stops counting, its last report is what's checked.
	b.eventually("the bedroom's write to stop counting", func() bool { return set(study, contractv1.On, true) == nil })
}

func TestAUnitWhoseModeIsntKnownCantTurnOnBesideARunningOne(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(home)
	cloud.set(func(c *fakeCloud) {
		c.outdoor = [][]string{{bedroom, study}}
		// The study is listed with no state, and reading it fails.
		c.groups[1][0].State = ""
		delete(c.states, "ac-2")
	})
	b := start(t, cloud, password, fast)
	b.waitAvailability(study, "online")
	err := b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: study, Capability: contractv1.On, Value: true})
	if err == nil || !strings.Contains(err.Error(), "its mode isn't known yet") {
		t.Errorf("on: %v", err)
	}
	// Given a mode that doesn't clash, it can.
	for _, w := range []struct {
		capability string
		value      any
	}{{contractv1.Mode, "heat"}, {contractv1.On, true}} {
		if err := b.handler.Set(t.Context(), nil, contractv1.SetCommand{ID: study, Capability: w.capability, Value: w.value}); err != nil {
			t.Errorf("%s %v: %v", w.capability, w.value, err)
		}
	}
}
