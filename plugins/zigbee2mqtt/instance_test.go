package zigbee2mqtt

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

type recorder struct {
	mu         sync.Mutex
	discovered map[ident.ID]driver.Info
	changes    []driver.Update
	available  map[ident.ID]bool
	seen       map[ident.ID]time.Time
	undeclared int
	inventory  []driver.AppDevice
	inventoryN int // how many times Inventory was called
}

func newRecorder() *recorder {
	return &recorder{
		discovered: map[ident.ID]driver.Info{},
		available:  map[ident.ID]bool{},
		seen:       map[ident.ID]time.Time{},
	}
}

func (r *recorder) Discovered(d ident.ID, i driver.Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discovered[d] = i
}
func (r *recorder) Changed(u driver.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, u)
}
func (r *recorder) Availability(d ident.ID, online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.available[d] = online
}
func (r *recorder) Seen(d ident.ID, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[d] = t
}
func (r *recorder) Undeclared(_ driver.InstanceKey, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.undeclared = n
}
func (r *recorder) Inventory(_ driver.InstanceKey, devices []driver.AppDevice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inventory = devices
	r.inventoryN++
}

func dev(name string) ident.ID {
	return ident.ID{Namespace: "home", Name: name}
}

var (
	lamp   = dev("hallway-light")
	motion = dev("hallway-motion")
	door   = dev("front-door")
	plug   = dev("desk-plug")
	ghost  = dev("not-paired")
)

func setup(t *testing.T) (*Instance, *mqtt.Memory, *recorder) {
	t.Helper()
	ctx := context.Background()
	bus := mqtt.NewMemory()
	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = bus.Publish(ctx, "z2m/bridge/devices", devices, true)

	rec := newRecorder()
	now := time.Date(2026, 10, 17, 19, 0, 0, 0, time.UTC)
	i := &Instance{
		Key: dev("z2m"), BaseTopic: "z2m", Bus: bus, Sink: rec,
		Now: func() time.Time { return now }, Log: logging.Discard(),
	}
	i.SetDevices(map[string]ident.ID{
		"0x000b57fffe000001": lamp,
		"0x00158d0000000002": motion,
		"0x00158d0000000003": door,
		"0x00124b0000000004": plug,
		"0x00124b00000000ff": ghost,
	})
	if err := i.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.Stop)
	return i, bus, rec
}

func TestDiscovery(t *testing.T) {
	_, _, rec := setup(t)

	if got := rec.discovered[lamp]; !got.Found || got.Manufacturer != "IKEA" ||
		!slices.Equal(got.Capabilities, []string{capability.On, capability.Brightness, capability.LinkQuality}) {
		t.Errorf("lamp = %+v", got)
	}
	if got := rec.discovered[motion].Capabilities; !slices.Equal(got,
		[]string{capability.Occupied, capability.Temperature, capability.Battery, capability.LinkQuality}) {
		t.Errorf("motion capabilities = %v", got)
	}
	if got := rec.discovered[door].Capabilities; !slices.Equal(got, []string{capability.Open, capability.Battery}) {
		t.Errorf("door capabilities = %v", got)
	}
	if got := rec.discovered[plug].Capabilities; !slices.Equal(got, []string{capability.On}) {
		t.Errorf("plug capabilities = %v", got)
	}
	if rec.discovered[ghost].Found {
		t.Error("unpaired device reported as found")
	}
	// The ZBMINI is paired but not declared; the coordinator doesn't count.
	if rec.undeclared != 1 {
		t.Errorf("undeclared = %d, want 1", rec.undeclared)
	}
}

func TestStateChanges(t *testing.T) {
	ctx := context.Background()
	_, bus, rec := setup(t)

	// Addressed by IEEE because Zigbee2MQTT hasn't applied the friendly name yet.
	_ = bus.Publish(ctx, "z2m/0x000b57fffe000001", []byte(`{"state":"ON","brightness":127,"color_temp":300,"linkquality":80}`), false)
	_ = bus.Publish(ctx, "z2m/0x000b57fffe000001", []byte(`{"state":"ON","brightness":127,"linkquality":80}`), false)
	_ = bus.Publish(ctx, "z2m/front-door", []byte(`{"contact":false,"battery":91}`), false)
	_ = bus.Publish(ctx, "z2m/hallway-motion", []byte(`{"occupancy":true,"battery":"bogus"}`), false)

	type change struct {
		dev   ident.ID
		cap   string
		value any
	}
	got := make([]change, 0, len(rec.changes))
	for _, u := range rec.changes {
		got = append(got, change{u.Device, u.Capability, u.Value})
	}
	want := []change{
		{lamp, capability.On, true},
		{lamp, capability.Brightness, 50.0},
		{lamp, capability.LinkQuality, 80.0},
		// The repeat message changes nothing.
		{door, capability.Open, true}, // Zigbee2MQTT contact=false means open.
		{door, capability.Battery, 91.0},
		{motion, capability.Occupied, true}, // The invalid battery value is dropped.
	}
	if !slices.Equal(got, want) {
		t.Errorf("changes:\n got %v\nwant %v", got, want)
	}
	if _, ok := rec.seen[lamp]; !ok {
		t.Error("lamp not marked seen")
	}

	_ = bus.Publish(ctx, "z2m/hallway-motion", []byte(`{"occupancy":false}`), false)
	last := rec.changes[len(rec.changes)-1]
	if last.Value != false || last.Previous != true {
		t.Errorf("last change = %+v, want false with previous true", last)
	}
}

// TestRenamedInFrontend checks that a Device follows its IEEE address, not its friendly name, which
// Zigbee2MQTT owns: after a rename, even to a name with a slash, state and commands still work.
func TestRenamedInFrontend(t *testing.T) {
	ctx := context.Background()
	i, bus, rec := setup(t)

	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(string(devices), `"friendly_name":"front-door"`, `"friendly_name":"garden/side gate"`, 1)
	if renamed == string(devices) {
		t.Fatal("fixture has no front-door")
	}
	_ = bus.Publish(ctx, "z2m/bridge/devices", []byte(renamed), true)

	_ = bus.Publish(ctx, "z2m/garden/side gate", []byte(`{"contact":true}`), false)
	_ = bus.Publish(ctx, "z2m/garden/side gate/availability", []byte(`{"state":"online"}`), false)
	// The old name no longer resolves.
	_ = bus.Publish(ctx, "z2m/front-door", []byte(`{"contact":false}`), false)
	// Nor do commands sent to the device by name.
	_ = bus.Publish(ctx, "z2m/garden/side gate/set", []byte(`{"contact":false}`), false)

	if len(rec.changes) != 1 || rec.changes[0].Device != door || rec.changes[0].Value != false {
		t.Errorf("changes = %+v, want the door closed once", rec.changes)
	}
	if !rec.available[door] {
		t.Error("door should be online under its new name")
	}
	if err := i.Set(ctx, plug, capability.On, true); err != nil {
		t.Fatal(err)
	}
	if sent := bus.Sent("z2m/0x00124b0000000004/set"); len(sent) != 1 {
		t.Errorf("set sent %v", sent)
	}
}

// TestNameIsAnotherAddress checks that a friendly name equal to another device's IEEE address
// doesn't take that device's topic.
func TestNameIsAnotherAddress(t *testing.T) {
	ctx := context.Background()
	_, bus, rec := setup(t)
	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	// desk-plug is listed after front-door, so a plain map write would give it the door's address.
	renamed := strings.Replace(string(devices), `"friendly_name":"desk-plug"`, `"friendly_name":"0x00158d0000000003"`, 1)
	_ = bus.Publish(ctx, "z2m/bridge/devices", []byte(renamed), true)
	_ = bus.Publish(ctx, "z2m/0x00158d0000000003", []byte(`{"contact":true}`), false)
	if len(rec.changes) != 1 || rec.changes[0].Device != door {
		t.Errorf("changes = %+v, want the door's", rec.changes)
	}
}

func TestAvailability(t *testing.T) {
	ctx := context.Background()
	_, bus, rec := setup(t)
	_ = bus.Publish(ctx, "z2m/desk-plug/availability", []byte(`{"state":"online"}`), true)
	_ = bus.Publish(ctx, "z2m/front-door/availability", []byte(`offline`), true)
	if !rec.available[plug] {
		t.Error("plug should be online")
	}
	if online, ok := rec.available[door]; !ok || online {
		t.Error("door should be offline")
	}
}

func TestSet(t *testing.T) {
	ctx := context.Background()
	i, bus, _ := setup(t)
	for _, tc := range []struct {
		dev   ident.ID
		cap   string
		value any
		want  string
	}{
		{lamp, capability.On, true, `{"state":"ON"}`},
		{lamp, capability.On, false, `{"state":"OFF"}`},
		{lamp, capability.Brightness, 40.0, `{"brightness":102}`},
		{plug, capability.On, true, `{"state":"ON"}`},
	} {
		if err := i.Set(ctx, tc.dev, tc.cap, tc.value); err != nil {
			t.Fatal(err)
		}
		topic := "z2m/" + map[ident.ID]string{lamp: "0x000b57fffe000001", plug: "0x00124b0000000004"}[tc.dev] + "/set"
		sent := bus.Sent(topic)
		if len(sent) == 0 || sent[len(sent)-1] != tc.want {
			t.Errorf("Set(%s, %s, %v) sent %v, want %s", tc.dev.Name, tc.cap, tc.value, sent, tc.want)
		}
	}
	if err := i.Set(ctx, door, capability.On, true); err == nil {
		t.Error("Set accepted a capability the device doesn't have")
	}
	if err := i.Set(ctx, ghost, capability.On, true); err == nil {
		t.Error("Set accepted an unpaired device")
	}
}

func TestInventory(t *testing.T) {
	_, bus, rec := setup(t)

	// Every paired device but the coordinator, sorted by IEEE address, declared or not. A device
	// still named by its IEEE address has no name.
	ids, names := make([]string, 0, len(rec.inventory)), make([]string, 0, len(rec.inventory))
	for _, d := range rec.inventory {
		ids, names = append(ids, d.ID), append(names, d.Name)
	}
	if !slices.Equal(ids, []string{"0x000b57fffe000001", "0x00124b0000000004", "0x00124b0000000005",
		"0x00158d0000000002", "0x00158d0000000003"}) {
		t.Errorf("inventory ids = %v", ids)
	}
	if !slices.Equal(names, []string{"", "desk-plug", "", "hallway-motion", "front-door"}) {
		t.Errorf("inventory names = %q", names)
	}
	if d := rec.inventory[1]; d.Model == "" || !slices.Equal(d.Capabilities, []string{capability.On}) {
		t.Errorf("desk-plug = %+v", d)
	}

	// An empty network is an empty list, not none.
	if err := bus.Publish(context.Background(), "z2m/bridge/devices", []byte(`[]`), true); err != nil {
		t.Fatal(err)
	}
	if rec.inventory == nil || len(rec.inventory) != 0 {
		t.Errorf("inventory after an empty list = %#v, want empty", rec.inventory)
	}
}

// A broker replays retained messages in any order, and only the device list resolves a device's.
func TestMessagesFromBeforeTheDeviceListAreKept(t *testing.T) {
	ctx := context.Background()
	bus := mqtt.NewMemory()
	rec := newRecorder()
	now := time.Date(2026, 10, 17, 19, 0, 0, 0, time.UTC)
	i := &Instance{
		Key: dev("z2m"), BaseTopic: "z2m", Bus: bus, Sink: rec,
		Now: func() time.Time { return now }, Log: logging.Discard(),
	}
	i.SetDevices(map[string]ident.ID{"0x00158d0000000003": door})
	if err := i.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, m := range [][2]string{
		{"z2m/front-door", `{"contact":false}`},
		{"z2m/front-door", `{"contact":true}`},
		{"z2m/front-door/availability", `{"state":"offline"}`},
		{"z2m/unknown", `{"contact":true}`},
	} {
		if err := bus.Publish(ctx, m[0], []byte(m[1]), true); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.changes) != 0 {
		t.Fatalf("heard %+v before the list", rec.changes)
	}

	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "z2m/bridge/devices", devices, true); err != nil {
		t.Fatal(err)
	}
	if online, ok := rec.available[door]; !ok || online {
		t.Errorf("available = %v, %v", online, ok)
	}
	if len(rec.changes) != 1 || rec.changes[0].Capability != capability.Open || rec.changes[0].Value != false {
		t.Errorf("changes = %+v, want only the latest contact", rec.changes)
	}
	if len(i.early) != 0 {
		t.Errorf("still holding %v", i.early)
	}
}

// A new device can report before it's in the list, and its first report may be its last for hours.
func TestAMessageFromADeviceNotYetListedIsKept(t *testing.T) {
	_, bus, rec := setup(t)
	ctx := context.Background()
	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "z2m/bridge/devices", []byte(`[]`), true); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "z2m/front-door", []byte(`{"contact":true}`), true); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "z2m/bridge/devices", devices, true); err != nil {
		t.Fatal(err)
	}
	if len(rec.changes) != 1 {
		t.Fatalf("changes = %+v", rec.changes)
	}
}

// A device re-paired under its address has no values anywhere else, so its first report counts.
func TestARepairedDevicesReportIsNotARepeat(t *testing.T) {
	_, bus, rec := setup(t)
	ctx := context.Background()
	report := []byte(`{"contact":true}`)
	if err := bus.Publish(ctx, "z2m/front-door", report, false); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "z2m/bridge/devices", []byte(`[]`), true); err != nil {
		t.Fatal(err)
	}
	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "z2m/bridge/devices", devices, true); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "z2m/front-door", report, false); err != nil {
		t.Fatal(err)
	}
	if len(rec.changes) != 2 {
		t.Fatalf("changes = %+v", rec.changes)
	}
}

// A message held while a device was undeclared is older than anything heard since.
func TestAHeldMessageDoesNotUndoANewerOne(t *testing.T) {
	i, bus, rec := setup(t)
	ctx := context.Background()
	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	i.SetDevices(map[string]ident.ID{})
	if err := bus.Publish(ctx, "z2m/front-door", []byte(`{"contact":false}`), false); err != nil {
		t.Fatal(err)
	}
	i.SetDevices(map[string]ident.ID{"0x00158d0000000003": door})
	if err := bus.Publish(ctx, "z2m/front-door", []byte(`{"contact":true}`), false); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "z2m/bridge/devices", devices, true); err != nil {
		t.Fatal(err)
	}
	if len(rec.changes) != 1 || rec.changes[0].Value != false {
		t.Errorf("changes = %+v, want only the newer report, of a closed door", rec.changes)
	}
}

// startRequesting starts an Instance on bus, with every paired device in the fixture declared but
// the ZBMINI, and waits for its first round of /get requests to end.
func startRequesting(t *testing.T, bus *mqtt.Memory, log *slog.Logger) (*Instance, *recorder) {
	t.Helper()
	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = bus.Publish(ctx, "z2m/bridge/devices", devices, true)
	rec := newRecorder()
	done := make(chan struct{}, 1)
	i := &Instance{
		Key: dev("z2m"), BaseTopic: "z2m", Bus: bus, Sink: rec, Now: time.Now, Log: log,
		getInterval: time.Millisecond,
		requested: func() {
			select {
			case done <- struct{}{}:
			default:
			}
		},
	}
	i.SetDevices(map[string]ident.ID{
		"0x000b57fffe000001": lamp,
		"0x00158d0000000002": motion,
		"0x00158d0000000003": door,
		"0x00124b0000000004": plug,
	})
	if err := i.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.Stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the /get requests never ended")
	}
	return i, rec
}

// gets is every /get sent, as topic=payload.
func gets(bus *mqtt.Memory) []string {
	var out []string
	for _, m := range slices.Clone(bus.Published) {
		if strings.HasSuffix(m.Topic, "/get") {
			out = append(out, m.Topic+"="+string(m.Payload))
		}
	}
	return out
}

// Only what a device can answer and nothing retained is asked for: the plug's state is retained, and
// the sensors' properties can't be read. Nothing is asked of a device that isn't declared.
func TestARetainedValueIsNotRequested(t *testing.T) {
	bus := mqtt.NewMemory()
	_ = bus.Publish(context.Background(), "z2m/desk-plug", []byte(`{"state":"OFF"}`), true)
	i, _ := startRequesting(t, bus, logging.Discard())
	want := []string{
		`z2m/0x000b57fffe000001/get={"state":""}`,
		`z2m/0x000b57fffe000001/get={"brightness":""}`,
	}
	if got := gets(bus); !slices.Equal(got, want) {
		t.Errorf("gets:\n got %v\nwant %v", got, want)
	}
	// The broker echoes each /get back, and it isn't held for the next device list.
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.early) != 0 {
		t.Errorf("holding %v", i.early)
	}
}

// A device Zigbee2MQTT says is offline can't answer, so it isn't asked.
func TestAnOfflineDeviceIsNotAsked(t *testing.T) {
	bus := mqtt.NewMemory()
	_ = bus.Publish(context.Background(), "z2m/0x000b57fffe000001/availability", []byte(`{"state":"offline"}`), true)
	startRequesting(t, bus, logging.Discard())
	want := []string{`z2m/0x00124b0000000004/get={"state":""}`}
	if got := gets(bus); !slices.Equal(got, want) {
		t.Errorf("gets:\n got %v\nwant %v", got, want)
	}
}

// A reply is a first value, as a retained one is: it has no previous value, so it fires nothing.
func TestAReplyIsAFirstValue(t *testing.T) {
	bus := mqtt.NewMemory()
	_, err := bus.Subscribe(context.Background(), "z2m/0x000b57fffe000001/get", func(mqtt.Message) {
		_ = bus.Publish(context.Background(), "z2m/0x000b57fffe000001", []byte(`{"state":"ON","brightness":254}`), false)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, rec := startRequesting(t, bus, logging.Discard())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.changes) != 2 || rec.changes[0].Previous != nil || rec.changes[1].Previous != nil ||
		rec.changes[0].Value != true || rec.changes[1].Value != 100.0 {
		t.Errorf("changes = %+v, want the lamp's state and level, each with no previous value", rec.changes)
	}
	// Answered once, its brightness isn't asked for as well.
	lampGets := slices.DeleteFunc(gets(bus), func(g string) bool { return !strings.HasPrefix(g, "z2m/0x000b57fffe000001/") })
	if len(lampGets) != 1 {
		t.Errorf("gets of the lamp = %v, want only the first", lampGets)
	}
}

// A device asleep doesn't answer, and that isn't an error: its value stays unknown until it reports.
func TestADeviceThatNeverAnswersStaysUnknown(t *testing.T) {
	bus := mqtt.NewMemory()
	var logs bytes.Buffer
	_, rec := startRequesting(t, bus, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if len(gets(bus)) == 0 {
		t.Fatal("nothing was requested")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.changes) != 0 {
		t.Errorf("changes = %+v, want none", rec.changes)
	}
	if logs.Len() != 0 {
		t.Errorf("logged %s", logs.String())
	}
}

// Zigbee2MQTT's contact is true while the door is shut, and open is true while it's open.
func TestAShutContactIsNotOpen(t *testing.T) {
	ctx := context.Background()
	_, bus, rec := setup(t)
	_ = bus.Publish(ctx, "z2m/front-door", []byte(`{"contact":true}`), false)
	if len(rec.changes) == 0 {
		t.Fatal("no change")
	}
	if last := rec.changes[len(rec.changes)-1]; last.Device != door || last.Capability != capability.Open || last.Value != false {
		t.Fatalf("got %+v, want front-door open false", last)
	}
}
