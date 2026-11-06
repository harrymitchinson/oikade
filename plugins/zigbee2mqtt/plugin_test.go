package zigbee2mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"slices"
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
	waitLimit  = 20 * time.Second
	plugIEEE   = "0x00124b0000000004"
	motionIEEE = "0x00158d0000000002"
	doorIEEE   = "0x00158d0000000003"
	base       = "z2m"
)

var root = contractv1.NewRoot("home", "zigbee")

// heard follows what the plugin publishes, as the host would.
type heard struct {
	mu     sync.Mutex
	topics map[string]string
	bus    *mqtt.Client
}

func hear(t *testing.T, url string) *heard {
	t.Helper()
	// The connection lasts as long as this context.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bus, err := mqtt.Connect(ctx, url, "z2m-test-"+clientID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Disconnect(context.Background()) })
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
	if err := h.bus.Publish(context.Background(), topic, []byte(payload), retain); err != nil {
		t.Fatal(err)
	}
}

func (h *heard) wait(t *testing.T, what string, ok func(topics map[string]string) bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		done := ok(h.topics)
		h.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t.Fatalf("no %s in %v", what, h.topics)
}

func (h *heard) waitFor(t *testing.T, topic, want string) {
	t.Helper()
	h.wait(t, topic+" = "+want, func(topics map[string]string) bool { return topics[topic] == want })
}

// waitState waits for a device's published state to be want.
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
	ctx, cancel := context.WithCancel(context.Background())
	env := plugin.Environment{MQTTURL: zigbee, Root: root}
	h, err := New(Config{BaseTopic: base})(env, declared)
	if err != nil {
		t.Fatal(err)
	}
	started, startCancel := context.WithTimeout(ctx, waitLimit)
	defer startCancel()
	p, err := plugin.Start(started, plugin.Options{
		Environment: plugin.Environment{MQTTURL: oikade, Root: root}, OnSet: h.Set,
		Logger: slog.New(slog.DiscardHandler),
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
			_ = p.Close(context.Background())
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
	if err == nil || !strings.Contains(err.Error(), "--broker") {
		t.Fatalf("got %v", err)
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
	h.mu.Lock()
	got := reported(t, h.topics[root.Devices()])
	h.mu.Unlock()

	if i := slices.IndexFunc(got.Discovered, func(d contractv1.DiscoveredDevice) bool { return d.ID == plugIEEE }); i < 0 ||
		got.Discovered[i].Name != "desk-plug" || !slices.Equal(got.Discovered[i].Capabilities, []string{"switch.on"}) {
		t.Errorf("the plug: %+v", got.Discovered)
	}
	if slices.ContainsFunc(got.Discovered, func(d contractv1.DiscoveredDevice) bool { return d.ID == "0x00124b0000000001" }) {
		t.Error("the coordinator was reported")
	}
	if i := slices.IndexFunc(got.Devices, func(d contractv1.DeviceReport) bool { return d.ID == "0x00aa000000000000" }); i < 0 || got.Devices[i].Found {
		t.Errorf("a device on record that's gone: %+v", got.Devices)
	}
	if i := slices.IndexFunc(got.Devices, func(d contractv1.DeviceReport) bool { return d.ID == plugIEEE }); i < 0 || !got.Devices[i].Found {
		t.Errorf("the plug: %+v", got.Devices)
	}

	// A sleeping sensor reports one value at a time, and that's published as far as it's known.
	h.publish(t, base+"/hallway-motion", `{"occupancy":true}`, false)
	h.waitState(t, motionIEEE, contractv1.State{"sensor.occupancy-detected": true})
	h.publish(t, base+"/hallway-motion", `{"battery":90}`, false)
	h.waitState(t, motionIEEE, contractv1.State{"sensor.occupancy-detected": true, "battery.level": 90.0})

	cmd, err := contractv1.SetCommand{ID: plugIEEE, Capability: "switch.on", Value: true}.Payload()
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
	h.waitState(t, doorIEEE, contractv1.State{"sensor.contact-open": false})
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
	h.waitState(t, doorIEEE, contractv1.State{"sensor.contact-open": false})
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
	err := b.Set(context.Background(), nil, contractv1.SetCommand{ID: plugIEEE, Capability: "switch.on", Value: true})
	if !errors.Is(err, errBridgeOffline) {
		t.Fatalf("got %v", err)
	}
	if got := bus.Sent(base + "/" + plugIEEE + "/set"); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
}

// A process that's gone reads as offline, so a new one has to say otherwise, even of a device
// Zigbee2MQTT says nothing about.
func TestARestartBringsBackDevicesWithNoAvailabilityOfTheirOwn(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	h.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	run(t, url)
	h.waitFor(t, root.Availability(plugIEEE), contractv1.Online)
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
	for z.bus.Publish(context.Background(), base+"/bridge/state", []byte(`{"state":"online"}`), true) != nil {
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
	h, err := New(Config{})(plugin.Environment{MQTTURL: broker.URL, Root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := h.(*bridge)
	if !ok {
		t.Fatalf("got %T", h)
	}
	b.connectTimeout = 50 * time.Millisecond
	if err := b.Serve(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Fatalf("got %v", err)
	}
}

// One message is one report, so nothing sees half of it, and a steady sensor still says it's alive.
func TestAMessageIsReportedWholeAndOnceEvenIfNothingChanged(t *testing.T) {
	url := mqtttest.StartBroker(t)
	h := hear(t, url)
	var reports []string
	var mu sync.Mutex
	_, err := h.bus.Subscribe(context.Background(), root.State(motionIEEE), func(m mqtt.Message) {
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
	h.waitState(t, motionIEEE, contractv1.State{"sensor.occupancy-detected": true, "battery.level": 90.0})
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
	ctx, cancel := context.WithCancel(context.Background())
	env := plugin.Environment{MQTTURL: url, Root: root}
	handler, err := New(Config{})(env, nil)
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
		_ = p.Close(context.Background())
	})
	h.wait(t, "devices", func(topics map[string]string) bool {
		return len(reported(t, topics[root.Devices()]).Discovered) > 0
	})
}
