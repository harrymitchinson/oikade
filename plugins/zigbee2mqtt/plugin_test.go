package zigbee2mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

const (
	waitLimit  = 20 * time.Second
	plugIEEE   = "0x00124b0000000004"
	lampIEEE   = "0x000b57fffe000001"
	motionIEEE = "0x00158d0000000002"
	doorIEEE   = "0x00158d0000000003"
	base       = "z2m"
)

var root = contractv1.NewRoot("zigbee")

// heard follows what the plugin publishes, as the host would.
type heard struct {
	mu     sync.Mutex
	topics map[string]string
	bus    *mqtt.Client
}

func hear(t *testing.T, url string) *heard {
	t.Helper()
	// The connection lasts as long as this context.
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	bus, err := mqtt.Connect(ctx, url, "z2m-test-"+clientID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Disconnect(context.Background()) }) //nolint:usetesting // t.Context() is done once cleanups run
	h := &heard{topics: map[string]string{}, bus: bus}
	record := func(m mqtt.Message) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.topics[m.Topic] = string(m.Payload)
	}
	for _, filter := range []string{root.Filter(), base + "/+/set"} {
		if _, err := bus.Subscribe(ctx, filter, record); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *heard) publish(t *testing.T, topic, payload string, retain bool) {
	t.Helper()
	if err := h.bus.Publish(t.Context(), topic, []byte(payload), retain); err != nil {
		t.Fatal(err)
	}
}

func (h *heard) wait(t *testing.T, what string, ok func(topics map[string]string) bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		// ok may fail the test, so it doesn't run under the lock.
		if ok(h.copy()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s in %v", what, h.copy())
}

// copy is what's been heard so far.
func (h *heard) copy() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.topics)
}

func (h *heard) waitFor(t *testing.T, topic, want string) {
	t.Helper()
	h.wait(t, topic+" = "+want, func(topics map[string]string) bool { return topics[topic] == want })
}

func (h *heard) waitState(t *testing.T, id string, want contractv1.State) {
	t.Helper()
	h.wait(t, id+" state", func(topics map[string]string) bool {
		var got contractv1.State
		return json.Unmarshal([]byte(topics[root.State(id)]), &got) == nil && reflect.DeepEqual(got, want)
	})
}

// run serves the plugin until the returned func is called, and waits for it to come online.
func run(t *testing.T, url string, declared ...contractv1.DeclaredDevice) (stop func()) {
	t.Helper()
	return runOn(t, url, url, declared...)
}

// runOn runs the plugin with Oikade on one broker and Zigbee2MQTT on another. Under the host, its
// link to Oikade is a socket, which outlives the broker.
func runOn(t *testing.T, oikade, zigbee string, declared ...contractv1.DeclaredDevice) (stop func()) {
	t.Helper()
	return runWith(t, oikade, Config{Broker: zigbee, BaseTopic: base}, declared...)
}

func runWith(t *testing.T, oikade string, c Config, declared ...contractv1.DeclaredDevice) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	h, err := factory(c, logging.Test(t))(plugin.Environment{Root: root}, declared)
	if err != nil {
		t.Fatal(err)
	}
	h.(*bridge).infoWait = time.Second
	started, startCancel := context.WithTimeout(ctx, waitLimit)
	defer startCancel()
	p, err := plugin.Start(started, plugin.Options{
		Environment: plugin.Environment{MQTTURL: oikade, Root: root}, OnSet: h.Set,
		OnSetup: h.(plugin.Setup).Setup, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := h.Serve(ctx, p); err != nil {
			t.Error(err)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
			_ = p.Close(context.Background()) //nolint:usetesting // t.Context() is done once cleanups run
		})
	}
	t.Cleanup(stop)
	return stop
}

func bridgeDevices(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func reported(t *testing.T, payload string) contractv1.Devices {
	t.Helper()
	var d contractv1.Devices
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &d); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestItNeedsABroker(t *testing.T) {
	_, err := New(Config{})(plugin.Environment{Root: root}, nil)
	if err == nil || !strings.Contains(err.Error(), "no Broker") {
		t.Fatalf("got %v", err)
	}
}

func TestABrokersPasswordIsOutOfItsURLForAUserItNames(t *testing.T) {
	for _, c := range []struct {
		name   string
		config Config
		want   string
	}{
		{"not a URL", Config{Broker: "mqtt://broker:port"}, "isn't a broker's URL"},
		{"no scheme", Config{Broker: "localhost:1883"}, "isn't a broker's URL"},
		{"a password in the URL", Config{Broker: "mqtt://oikade:hunter2@broker:1883"}, "has a password in it"},
		{"no user for the password", Config{Broker: "mqtt://broker:1883", Password: "hunter2"}, "mqtt://oikade@broker:1883"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.config)(plugin.Environment{Root: root}, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want one saying %q", err, c.want)
			}
			if strings.Contains(fmt.Sprint(err), "hunter2") {
				t.Errorf("the error shows the password: %v", err)
			}
		})
	}
}

// passwordHook lets in only the user oikade with its password, as a broker with a password does.
type passwordHook struct {
	mochi.HookBase
	password string
}

func (h *passwordHook) ID() string { return "password" }

func (h *passwordHook) Provides(b byte) bool {
	return b == mochi.OnConnectAuthenticate || b == mochi.OnACLCheck
}

func (h *passwordHook) OnConnectAuthenticate(_ *mochi.Client, pk packets.Packet) bool {
	return string(pk.Connect.Username) == "oikade" && string(pk.Connect.Password) == h.password
}

func (h *passwordHook) OnACLCheck(*mochi.Client, string, bool) bool { return true }

// passwordBroker starts a broker with Zigbee2MQTT's devices on it, which lets in only the user
// oikade with password, and returns its URL for that user.
func passwordBroker(t *testing.T, password string) string {
	t.Helper()
	s := mochi.New(&mochi.Options{InlineClient: true, Logger: slog.New(slog.DiscardHandler)})
	if err := s.AddHook(&passwordHook{password: password}, nil); err != nil {
		t.Fatal(err)
	}
	ln := listeners.NewTCP(listeners.Config{ID: "zigbee", Address: "127.0.0.1:0"})
	if err := s.AddListener(ln); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Publish(base+"/bridge/devices", []byte(bridgeDevices(t)), true, 0); err != nil {
		t.Fatal(err)
	}
	return "mqtt://oikade@" + ln.Address()
}

func TestThePluginLogsInWithItsBrokersPassword(t *testing.T) {
	const password = "sec ret:@/"
	zigbee := passwordBroker(t, password)
	oikade := mqtttest.StartBroker(t)
	h := hear(t, oikade)
	// With the newline the environment can leave after it, which is no part of it.
	runWith(t, oikade, Config{Broker: zigbee, Password: password + "\n", BaseTopic: base})
	h.wait(t, "its devices", func(topics map[string]string) bool {
		return len(reported(t, topics[root.Devices()]).Discovered) > 0
	})

	// Another password isn't let in.
	handler, err := factory(Config{Broker: zigbee, Password: "not " + password, BaseTopic: base}, logging.Test(t))(plugin.Environment{Root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := handler.(*bridge)
	b.connectTimeout = 200 * time.Millisecond
	if err := b.Serve(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Fatalf("with another password: %v", err)
	}
}

func TestEveryPairedDeviceIsReportedAndCanBeSet(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	h.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	run(t, url, contractv1.DeclaredDevice{ID: "0x00aa000000000000", Name: "gone", DisplayName: "gone"})

	h.wait(t, "devices", func(topics map[string]string) bool {
		return len(reported(t, topics[root.Devices()]).Discovered) > 0
	})
	got := reported(t, h.copy()[root.Devices()])

	if i := slices.IndexFunc(got.Discovered, func(d contractv1.DiscoveredDevice) bool { return d.ID == plugIEEE }); i < 0 ||
		got.Discovered[i].Name != "desk-plug" || !slices.Equal(got.Discovered[i].Capabilities, []string{"on"}) {
		t.Errorf("the plug: %+v", got.Discovered)
	}
	if slices.ContainsFunc(got.Discovered, func(d contractv1.DiscoveredDevice) bool { return d.ID == "0x00124b0000000001" }) {
		t.Error("the coordinator was reported")
	}
	if i := slices.IndexFunc(got.Devices, func(d contractv1.DeviceReport) bool { return d.ID == "0x00aa000000000000" }); i < 0 || got.Devices[i].Found {
		t.Errorf("a device on record that's gone: %+v", got.Devices)
	}
	if i := slices.IndexFunc(got.Devices, func(d contractv1.DeviceReport) bool { return d.ID == plugIEEE }); i < 0 || !got.Devices[i].Found ||
		got.Devices[i].DeviceType != "" {
		t.Errorf("the plug: %+v", got.Devices)
	}
	// The light says what it is, and its firmware, in both lists, with nobody setting it.
	if i := slices.IndexFunc(got.Devices, func(d contractv1.DeviceReport) bool { return d.ID == lampIEEE }); i < 0 ||
		got.Devices[i].DeviceType != contractv1.DimmableLight || got.Devices[i].FirmwareVersion != "2.3.087" {
		t.Errorf("the lamp: %+v", got.Devices)
	}
	if i := slices.IndexFunc(got.Discovered, func(d contractv1.DiscoveredDevice) bool { return d.ID == lampIEEE }); i < 0 ||
		got.Discovered[i].DeviceType != contractv1.DimmableLight || got.Discovered[i].FirmwareVersion != "2.3.087" {
		t.Errorf("the lamp: %+v", got.Discovered)
	}

	// A sleeping sensor reports one value at a time, and that's published as far as it's known.
	h.publish(t, base+"/hallway-motion", `{"occupancy":true}`, false)
	h.waitState(t, motionIEEE, contractv1.State{"occupied": true})
	h.publish(t, base+"/hallway-motion", `{"battery":90}`, false)
	h.waitState(t, motionIEEE, contractv1.State{"occupied": true, "battery": 90.0})

	cmd, err := contractv1.SetCommand{ID: plugIEEE, Capability: "on", Value: true}.Payload()
	if err != nil {
		t.Fatal(err)
	}
	h.publish(t, root.Set(plugIEEE), string(cmd), false)
	h.waitFor(t, base+"/"+plugIEEE+"/set", `{"state":"ON"}`)
}

func TestARestartKeepsWhatZigbee2MQTTRetains(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	// The order a broker replays these in is Instance's to handle: see
	// TestMessagesFromBeforeTheDeviceListAreKept.
	h.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	h.publish(t, base+"/front-door", `{"contact":true}`, true)
	h.publish(t, base+"/front-door/availability", `{"state":"offline"}`, true)
	stop := run(t, url)
	h.waitState(t, doorIEEE, contractv1.State{"open": false})
	h.waitFor(t, root.Availability(doorIEEE), contractv1.Offline)

	// A deploy waits for this, with the automations stopped.
	began := time.Now()
	stop()
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("stopping took %s", took)
	}
	h.mu.Lock()
	delete(h.topics, root.State(doorIEEE))
	delete(h.topics, root.Availability(doorIEEE))
	h.mu.Unlock()

	run(t, url)
	h.waitState(t, doorIEEE, contractv1.State{"open": false})
	h.waitFor(t, root.Availability(doorIEEE), contractv1.Offline)
}

func TestDevicesFollowZigbee2MQTTOfflineAndBack(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	h.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	h.publish(t, base+"/bridge/state", `{"state":"online"}`, true)
	h.publish(t, base+"/front-door/availability", `{"state":"offline"}`, true)
	h.publish(t, base+"/desk-plug/availability", `{"state":"online"}`, true)
	run(t, url)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Online)
	h.waitFor(t, root.Availability(doorIEEE), contractv1.Offline)

	h.publish(t, base+"/bridge/state", `{"state":"offline"}`, true)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Offline)

	h.publish(t, base+"/bridge/state", "online", true) // The bare form, from before 2.x.
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Online)
	// A device with no word of its own counts as online, and one that was offline stays so.
	h.waitFor(t, root.Availability(motionIEEE), contractv1.Online)
	if got := func() string {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.topics[root.Availability(doorIEEE)]
	}(); got != contractv1.Offline {
		t.Errorf("the door = %s", got)
	}
}

func TestAWriteIsRefusedWhileZigbee2MQTTIsOffline(t *testing.T) {
	bus := mqtt.NewMemory()
	b := &bridge{up: false, instance: &Instance{BaseTopic: base, Bus: bus}}
	err := b.Set(t.Context(), nil, contractv1.SetCommand{ID: plugIEEE, Capability: "on", Value: true})
	if !errors.Is(err, errBridgeOffline) {
		t.Fatalf("got %v", err)
	}
	if got := bus.Sent(base + "/" + plugIEEE + "/set"); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
}

func TestADeviceThatLeftStaysOffline(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	h.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	const gone = "0x00aa000000000000"
	run(t, url, contractv1.DeclaredDevice{ID: gone, Name: "gone", DisplayName: "gone"})
	h.waitFor(t, root.Availability(gone), contractv1.Offline)
	h.publish(t, base+"/bridge/state", `{"state":"offline"}`, true)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Offline)
	h.publish(t, base+"/bridge/state", `{"state":"online"}`, true)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Online)
	h.waitFor(t, root.Availability(gone), contractv1.Offline)
}

func TestLosingTheBrokerMarksDevicesOffline(t *testing.T) {
	oikade := mqtttest.StartBroker(t)
	zigbee := mqtttest.NewBroker(t)
	h := hear(t, oikade)
	z := hear(t, zigbee.URL)
	z.publish(t, base+"/bridge/state", `{"state":"online"}`, true)
	z.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	runOn(t, oikade, zigbee.URL)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Online)

	zigbee.Restart(t)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Offline)
	// This client is reconnecting too.
	deadline := time.Now().Add(waitLimit)
	for z.bus.Publish(t.Context(), base+"/bridge/state", []byte(`{"state":"online"}`), true) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the test's client never reconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	z.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Online)
}

func TestItGivesUpOnABrokerItCantReach(t *testing.T) {
	broker, err := mqtttest.Listen()
	if err != nil {
		t.Fatal(err)
	}
	broker.Close()
	h, err := factory(Config{Broker: broker.URL}, logging.Test(t))(plugin.Environment{Root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := h.(*bridge)
	if !ok {
		t.Fatalf("got %T", h)
	}
	b.connectTimeout = 50 * time.Millisecond
	if err := b.Serve(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Fatalf("got %v", err)
	}
}

// One message is one report, so nothing sees half of it, and a steady sensor still says it's alive.
func TestAMessageIsReportedWholeAndOnceEvenIfNothingChanged(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	var reports []string
	var mu sync.Mutex
	_, err := h.bus.Subscribe(t.Context(), root.State(motionIEEE), func(m mqtt.Message) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, string(m.Payload))
	})
	if err != nil {
		t.Fatal(err)
	}
	h.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	run(t, url)
	h.wait(t, "devices", func(topics map[string]string) bool { return topics[root.Devices()] != "" })

	h.publish(t, base+"/hallway-motion", `{"occupancy":true,"battery":90}`, false)
	h.waitState(t, motionIEEE, contractv1.State{"occupied": true, "battery": 90.0})
	h.publish(t, base+"/hallway-motion", `{"occupancy":true,"battery":90}`, false)
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(reports)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 2 || reports[0] != reports[1] {
		t.Errorf("got %q, want the same whole report twice", reports)
	}
}

func TestStartsWithZigbee2MQTTAlreadyOffline(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	h.publish(t, base+"/bridge/state", `{"state":"offline"}`, true)
	h.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	run(t, url)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Offline)
}

func TestItFollowsADefaultBaseTopic(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	h.publish(t, DefaultBaseTopic+"/bridge/devices", bridgeDevices(t), true)
	ctx, cancel := context.WithCancel(t.Context())
	env := plugin.Environment{MQTTURL: url, Root: root}
	handler, err := factory(Config{Broker: url}, logging.Test(t))(plugin.Environment{Root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plugin.Start(ctx, plugin.Options{Environment: env, OnSet: handler.Set, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = handler.Serve(ctx, p)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = p.Close(context.Background()) //nolint:usetesting // t.Context() is done once cleanups run
	})
	h.wait(t, "devices", func(topics map[string]string) bool {
		return len(reported(t, topics[root.Devices()]).Discovered) > 0
	})
}
