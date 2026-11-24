package hue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"oikade.dev/contract/v1"
	"oikade.dev/internal/mdns"
	"oikade.dev/internal/mqtt"
	"oikade.dev/internal/mqtt/mqtttest"
	"oikade.dev/internal/testkit"
	"oikade.dev/sdk/plugin"
)

const (
	waitLimit = 20 * time.Second

	// The devices the fake bridge has, by their Zigbee MACs.
	hallLight  = "0x7cc6b6fffecb1814"
	deskLamp   = "0x001788010b2c3d4e"
	heaterPlug = "0x001788010aaabbcc"
	hallSensor = "0x001788010c112233"
)

var root = contract.NewRoot("hue")

// fast keeps the real timing's order, in less time.
var fast = timing{
	retry: 10 * time.Millisecond, retryMax: 40 * time.Millisecond, refused: 50 * time.Millisecond, limited: 50 * time.Millisecond,
	find: time.Second, request: 2 * time.Second, ping: 10 * time.Second, pong: 5 * time.Second,
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
	t      *testing.T
	bridge *fakeBridge
	logs   *logs
	env    plugin.Environment
	// waits are the backoffs the plugin waited.
	waits chan time.Duration

	mu     sync.Mutex
	topics map[string][]string
}

// options are how a bench differs from the usual: a home whose secret names bridgeOne, found over
// mDNS, with the fake bridge's authority.
type options struct {
	config   Config
	secret   string
	find     func(ctx context.Context, id string, within time.Duration) (string, error)
	declared []string
}

func keptSecret(bridge, key string) string {
	raw, _ := json.Marshal(kept{Bridge: bridge, Key: key})
	return string(raw)
}

// start runs the plugin on f.
func start(t *testing.T, f *fakeBridge, o options) *bench {
	t.Helper()
	broker := mqtttest.StartBroker(t)
	b := &bench{t: t, bridge: f, logs: &logs{}, topics: map[string][]string{}, waits: make(chan time.Duration, 100)}
	timings := fast
	timings.backoff = func(ctx context.Context, d time.Duration) {
		select {
		case b.waits <- d:
		default:
		}
		sleep(ctx, min(d, 50*time.Millisecond))
	}
	listening, stopListening := context.WithCancel(t.Context())
	t.Cleanup(stopListening)
	bus, err := mqtt.Connect(listening, broker, "hue-test")
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

	b.env = plugin.Environment{MQTTURL: broker, Root: root, SecretsDir: t.TempDir()}
	if o.secret == "" {
		o.secret = keptSecret(bridgeOne, appKey)
	}
	if o.secret != "none" {
		b.keep(o.secret)
	}
	if o.find == nil {
		o.find = func(_ context.Context, id string, _ time.Duration) (string, error) {
			if id != bridgeOne {
				return "", fmt.Errorf("%w: looked for %s", mdns.ErrNotFound, id)
			}
			return f.address(), nil
		}
	}
	if o.config.roots == nil {
		o.config.roots = f.ca.pool
	}
	devices := make([]contract.DeclaredDevice, 0, len(o.declared))
	for _, id := range o.declared {
		devices = append(devices, contract.DeclaredDevice{ID: id, Name: id, DisplayName: id})
	}
	handler := newBridge(o.config, b.env, devices, timings, o.find, slog.New(slog.NewJSONHandler(b.logs, nil)))
	ctx, cancel := context.WithCancel(t.Context())
	started, startCancel := context.WithTimeout(ctx, waitLimit)
	defer startCancel()
	p, err := plugin.Start(started, plugin.Options{Environment: b.env, OnSet: handler.Set, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := handler.Serve(ctx, p); err != nil {
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

// keep writes the plugin's secret, as setup would.
func (b *bench) keep(value string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.env.SecretsDir, "hue.hue-bridge"), []byte(value), 0o600); err != nil {
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

func (b *bench) devices() contract.Devices {
	b.t.Helper()
	var d contract.Devices
	if raw := b.topic(root.Devices()); raw != "" {
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			b.t.Fatal(err)
		}
	}
	return d
}

func (b *bench) status() contract.Status {
	b.t.Helper()
	var s contract.Status
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

// waitOnline waits for the plugin to have read the bridge and reported every device.
func (b *bench) waitOnline() {
	b.t.Helper()
	b.waitState(hallLight, `{"on":false}`)
	b.waitState(deskLamp, `{"brightness":100,"on":true}`)
	b.waitState(heaterPlug, `{"on":true}`)
	b.waitState(hallSensor, `{"battery":100,"occupied":false,"temperature":8.68}`)
}

func TestTheBridgesDevicesAreReportedByTheirZigbeeMAC(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{declared: []string{"0x00178801ffffffff"}})
	b.waitOnline()
	b.waitAvailability(hallLight, "online")
	b.waitAvailability(heaterPlug, "online")
	b.waitAvailability(hallSensor, "online")
	// Its zigbee_connectivity says connectivity_issue.
	b.waitAvailability(deskLamp, "offline")

	signify := "Signify Netherlands B.V."
	want := []contract.DeviceReport{
		{ID: heaterPlug, Found: true, Manufacturer: signify, Model: "LOM008", FirmwareVersion: "1.122.2",
			Capabilities: []string{"on"}, DeviceType: contract.OnOffPlugInUnit},
		{ID: deskLamp, Found: true, Manufacturer: signify, Model: "LCA006", FirmwareVersion: "1.163.1",
			Capabilities: []string{"brightness", "on"}, DeviceType: contract.DimmableLight},
		{ID: hallSensor, Found: true, Manufacturer: signify, Model: "SML004", FirmwareVersion: "3.2.1",
			Capabilities: []string{"battery", "occupied", "temperature"}},
		{ID: hallLight, Found: true, Manufacturer: "SONOFF", Model: "ZBMINIR2", FirmwareVersion: "1.0.4",
			Capabilities: []string{"on"}, DeviceType: contract.OnOffLight},
		// On record and not on the bridge: not found, and kept.
		{ID: "0x00178801ffffffff"},
	}
	got := b.devices()
	if !reflect.DeepEqual(got.Devices, want) {
		t.Errorf("devices:\n got %+v\nwant %+v", got.Devices, want)
	}
	names := map[string][2]string{}
	for _, d := range got.Discovered {
		names[d.ID] = [2]string{d.Name, d.Room}
	}
	wantNames := map[string][2]string{
		deskLamp: {"Desk lamp", ""}, heaterPlug: {"Heater plug", ""}, hallSensor: {"Hall sensor", "Hall"}, hallLight: {"Hall light", "Hall"},
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("discovered names and rooms %v, want %v", names, wantNames)
	}
	if s := b.status(); s.State != contract.Online || s.Error != "" {
		t.Errorf("status: %+v", s)
	}
	// The dimmer switch is skipped, and named once. The bridge itself isn't worth a line.
	if n := b.logs.count("Skipped Hue device"); n != 1 || !strings.Contains(b.logs.String(), `"model":"ROM001"`) {
		t.Errorf("logged %d skipped devices, want the ROM001 once: %s", n, b.logs)
	}
}

func TestAMotionEventLandsAsOccupied(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	f.send("update", `{"id":"bf215aa3-7c36-46c6-bbd9-7e3eda524b86","id_v1":"/sensors/13","owner":{"rid":"05ccb499-ef88-470e-8800-6daef2c4deea","rtype":"device"},`+
		`"motion":{"motion":true,"motion_report":{"changed":"2026-11-24T10:07:52.447Z","motion":true},"motion_valid":true},"type":"motion"}`)
	b.waitState(hallSensor, `{"battery":100,"occupied":true,"temperature":8.68}`)
	f.send("update", `{"id":"863f199f-2725-4c4b-b0ab-ee59eedcdfe2","owner":{"rid":"05ccb499-ef88-470e-8800-6daef2c4deea","rtype":"device"},`+
		`"temperature":{"temperature":23.85,"temperature_report":{"changed":"2026-11-24T10:07:52.447Z","temperature":23.85},"temperature_valid":true},"type":"temperature"}`)
	b.waitState(hallSensor, `{"battery":100,"occupied":true,"temperature":23.85}`)
}

func TestAnEventChangesOnlyWhatItCarries(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	f.send("update", `{"id":"027ae662-aaac-4595-acc6-03bb15e293cf","type":"light","dimming":{"brightness":49.8}}`)
	b.waitState(deskLamp, `{"brightness":49.8,"on":true}`)
	f.send("update", `{"id":"027ae662-aaac-4595-acc6-03bb15e293cf","type":"light","on":{"on":false}}`)
	b.waitState(deskLamp, `{"brightness":49.8,"on":false}`)
	// A change to a resource it doesn't read, and one it can't, change nothing.
	f.send("update", `{"id":"cc4a9369-511a-4ba5-b895-0d2d9465fe62","type":"light_level","light":{"light_level":1}}`,
		`{"id":"027ae662-aaac-4595-acc6-03bb15e293cf","type":"light","on":{"on":"yes"}}`)
	f.send("update", `{"id":"d3afe652-b85c-4d64-ad5d-f8fd5469b415","type":"light","on":{"on":true}}`)
	b.waitState(hallLight, `{"on":true}`)
	if got := b.topic(root.State(deskLamp)); got != `{"brightness":49.8,"on":false}` {
		t.Errorf("desk lamp %s, want it unchanged", got)
	}
	if n := b.logs.count("Ignored Hue event"); n != 1 {
		t.Errorf("logged %d ignored events, want 1: %s", n, b.logs)
	}
	if reads, _ := f.counts(); reads != 1 {
		t.Errorf("read the bridge %d times, want once", reads)
	}
}

func TestADeviceThatLosesZigbeeIsOffline(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	b.waitAvailability(hallLight, "online")
	f.send("update", `{"id":"5cf5b597-313e-48e7-9136-f7d49d4e0971","type":"zigbee_connectivity","status":"connectivity_issue"}`)
	b.waitAvailability(hallLight, "offline")
	f.send("update", `{"id":"5cf5b597-313e-48e7-9136-f7d49d4e0971","type":"zigbee_connectivity","status":"connected"}`)
	b.waitAvailability(hallLight, "online")
	b.waitAvailability(deskLamp, "offline")
}

func TestAStreamThatEndsIsOpenedAgainAndTheBridgeReadAgain(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	b.waitAvailability(hallLight, "online")
	f.waitOpen()
	// The session lasts beyond retryMax, so it starts again at once.
	time.Sleep(2 * fast.retryMax)
	f.endStream()
	b.eventually("the bridge read again", func() bool { reads, streams := f.counts(); return reads == 2 && streams == 2 })
	f.send("update", `{"id":"d3afe652-b85c-4d64-ad5d-f8fd5469b415","type":"light","on":{"on":true}}`)
	b.waitState(hallLight, `{"on":true}`)
	if h := b.history(root.Availability(hallLight)); slices.Contains(h, "offline") {
		t.Errorf("hall light was %v, want it online throughout", h)
	}
	if n := b.logs.count("Hue bridge's event stream ended, so the plugin is reading the bridge again"); n != 1 {
		t.Errorf("logged the stream ending %d times, want once: %s", n, b.logs)
	}
}

func TestADeviceAddedReadsTheBridgeAgain(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	device := `{"id":"77777777-6666-4f4f-8a8a-020202020201","type":"device","metadata":{"name":"Porch light"},` +
		`"product_data":{"model_id":"ZBMINIR2","manufacturer_name":"SONOFF","product_archetype":"classic_bulb","software_version":"1.0.4"},` +
		`"services":[{"rid":"77777777-6666-4f4f-8a8a-020202020202","rtype":"zigbee_connectivity"},{"rid":"77777777-6666-4f4f-8a8a-020202020203","rtype":"light"}]}`
	connectivity := `{"id":"77777777-6666-4f4f-8a8a-020202020202","type":"zigbee_connectivity","status":"connected","mac_address":"7c:c6:b6:ff:fe:cb:20:20"}`
	light := `{"id":"77777777-6666-4f4f-8a8a-020202020203","type":"light","on":{"on":true}}`
	for _, r := range []string{device, connectivity, light} {
		f.add(r)
	}
	f.send("add", device, connectivity, light)
	b.waitState("0x7cc6b6fffecb2020", `{"on":true}`)
	b.waitAvailability("0x7cc6b6fffecb2020", "online")
	if reads, streams := f.counts(); reads != 2 || streams != 1 {
		t.Errorf("read %d times on %d streams, want twice on one", reads, streams)
	}
}

func TestABridgeThatGoesAwayMakesEveryDeviceOffline(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	b.waitAvailability(hallLight, "online")
	f.setRefuse(http.StatusServiceUnavailable)
	f.endStream()
	b.waitAvailability(hallLight, "offline")
	b.waitAvailability(hallSensor, "offline")
	b.waitProblem("could not reach the Hue bridge " + bridgeOne + ": the bridge answered 503 Service Unavailable")
	if d := b.devices(); d.Discovered != nil {
		t.Errorf("discovered %v while the bridge is away, want nil", d.Discovered)
	}
	f.setRefuse(0)
	b.waitAvailability(hallLight, "online")
	b.eventually("the problem gone", func() bool { return b.status().Error == "" })
}

func TestACertificateForAnotherBridgeIsRefused(t *testing.T) {
	f := newFakeBridge(t, bridgeTwo)
	b := start(t, f, options{})
	b.waitProblem("the Hue bridge " + bridgeOne + " has a certificate that is for the bridge " + bridgeTwo + ", not " + bridgeOne)
	if reads, streams := f.counts(); reads != 0 || streams != 0 {
		t.Errorf("read %d times and streamed %d, want neither: the key mustn't go to another bridge", reads, streams)
	}
}

func TestACertificateFromAnotherAuthorityIsRefused(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{config: Config{Address: f.address(), roots: newAuthority(t).pool}})
	b.waitProblem("the Hue bridge at " + f.address() + " has a certificate that for " + bridgeOne + " isn't signed by Philips Hue: check the Address")
	if reads, streams := f.counts(); reads != 0 || streams != 0 {
		t.Errorf("read %d times and streamed %d, want neither", reads, streams)
	}
}

func TestARefusedKeySaysSo(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{secret: keptSecret(bridgeOne, "removed-in-the-app")})
	b.waitProblem("the Hue bridge " + bridgeOne + " refused the plugin's key, which may have been removed in the Hue app (the bridge answered 403 Forbidden)")
	b.eventually("a wait of refused", func() bool {
		select {
		case d := <-b.waits:
			return d == fast.refused
		default:
			return false
		}
	})
	// A new key, kept, is read on the next try.
	b.keep(keptSecret(bridgeOne, appKey))
	b.waitOnline()
}

func TestNoKeySaysSo(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{secret: "none", declared: []string{hallLight}})
	b.waitProblem("has no key for the Hue bridge yet")
	if d := b.devices(); len(d.Devices) != 1 || d.Devices[0].Found {
		t.Errorf("devices %+v, want the hall light on record, not found", d.Devices)
	}
	b.keep(keptSecret(bridgeOne, appKey))
	b.waitOnline()
}

func TestASecretThatIsntOneSaysSo(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{secret: `{"bridge":"not-a-bridge","key":"k"}`})
	b.waitProblem("could not read the Hue bridge's key: the host's secret hue.hue-bridge isn't a Hue bridge's ID and key")
}

func TestABridgeMDNSCantFindSaysSo(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{secret: keptSecret(bridgeTwo, appKey)})
	b.waitProblem("could not find the Hue bridge " + bridgeTwo + " on the network over mDNS: check it's on, or set hue.Config's Address")
}

func TestAnAddressSkipsMDNS(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{
		config: Config{Address: f.address()},
		find: func(context.Context, string, time.Duration) (string, error) {
			return "", errors.New("mDNS mustn't be asked")
		},
	})
	b.waitOnline()
}
