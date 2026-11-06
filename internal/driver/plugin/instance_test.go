package plugin

import (
	"context"
	"slices"
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
		undeclared: -1,
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

func (r *recorder) takeChanges() []driver.Update {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.changes
	r.changes = nil
	return c
}

func dev(name string) ident.ID {
	return ident.ID{Namespace: "home", Name: name}
}

const root = "oikade/v1/home/sonoff"

var (
	porch = dev("porch-light")
	hall  = dev("hall-light")
	ghost = dev("not-found")
	now   = time.Date(2026, 10, 18, 19, 0, 0, 0, time.UTC)
)

const devicesJSON = `{"devices":[
	{"id":"1000aa","found":true,"manufacturer":"SONOFF","model":"MINIR4","capabilities":["light.on","sensor.temperature"]},
	{"id":"1000bb","found":true,"manufacturer":"SONOFF","model":"MINIR4","capabilities":["switch.on","colour.hue"]},
	{"id":"1000ff","found":false,"error":"no LAN key for 1000ff"}
],"undeclared":2}`

// publish sends a retained message, as plugins do on every topic.
func publish(t *testing.T, bus *mqtt.Memory, topic, payload string) {
	t.Helper()
	if err := bus.Publish(context.Background(), topic, []byte(payload), true); err != nil {
		t.Fatal(err)
	}
}

// setup starts an instance the way the hub does: subscribe first, then declare devices.
func setup(t *testing.T, retained map[string]string) (*Instance, *mqtt.Memory, *recorder) {
	t.Helper()
	bus := mqtt.NewMemory()
	for topic, payload := range retained {
		publish(t, bus, root+"/"+topic, payload)
	}
	rec := newRecorder()
	i := &Instance{
		Key: dev("sonoff"), Root: root, Bus: bus, Sink: rec,
		Now: func() time.Time { return now }, Log: logging.Discard(),
	}
	if err := i.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	i.SetDevices(map[string]ident.ID{"1000aa": porch, "1000bb": hall, "1000ff": ghost})
	return i, bus, rec
}

func online() map[string]string {
	return map[string]string{
		"status":  `{"state":"online","contract":1}`,
		"devices": devicesJSON,
	}
}

func TestDevicesDropsUnknownCapability(t *testing.T) {
	i, _, rec := setup(t, online())

	if got := rec.discovered[porch]; !got.Found || got.Model != "MINIR4" ||
		!slices.Equal(got.Capabilities, []string{capability.LightOn, capability.Temperature}) {
		t.Errorf("porch = %+v", got)
	}
	if got := rec.discovered[hall].Capabilities; !slices.Equal(got, []string{capability.SwitchOn}) {
		t.Errorf("hall capabilities = %v, want colour.hue dropped", got)
	}
	if got := rec.discovered[ghost]; got.Found || got.Error != "no LAN key for 1000ff" {
		t.Errorf("ghost = %+v, want not found with the plugin's error", got)
	}
	if rec.undeclared != 2 {
		t.Errorf("undeclared = %d, want 2", rec.undeclared)
	}
	if got := i.Stats().Rejected[ReasonUnknownCapability]; got != 1 {
		t.Errorf("unknown_capability = %d, want 1", got)
	}
}

func TestStateChangesAndRejects(t *testing.T) {
	i, bus, rec := setup(t, online())

	publish(t, bus, root+"/1000aa/state", `{"light.on":true,"sensor.temperature":21.5}`)
	got := rec.takeChanges()
	if len(got) != 2 || got[0].Capability != capability.LightOn || got[0].Value != true || got[0].Previous != nil ||
		got[1].Value != 21.5 || got[1].Device != porch || !got[1].Time.Equal(now) {
		t.Fatalf("first state = %+v", got)
	}
	if !rec.seen[porch].Equal(now) {
		t.Errorf("porch not seen")
	}

	// The full state again, with only the temperature changed.
	publish(t, bus, root+"/1000aa/state", `{"light.on":true,"sensor.temperature":22}`)
	got = rec.takeChanges()
	if len(got) != 1 || got[0].Value != 22.0 || got[0].Previous != 21.5 {
		t.Fatalf("diffed state = %+v", got)
	}

	for _, payload := range []string{
		`{"light.on":"on"}`,           // invalid_value: wrong kind
		`{"sensor.temperature":5000}`, // invalid_value: out of range
		`{"colour.hue":120}`,          // unknown_capability
		`{"switch.on":true}`,          // unannounced_capability: porch announced light.on
		`not json`,                    // malformed
	} {
		publish(t, bus, root+"/1000aa/state", payload)
	}
	publish(t, bus, root+"/1000cc/state", `{"switch.on":true}`) // undeclared_device
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("rejected messages produced changes: %+v", got)
	}
	want := map[string]uint64{
		ReasonInvalidValue: 2, ReasonUnknownCapability: 2, ReasonUnannouncedCapability: 1,
		ReasonMalformed: 1, ReasonUndeclaredDevice: 1,
	}
	stats := i.Stats()
	for reason, n := range want {
		if stats.Rejected[reason] != n {
			t.Errorf("%s = %d, want %d (all: %v)", reason, stats.Rejected[reason], n, stats.Rejected)
		}
	}
}

func TestRetainedStateBeforeDeclaration(t *testing.T) {
	// The broker replays retained topics in any order, and before the hub declares devices.
	retained := online()
	retained["1000aa/state"] = `{"light.on":true}`
	retained["1000aa/availability"] = "online"
	i, _, rec := setup(t, retained)

	if got := rec.takeChanges(); len(got) != 1 || got[0].Device != porch || got[0].Value != true {
		t.Fatalf("replayed state = %+v", got)
	}
	if !rec.available[porch] {
		t.Errorf("porch availability not replayed")
	}
	if n := i.Stats().Rejected[ReasonUndeclaredDevice]; n != 0 {
		t.Errorf("undeclared_device = %d before any declaration, want 0", n)
	}

	// Re-declaring re-reports discovery but doesn't repeat changes.
	i.SetDevices(map[string]ident.ID{"1000aa": porch})
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("re-declaring repeated changes: %+v", got)
	}
}

func TestStateBeforeDevicesIsReplayed(t *testing.T) {
	_, bus, rec := setup(t, map[string]string{"status": `{"state":"online","contract":1}`})
	if len(rec.discovered) != 0 {
		t.Fatalf("devices discovered before the devices topic: %+v", rec.discovered)
	}

	publish(t, bus, root+"/1000aa/state", `{"light.on":false}`)
	if got := rec.takeChanges(); len(got) != 0 {
		t.Fatalf("state before devices produced changes: %+v", got)
	}
	publish(t, bus, root+"/devices", devicesJSON)
	if got := rec.takeChanges(); len(got) != 1 || got[0].Value != false {
		t.Fatalf("state not replayed after devices: %+v", got)
	}
	if !rec.discovered[porch].Found {
		t.Errorf("porch not discovered after devices: %+v", rec.discovered[porch])
	}
}

func TestStatusOfflineAndContract(t *testing.T) {
	retained := online()
	retained["1000aa/availability"] = "online"
	retained["1000bb/availability"] = "online"
	i, bus, rec := setup(t, retained)
	if !i.Stats().Up || !rec.available[porch] || !rec.available[hall] {
		t.Fatalf("not up: %+v %v", i.Stats(), rec.available)
	}

	// The Last Will.
	publish(t, bus, root+"/status", `{"state":"offline","contract":1}`)
	if i.Stats().Up {
		t.Errorf("up after offline status")
	}
	for _, d := range []ident.ID{porch, hall, ghost} {
		if rec.available[d] {
			t.Errorf("%s still available after the plugin went offline", d)
		}
	}
	if err := i.Set(context.Background(), porch, capability.LightOn, true); err == nil {
		t.Errorf("Set succeeded while the plugin is offline")
	}

	// Back online: the retained per-device availability counts again.
	publish(t, bus, root+"/status", `{"state":"online","contract":1}`)
	if !i.Stats().Up || !rec.available[porch] {
		t.Errorf("availability not restored when the plugin came back")
	}

	// A plugin speaking another contract is ignored entirely.
	publish(t, bus, root+"/status", `{"state":"online","contract":2}`)
	if i.Stats().Up || rec.available[porch] || rec.discovered[porch].Found {
		t.Errorf("contract 2 not ignored: up=%v available=%v info=%+v", i.Stats().Up, rec.available[porch], rec.discovered[porch])
	}
	publish(t, bus, root+"/1000aa/state", `{"light.on":true}`)
	publish(t, bus, root+"/devices", devicesJSON)
	if got := rec.takeChanges(); len(got) != 0 || rec.discovered[porch].Found {
		t.Errorf("messages under contract 2 were used: %+v", got)
	}
	if got := i.Stats().Rejected[ReasonWrongContract]; got != 3 {
		t.Errorf("wrong_contract = %d, want 3", got)
	}
}

func TestSetPublishes(t *testing.T) {
	i, bus, _ := setup(t, online())
	ctx := context.Background()

	if err := i.Set(ctx, porch, capability.LightOn, false); err != nil {
		t.Fatal(err)
	}
	if got := bus.Sent(root + "/1000aa/set"); !slices.Equal(got, []string{`{"light.on":false}`}) {
		t.Errorf("sent = %v", got)
	}
	if bus.Published[len(bus.Published)-1].Retained {
		t.Errorf("/set was retained")
	}

	for name, call := range map[string]func() error{
		"unannounced capability": func() error { return i.Set(ctx, porch, capability.SwitchOn, true) },
		"capability dropped as unknown": func() error {
			return i.Set(ctx, hall, "colour.hue", 1.0)
		},
		"device not found by the plugin": func() error { return i.Set(ctx, ghost, capability.SwitchOn, true) },
		"undeclared device":              func() error { return i.Set(ctx, dev("other"), capability.SwitchOn, true) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: Set succeeded", name)
		}
	}
	if got := bus.Sent(root + "/1000bb/set"); len(got) != 0 {
		t.Errorf("refused Set published %v", got)
	}
}

func TestAWriteWaitsForTheNextProcessToAnnounce(t *testing.T) {
	i, bus, _ := setup(t, online())
	ctx := context.Background()
	if err := i.Set(ctx, porch, capability.LightOn, false); err != nil {
		t.Fatal(err)
	}
	i.Forget()
	if err := i.Set(ctx, porch, capability.LightOn, false); err == nil {
		t.Fatal("a write was taken with nothing announced")
	}
	publish(t, bus, root+"/devices", devicesJSON)
	if err := i.Set(ctx, porch, capability.LightOn, false); err != nil {
		t.Fatal(err)
	}
}

func TestStopUnsubscribes(t *testing.T) {
	i, bus, rec := setup(t, online())
	i.Stop()
	publish(t, bus, root+"/1000aa/state", `{"light.on":true}`)
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("changes after Stop: %+v", got)
	}
}

func TestDiscoveredCountsUndeclared(t *testing.T) {
	retained := online()
	// 1000aa is declared, 1000cc isn't, and the plugin's own count is ignored.
	retained["devices"] = `{"devices":[],"undeclared":7,"discovered":[
		{"id":"1000aa","model":"MINIR4","capabilities":["light.on"],"name":"Porch light","room":"Porch"},
		{"id":"1000cc","capabilities":["switch.on","colour.hue"],"name":"Desk lamp"},
		{"id":"1000cc"},
		{"id":"bad/id"}
	]}`
	i, bus, rec := setup(t, retained)

	if rec.undeclared != 1 {
		t.Errorf("undeclared = %d, want 1", rec.undeclared)
	}
	got, ok := i.Discovered()
	if !ok || len(got) != 2 || got[0].Name != "Porch light" || got[0].Room != "Porch" ||
		!slices.Equal(got[1].Capabilities, []string{capability.SwitchOn}) {
		t.Errorf("Discovered() = %+v, %v", got, ok)
	}
	if len(rec.inventory) != 2 || rec.inventory[0].ID != "1000aa" || rec.inventory[0].Name != "Porch light" ||
		rec.inventory[1].ID != "1000cc" {
		t.Errorf("inventory = %+v", rec.inventory)
	}
	if r := i.Stats().Rejected; r[ReasonMalformed] != 2 || r[ReasonUnknownCapability] != 1 {
		t.Errorf("rejected = %v, want 2 malformed and 1 unknown_capability", r)
	}

	// Declaring 1000cc leaves nothing undeclared.
	i.SetDevices(map[string]ident.ID{"1000aa": porch, "1000cc": hall})
	if rec.undeclared != 0 {
		t.Errorf("undeclared after declaring = %d, want 0", rec.undeclared)
	}

	// An empty list is still a list: the app has no devices.
	publish(t, bus, root+"/devices", `{"devices":[],"undeclared":3,"discovered":[]}`)
	if got, ok := i.Discovered(); !ok || len(got) != 0 {
		t.Errorf("Discovered() = %+v, %v, want empty and true", got, ok)
	}
	if rec.undeclared != 0 {
		t.Errorf("undeclared = %d, want 0", rec.undeclared)
	}

	// Without the list, the plugin's count is used.
	publish(t, bus, root+"/devices", `{"devices":[],"undeclared":3}`)
	if _, ok := i.Discovered(); ok {
		t.Error("Discovered() is ok without a discovered list")
	}
	if rec.inventory != nil {
		t.Errorf("inventory without a discovered list = %+v, want nil", rec.inventory)
	}
	if rec.undeclared != 3 {
		t.Errorf("undeclared = %d, want 3", rec.undeclared)
	}

	// A contract Oikade doesn't speak forgets the list and the count.
	publish(t, bus, root+"/devices", `{"devices":[],"discovered":[{"id":"1000dd"}]}`)
	publish(t, bus, root+"/status", `{"state":"online","contract":2}`)
	if rec.inventory != nil {
		t.Errorf("inventory after a wrong contract = %+v, want nil", rec.inventory)
	}
	if _, ok := i.Discovered(); ok || rec.undeclared != 0 {
		t.Errorf("after a wrong contract, Discovered() ok = %v and undeclared = %d, want false and 0", ok, rec.undeclared)
	}
}
