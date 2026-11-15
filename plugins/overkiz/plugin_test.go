package overkiz

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

const (
	waitLimit = 20 * time.Second
	token     = "developer-mode-token"
	password  = "correct horse"
	kitchen   = "io.11111111"
	patio     = "io.22222222"
	kitchenAt = "io://1234-5678-9012/11111111"
)

var root = contractv1.NewRoot("tahoma")

// fast keeps the real timing's order, in less time.
var fast = timing{
	poll: 20 * time.Millisecond, busy: 5 * time.Millisecond, retry: 10 * time.Millisecond, retryMax: 40 * time.Millisecond,
	limited: 50 * time.Millisecond, refused: time.Hour, secret: 10 * time.Millisecond,
	request: 2 * time.Second, set: 2 * time.Second, execution: time.Minute,
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
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), `"msg":"`+msg+`"`)
}

// bench is the plugin under a broker, with its topics as last published.
type bench struct {
	t       *testing.T
	hub     *fakeHub
	secrets string
	logs    *logs
	broker  string
	// secretName is the account's secret.
	secretName string

	mu     sync.Mutex
	topics map[string]string

	plugin *plugin.Plugin
}

func start(t *testing.T, hub *fakeHub, secret string, declared ...string) *bench {
	t.Helper()
	return startWith(t, hub, Local{Address: hub.address(), TokenSecret: "tahoma-token", PIN: hubPIN, roots: hub.ca}, fast, secret, declared...)
}

// cloudFast is fast, with the cloud's limit waits.
var cloudFast = func() timing { t := fast; t.retryMax = 50 * time.Millisecond; return t }()

// startCloud starts the plugin on the fake Cozytouch cloud, as Thermor's.
func startCloud(t *testing.T, cloud *fakeHub, secret string, timings timing, declared ...string) *bench {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: cloud.roots()}
	account := Cozytouch{
		Brand: Thermor, Username: "someone@example.com", PasswordSecret: "cozytouch",
		atlanticAPI: cloud.server.URL, endpoint: cloud.server.URL + cloudAPI, transport: transport,
	}
	return startWith(t, cloud, account, timings, secret, declared...)
}

func startWith(t *testing.T, hub *fakeHub, account Account, timings timing, secret string, declared ...string) *bench {
	t.Helper()
	broker := mqtttest.StartBroker(t)
	b := &bench{
		t: t, hub: hub, secrets: t.TempDir(), logs: &logs{}, broker: broker, topics: map[string]string{},
		secretName: account.secret(),
	}
	if secret != "" {
		b.secret(secret)
	}
	listening, stopListening := context.WithCancel(context.Background())
	t.Cleanup(stopListening)
	bus, err := mqtt.Connect(listening, broker, "overkiz-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe(listening, root.Filter(), func(m mqtt.Message) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.topics[m.Topic] = string(m.Payload)
	}); err != nil {
		t.Fatal(err)
	}

	env := plugin.Environment{MQTTURL: broker, Root: root, SecretsDir: b.secrets}
	devices := make([]contractv1.DeclaredDevice, 0, len(declared))
	for _, id := range declared {
		devices = append(devices, contractv1.DeclaredDevice{ID: id, Name: id, DisplayName: id})
	}
	handler := newBridge(account, env, devices, timings, slog.New(slog.NewJSONHandler(b.logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	started, startCancel := context.WithTimeout(ctx, waitLimit)
	defer startCancel()
	b.plugin, err = plugin.Start(started, plugin.Options{Environment: env, OnSet: handler.Set, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := handler.Serve(ctx, b.plugin); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = b.plugin.Close(context.Background())
	})
	return b
}

func (b *bench) secret(value string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.secrets, b.secretName), []byte(value+"\n"), 0o600); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) topic(name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.topics[name]
}

func (b *bench) eventually(what string, ok func() bool) {
	b.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.t.Fatalf("Never saw %s. Topics: %v", what, b.topics)
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

// listening waits for the plugin to fetch events, so an event the hub queues from then on is heard.
func (b *bench) listening() {
	b.t.Helper()
	fetched := b.hub.count("/events/*/fetch")
	b.eventually("a fetch of events", func() bool { return b.hub.count("/events/*/fetch") > fetched })
}

// set writes as the host does, on the set topic.
func (b *bench) set(id string, payload string) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	c, err := mqtt.Connect(ctx, b.broker, "overkiz-host")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = c.Disconnect(context.Background()) }()
	if err := c.Publish(ctx, root.Set(id), []byte(payload), false); err != nil {
		b.t.Fatal(err)
	}
}

func TestTheHubsShuttersAreReportedByAStableID(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, token, "io.99999999")
	b.waitState(kitchen, `{"position":0}`)
	b.waitState(patio, `{"position":40}`)
	b.waitAvailability(kitchen, "online")

	got := b.devices()
	want := []contractv1.DeviceReport{
		{ID: kitchen, Found: true, Manufacturer: "Somfy", Model: "io:RollerShutterGenericIOComponent", Capabilities: []string{"position"}},
		{ID: patio, Found: true, Model: "io:VerticalExteriorAwningIOComponent", Capabilities: []string{"position"}},
		// On record and not on the hub: not found, and kept.
		{ID: "io.99999999"},
	}
	if !reflect.DeepEqual(got.Devices, want) {
		t.Errorf("devices:\n got %+v\nwant %+v", got.Devices, want)
	}
	// The sub-device folds into its screen, and the pod and the RTS blind aren't served.
	if len(got.Discovered) != 2 || got.Discovered[0].Name != "Kitchen shutter" || got.Discovered[1].Name != "Patio screen" {
		t.Errorf("discovered: %+v", got.Discovered)
	}
	if s := b.status(); s.State != contractv1.Online || s.Error != "" {
		t.Errorf("status: %+v", s)
	}
	for _, widget := range []string{"Pod", "UpDownRollerShutter"} {
		if !strings.Contains(b.logs.String(), `"widget":"`+widget+`"`) {
			t.Errorf("%s not logged as skipped:\n%s", widget, b.logs.String())
		}
	}
	if strings.Contains(b.logs.String(), "LightSensor") {
		t.Errorf("a sub-device was logged as a kind of its own:\n%s", b.logs.String())
	}
}

func TestASetIsAnExecutionAndReportsFollowTheShutter(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, token)
	b.waitState(kitchen, `{"position":0}`)
	b.listening()

	b.set(kitchen, `{"position":75}`)
	b.eventually("an execution", func() bool { return len(hub.executions()) == 1 })
	want := map[string]any{"label": "Oikade", "actions": []any{map[string]any{
		"deviceURL": kitchenAt, "commands": []any{map[string]any{"name": "setClosure", "parameters": []any{float64(75)}}},
	}}}
	if got := hub.executions()[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("executed %v, want %v", got, want)
	}
	// The plugin reports what the shutter says on the way, not what it was told.
	if got := b.topic(root.State(kitchen)); got != `{"position":0}` {
		t.Errorf("state after the write: %s", got)
	}
	hub.closure(kitchenAt, 30)
	b.waitState(kitchen, `{"position":30}`)
	hub.event(map[string]any{"name": eventExecutionChanged, "execId": "exec-1", "newState": execCompleted})
	hub.closure(kitchenAt, 75)
	b.waitState(kitchen, `{"position":75}`)
}

func TestTheHubGoingAwayLeavesEveryDeviceOfflineUntilItsBack(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, token)
	b.waitAvailability(kitchen, "online")

	hub.set(func(h *fakeHub) { h.down = true })
	b.waitAvailability(kitchen, "offline")
	b.waitAvailability(patio, "offline")
	b.eventually("the plugin's problem", func() bool { return strings.Contains(b.status().Error, "could not reach") })
	if s := b.status(); s.State != contractv1.Online {
		t.Errorf("the plugin went %s with its hub", s.State)
	}
	// Its devices stay on record, and it says it can't see the hub's list rather than that it's empty.
	if d := b.devices(); len(d.Devices) != 2 || !d.Devices[0].Found || d.Discovered != nil {
		t.Errorf("devices while the hub is down: %+v", d)
	}
	b.set(patio, `{"position":10}`)

	hub.set(func(h *fakeHub) { h.down = false })
	b.waitAvailability(kitchen, "online")
	b.eventually("the problem to clear", func() bool { return b.status().Error == "" })
	if n := len(hub.executions()); n != 0 {
		t.Errorf("a write while the hub was down was sent later: %d executions", n)
	}
}

func TestARefusedTokenWaitsForANewOne(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, "not-the-token", kitchen)
	b.eventually("the refusal", func() bool { return strings.Contains(b.status().Error, `check the secret "tahoma-token"`) })
	if d := b.devices(); len(d.Devices) != 1 || d.Devices[0].Found {
		t.Errorf("devices before a login: %+v", d)
	}
	// It doesn't try the same token again, which could lock the account.
	tries := hub.count("/setup/devices")
	time.Sleep(10 * fast.secret)
	if n := hub.count("/setup/devices"); n != tries {
		t.Errorf("tried a refused token %d more times", n-tries)
	}

	b.secret(token)
	b.waitState(kitchen, `{"position":0}`)
	b.eventually("the problem to clear", func() bool { return b.status().Error == "" })
}

func TestNoTokenSaysHowToAddOne(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, "")
	b.eventually("the missing secret", func() bool {
		return b.status().Error == "no credential: oikade secret set tahoma-token adds it"
	})
	if n := hub.count("/setup/devices"); n != 0 {
		t.Errorf("asked the hub %d times with no token", n)
	}
	b.secret(token)
	b.waitState(kitchen, `{"position":0}`)
}

func TestAForgottenListenerIsRegisteredAgain(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, token)
	b.waitState(kitchen, `{"position":0}`)
	b.listening()
	reads := hub.count("/setup/devices")

	hub.forget()
	// A change made while no listener heard it is read with the setup.
	hub.set(func(h *fakeHub) { h.setup[1]["states"].([]any)[1].(map[string]any)["value"] = 55 })
	b.waitState(kitchen, `{"position":55}`)
	if hub.count("/events/register") < 2 || hub.count("/setup/devices") <= reads {
		t.Errorf("registered %d times and read the setup %d times", hub.count("/events/register"), hub.count("/setup/devices"))
	}
	hub.closure(kitchenAt, 60)
	b.waitState(kitchen, `{"position":60}`)
}

func TestTooManyRequestsBacksOff(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, token)
	b.waitAvailability(kitchen, "online")
	hub.set(func(h *fakeHub) { h.limited = true })
	b.eventually("the limit", func() bool { return strings.Contains(b.status().Error, "too many requests") })
	b.waitAvailability(kitchen, "offline")
	hub.set(func(h *fakeHub) { h.limited = false })
	b.waitAvailability(kitchen, "online")
}

func TestAnAddedDeviceIsReadWithTheSetup(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, token)
	b.waitState(kitchen, `{"position":0}`)
	b.listening()
	hub.set(func(h *fakeHub) {
		h.setup = append(h.setup, map[string]any{
			"deviceURL": "io://1234-5678-9012/33333333", "label": "Office shutter", "widget": "PositionableRollerShutter",
			"controllableName": "io:RollerShutterGenericIOComponent", "available": true, "enabled": true,
			"states": []any{map[string]any{"name": "core:ClosureState", "type": 1, "value": 100}},
		})
	})
	hub.event(map[string]any{"name": eventCreated, "deviceURL": "io://1234-5678-9012/33333333"})
	b.waitState("io.33333333", `{"position":100}`)
	if n := b.logs.count("Skipped Overkiz device of a kind the plugin doesn't serve"); n != 2 {
		t.Errorf("logged %d skipped kinds after reading the setup again, want the 2 from the first", n)
	}

	hub.event(map[string]any{"name": eventUnavailable, "deviceURL": kitchenAt})
	b.waitAvailability(kitchen, "offline")
	hub.event(map[string]any{"name": eventAvailable, "deviceURL": kitchenAt})
	b.waitAvailability(kitchen, "online")
}

func TestAShutterThatCantSayWhereItIsLeavesTheRestWorking(t *testing.T) {
	hub := newFakeHub(t)
	// Uncalibrated: no closure the plugin can read.
	hub.set(func(h *fakeHub) {
		h.setup[1]["states"] = []any{map[string]any{"name": "core:ClosureState", "type": 1, "value": -1}}
	})
	b := start(t, hub, token)
	b.waitState(patio, `{"position":40}`)
	b.waitAvailability(kitchen, "online")
	if got := b.topic(root.State(kitchen)); got != `{}` {
		t.Errorf("state of a shutter with no position: %s", got)
	}
	if s := b.status(); s.Error != "" {
		t.Errorf("status: %+v", s)
	}
	b.listening()
	hub.closure(kitchenAt, 20)
	b.waitState(kitchen, `{"position":20}`)
}

func TestADeviceListedAsNumberOneKeepsItsSubDevices(t *testing.T) {
	hub := newFakeHub(t)
	hub.set(func(h *fakeHub) { h.setup[3]["deviceURL"] = "io://1234-5678-9012/22222222#1" })
	b := start(t, hub, token)
	b.waitState(patio, `{"position":40}`)
	b.listening()
	// Its events name it with the #1.
	hub.closure("io://1234-5678-9012/22222222#1", 70)
	b.waitState(patio, `{"position":70}`)
	if strings.Contains(b.logs.String(), "LightSensor") {
		t.Errorf("its sub-device was logged as a kind of its own:\n%s", b.logs.String())
	}
}

func TestAFailedExecutionReadsTheDeviceAgain(t *testing.T) {
	hub := newFakeHub(t)
	b := start(t, hub, token)
	b.waitState(kitchen, `{"position":0}`)
	b.listening()
	reads := hub.count("/setup/devices")
	b.set(kitchen, `{"position":100}`)
	b.eventually("an execution", func() bool { return len(hub.executions()) == 1 })
	// It stopped part way, and the hub never said so in an event.
	hub.set(func(h *fakeHub) { h.setup[1]["states"].([]any)[1].(map[string]any)["value"] = 35 })
	hub.event(map[string]any{"name": eventExecutionChanged, "execId": "exec-1", "newState": execFailed, "failureType": "WHILEEXEC_BLOCKED_BY_HAZARD"})
	b.waitState(kitchen, `{"position":35}`)
	if hub.count("/setup/devices") <= reads {
		t.Error("the setup wasn't read again")
	}
	// A value it can't send is refused, and nothing is sent.
	b.set(kitchen, `{"position":true}`)
	time.Sleep(20 * fast.poll)
	if n := len(hub.executions()); n != 1 {
		t.Errorf("%d executions after a write of true, want 1", n)
	}
}

const (
	radiator   = "io.9876543"
	radiatorAt = "io://0812-1234-5678/9876543#1"
)

func TestAThermorAccountLogsInAndServesItsRadiator(t *testing.T) {
	cloud := newFakeCloud(t)
	b := startCloud(t, cloud, password, cloudFast)
	b.waitState(radiator, `{"on":true,"setpoint":21,"temperature":18.5}`)
	b.waitAvailability(radiator, "online")
	want := []contractv1.DeviceReport{{
		ID: radiator, Found: true, Manufacturer: "Thermor", Model: "io:AtlanticElectricalTowelDryerIOComponent",
		Capabilities: []string{"on", "setpoint", "temperature"},
	}}
	if got := b.devices().Devices; !reflect.DeepEqual(got, want) {
		t.Errorf("devices:\n got %+v\nwant %+v", got, want)
	}
	// The consumption sensor at #2 folds in, and isn't a kind of its own.
	if strings.Contains(b.logs.String(), "CumulativeElectricPowerConsumptionSensor") {
		t.Errorf("a sub-device was logged as a kind of its own:\n%s", b.logs.String())
	}

	b.listening()
	b.set(radiator, `{"setpoint":19.5}`)
	b.eventually("an execution", func() bool { return len(cloud.executions()) == 1 })
	if got := fmt.Sprint(cloud.executions()[0]["actions"]); !strings.Contains(got, "setTargetTemperature") ||
		!strings.Contains(got, radiatorAt) || !strings.Contains(got, "19.5") {
		t.Errorf("executed %s", got)
	}
	// In its own schedule, the setpoint is the one it aims for, and a write is an override.
	cloud.event(map[string]any{"name": eventStateChanged, "deviceURL": radiatorAt, "deviceStates": []map[string]any{
		{"name": "core:OperatingModeState", "type": 3, "value": "internal"},
	}})
	b.waitState(radiator, `{"on":true,"setpoint":19,"temperature":18.5}`)
	b.set(radiator, `{"setpoint":22}`)
	b.eventually("an override", func() bool {
		e := cloud.executions()
		return len(e) == 2 && strings.Contains(fmt.Sprint(e[1]["actions"]), "setDerogatedTargetTemperature")
	})
	b.set(radiator, `{"on":false}`)
	b.eventually("standby", func() bool {
		e := cloud.executions()
		return len(e) == 3 && strings.Contains(fmt.Sprint(e[2]["actions"]), "setTowelDryerOperatingMode parameters:[standby]")
	})
	// The temperature is the sensor's, at #7.
	cloud.event(map[string]any{"name": eventStateChanged, "deviceURL": "io://0812-1234-5678/9876543#7", "deviceStates": []map[string]any{
		{"name": "core:TemperatureState", "type": 2, "value": 20.25},
	}})
	b.waitState(radiator, `{"on":true,"setpoint":19,"temperature":20.25}`)
}

func TestARefusedPasswordIsTheAccountsProblemUntilItChanges(t *testing.T) {
	cloud := newFakeCloud(t)
	b := startCloud(t, cloud, "wrong", cloudFast, radiator)
	b.eventually("the refusal", func() bool {
		e := b.status().Error
		return strings.HasPrefix(e, "Thermor's Cozytouch cloud refused the login (") && strings.Contains(e, "Bad credentials") &&
			strings.HasSuffix(e, `: check the secret "cozytouch"`)
	})
	if d := b.devices(); len(d.Devices) != 1 || d.Devices[0].Found {
		t.Errorf("devices with a refused password: %+v", d)
	}
	tries := cloud.count("/token")
	time.Sleep(10 * cloudFast.secret)
	if n := cloud.count("/token"); n != tries {
		t.Errorf("tried a refused password %d more times, which could lock the account", n-tries)
	}
	b.secret(password)
	b.waitAvailability(radiator, "online")
	b.eventually("the problem to clear", func() bool { return b.status().Error == "" })
}

func TestTheCloudDownLeavesTheRadiatorOfflineAndSaysWhy(t *testing.T) {
	cloud := newFakeCloud(t)
	b := startCloud(t, cloud, password, cloudFast)
	b.waitAvailability(radiator, "online")
	cloud.set(func(h *fakeHub) { h.down = true })
	b.waitAvailability(radiator, "offline")
	b.eventually("the problem", func() bool { return strings.Contains(b.status().Error, "could not reach Thermor's Cozytouch cloud") })
	// A write while it's down fails at once, without asking the cloud.
	b.set(radiator, `{"on":false}`)
	time.Sleep(10 * cloudFast.poll)
	if n := len(cloud.executions()); n != 0 {
		t.Errorf("%d executions while the cloud was down", n)
	}
	cloud.set(func(h *fakeHub) { h.down = false })
	b.waitAvailability(radiator, "online")
	b.eventually("the problem to clear", func() bool { return b.status().Error == "" })
}

func TestAnEndedSessionLogsInAgainWithTheRadiatorOnline(t *testing.T) {
	cloud := newFakeCloud(t)
	// Sessions last longer than retryMax here, so an ended one logs in again at once.
	timings := cloudFast
	timings.retryMax = 0
	b := startCloud(t, cloud, password, timings)
	b.waitAvailability(radiator, "online")
	b.listening()
	logins := cloud.count("/login")
	cloud.expire()
	b.eventually("a new login", func() bool { return cloud.count("/login") > logins })
	b.listening()
	if got := b.topic(root.Availability(radiator)); got != "online" {
		t.Errorf("availability across a new login: %s", got)
	}
	if s := b.status(); s.Error != "" {
		t.Errorf("status across a new login: %+v", s)
	}
}

func TestASessionEndedAsItBeginsBacksOff(t *testing.T) {
	cloud := newFakeCloud(t)
	b := startCloud(t, cloud, password, cloudFast)
	b.waitAvailability(radiator, "online")
	b.listening()
	// Every session ends at once from now on.
	cloud.set(func(h *fakeHub) { h.sessions = map[string]bool{}; h.password = password })
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				cloud.expire()
			}
		}
	}()
	b.eventually("the problem", func() bool { return strings.Contains(b.status().Error, "keeps ending the plugin's session") })
	close(stop)
	logins := cloud.count("/login")
	time.Sleep(cloudFast.retryMax)
	if n := cloud.count("/login") - logins; n > 2 {
		t.Errorf("logged in %d times in %s", n, cloudFast.retryMax)
	}
	b.waitAvailability(radiator, "online")
}

func TestCozytouchNeedsABrandAndAUsername(t *testing.T) {
	for _, c := range []Cozytouch{
		{Username: "someone@example.com", PasswordSecret: "cozytouch"},
		{Brand: "Atlantique", Username: "someone@example.com", PasswordSecret: "cozytouch"},
		{Brand: Thermor, PasswordSecret: "cozytouch"},
		{Brand: Thermor, Username: "someone@example.com"},
	} {
		if _, err := New(c)(plugin.Environment{}, nil); err == nil {
			t.Errorf("%+v was taken", c)
		}
	}
	if _, err := New(Local{TokenSecret: "tahoma-token"})(plugin.Environment{}, nil); err == nil {
		t.Error("a Local with no Address was taken")
	}
}

func TestAnotherRefusalFromAtlanticIsntTriedAgainSoon(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.set(func(h *fakeHub) { h.clientRefused = true })
	b := startCloud(t, cloud, password, cloudFast)
	b.eventually("the refusal", func() bool { return strings.Contains(b.status().Error, "Atlantic refused the login: invalid_client") })
	tries := cloud.count("/token")
	time.Sleep(10 * cloudFast.secret)
	if n := cloud.count("/token"); n != tries {
		t.Errorf("tried %d more times", n-tries)
	}
}
