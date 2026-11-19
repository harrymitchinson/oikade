package plugin

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

type recorder struct {
	mu         sync.Mutex
	discovered map[ident.ID]driver.Info
	changes    []driver.Update
	reports    int // how many times Changed was called
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
func (r *recorder) Changed(changes ...driver.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, changes...)
	r.reports++
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
	return ident.ID{Name: name}
}

const root = "oikade/v1/sonoff"

var (
	porch = dev("porch-light")
	hall  = dev("hall-light")
	ghost = dev("not-found")
	now   = time.Date(2026, 10, 18, 19, 0, 0, 0, time.UTC)
)

const devicesJSON = `{"devices":[
	{"id":"1000aa","found":true,"manufacturer":"SONOFF","model":"MINIR4","capabilities":["on","temperature"]},
	{"id":"1000bb","found":true,"manufacturer":"SONOFF","model":"MINIR4","capabilities":["on","colour.hue"]},
	{"id":"1000ff","found":false,"error":"no LAN key for 1000ff"}
],"undeclared":2}`

// publish sends a retained message, as plugins do on every topic.
func publish(t *testing.T, bus *mqtt.Memory, topic, payload string) {
	t.Helper()
	if err := bus.Publish(t.Context(), topic, []byte(payload), true); err != nil {
		t.Fatal(err)
	}
}

// setup starts an instance the way the hub does: subscribe first, then declare devices.
func setup(t *testing.T, retained map[string]string) (*Instance, *mqtt.Memory, *recorder) {
	t.Helper()
	return setupLogged(t, retained, logging.Discard())
}

// setupLogged is setup, logging to log.
func setupLogged(t *testing.T, retained map[string]string, log *slog.Logger) (*Instance, *mqtt.Memory, *recorder) {
	t.Helper()
	bus := mqtt.NewMemory()
	for topic, payload := range retained {
		publish(t, bus, root+"/"+topic, payload)
	}
	rec := newRecorder()
	i := &Instance{
		Key: dev("sonoff"), Root: root, Bus: bus, Sink: rec,
		Now: func() time.Time { return now }, Log: log,
	}
	if err := i.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	i.SetDevices(map[string]ident.ID{"1000aa": porch, "1000bb": hall, "1000ff": ghost}, nil)
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
		!slices.Equal(got.Capabilities, []string{capability.On, capability.Temperature}) {
		t.Errorf("porch = %+v", got)
	}
	if got := rec.discovered[hall].Capabilities; !slices.Equal(got, []string{capability.On}) {
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

	publish(t, bus, root+"/1000aa/state", `{"on":true,"temperature":21.5}`)
	if rec.reports != 1 {
		t.Errorf("one state reached the sink in %d calls, want 1", rec.reports)
	}
	got := rec.takeChanges()
	if len(got) != 2 || got[0].Capability != capability.On || got[0].Value != true || got[0].Previous != nil ||
		got[1].Value != 21.5 || got[1].Device != porch || !got[1].Time.Equal(now) {
		t.Fatalf("first state = %+v", got)
	}
	if !rec.seen[porch].Equal(now) {
		t.Errorf("porch not seen")
	}

	// The full state again, with only the temperature changed.
	publish(t, bus, root+"/1000aa/state", `{"on":true,"temperature":22}`)
	got = rec.takeChanges()
	if len(got) != 1 || got[0].Value != 22.0 || got[0].Previous != 21.5 {
		t.Fatalf("diffed state = %+v", got)
	}

	for _, payload := range []string{
		`{"on":"on"}`,          // invalid_value: wrong kind
		`{"temperature":5000}`, // invalid_value: out of range
		`{"colour.hue":120}`,   // unknown_capability
		`{"switch.on":true}`,   // unknown_capability: an old name has no alias
		`{"brightness":50}`,    // unannounced_capability: porch announced on and temperature
		`not json`,             // malformed
	} {
		publish(t, bus, root+"/1000aa/state", payload)
	}
	publish(t, bus, root+"/1000cc/state", `{"on":true}`) // undeclared_device
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("rejected messages produced changes: %+v", got)
	}
	want := map[string]uint64{
		ReasonInvalidValue: 2, ReasonUnknownCapability: 3, ReasonUnannouncedCapability: 1,
		ReasonMalformed: 1, ReasonUndeclaredDevice: 1,
	}
	stats := i.Stats()
	for reason, n := range want {
		if stats.Rejected[reason] != n {
			t.Errorf("%s = %d, want %d (all: %v)", reason, stats.Rejected[reason], n, stats.Rejected)
		}
	}
}

// A mode is a name on the wire, and the host keeps it as one. A name the registry doesn't have is
// an invalid value, as a number out of range is.
func TestAModeIsANameAndAnUnknownOneIsRefused(t *testing.T) {
	i, bus, rec := setup(t, map[string]string{
		"status":  `{"state":"online","contract":1}`,
		"devices": `{"devices":[{"id":"1000aa","found":true,"capabilities":["on","mode"]}]}`,
	})

	publish(t, bus, root+"/1000aa/state", `{"on":true,"mode":"dry"}`)
	got := rec.takeChanges()
	if len(got) != 2 || got[0].Capability != capability.Mode || got[0].Value != "dry" {
		t.Fatalf("state = %+v, want mode dry", got)
	}

	for _, payload := range []string{`{"on":true,"mode":"frost"}`, `{"on":true,"mode":3}`} {
		publish(t, bus, root+"/1000aa/state", payload)
	}
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("an unknown mode changed the value: %+v", got)
	}
	if got := i.Stats().Rejected[ReasonInvalidValue]; got != 2 {
		t.Errorf("invalid_value = %d, want 2", got)
	}

	if err := i.Set(t.Context(), porch, capability.Mode, "cool"); err != nil {
		t.Fatal(err)
	}
	if got := bus.Sent(root + "/1000aa/set"); !slices.Equal(got, []string{`{"mode":"cool"}`}) {
		t.Errorf("sent = %v", got)
	}
}

func TestRetainedStateBeforeDeclaration(t *testing.T) {
	// The broker replays retained topics in any order, and before the hub declares devices.
	retained := online()
	retained["1000aa/state"] = `{"on":true}`
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
	i.SetDevices(map[string]ident.ID{"1000aa": porch}, nil)
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("re-declaring repeated changes: %+v", got)
	}
}

func TestStateBeforeDevicesIsReplayed(t *testing.T) {
	_, bus, rec := setup(t, map[string]string{"status": `{"state":"online","contract":1}`})
	if len(rec.discovered) != 0 {
		t.Fatalf("devices discovered before the devices topic: %+v", rec.discovered)
	}

	publish(t, bus, root+"/1000aa/state", `{"on":false}`)
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
	if err := i.Set(t.Context(), porch, capability.On, true); err == nil {
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
	publish(t, bus, root+"/1000aa/state", `{"on":true}`)
	publish(t, bus, root+"/devices", devicesJSON)
	if got := rec.takeChanges(); len(got) != 0 || rec.discovered[porch].Found {
		t.Errorf("messages under contract 2 were used: %+v", got)
	}
	if got := i.Stats().Rejected[ReasonWrongContract]; got != 3 {
		t.Errorf("wrong_contract = %d, want 3", got)
	}
}

func TestStatusError(t *testing.T) {
	retained := online()
	retained["1000aa/availability"] = "online"
	i, bus, rec := setup(t, retained)
	var problems []string
	i.OnProblem = func(problem string) { problems = append(problems, problem) }

	// A plugin with a problem of its own is still up, and its devices still count.
	publish(t, bus, root+"/status", `{"state":"online","contract":1,"error":"login refused"}`)
	if st := i.Stats(); !st.Up || i.Problem() != "login refused" || !rec.available[porch] {
		t.Errorf("with an error: %+v %q, porch available %v", st, i.Problem(), rec.available[porch])
	}
	// The Will drops it: a plugin that isn't running has no problem to say.
	publish(t, bus, root+"/status", `{"state":"offline","contract":1}`)
	if got := i.Problem(); got != "" {
		t.Errorf("error kept while offline: %q", got)
	}
	publish(t, bus, root+"/status", `{"state":"online","contract":1,"error":"login refused"}`)
	publish(t, bus, root+"/status", `{"state":"online","contract":1}`)
	if st := i.Stats(); !st.Up || i.Problem() != "" {
		t.Errorf("error not cleared: %+v %q", st, i.Problem())
	}
	// The start and the end were heard once each, for the event log: going down didn't end it, and
	// coming back with it didn't start it again.
	if want := []string{"login refused", ""}; !slices.Equal(problems, want) {
		t.Errorf("OnProblem heard %q, want %q", problems, want)
	}

	// An error page passed on whole is one line, and cut.
	page := "<html>\n<body>\x1b[31mBad gateway</body>" + strings.Repeat("x", 300)
	raw, _ := json.Marshal(contractv1.Status{State: contractv1.Online, Contract: contractv1.Contract, Error: page})
	publish(t, bus, root+"/status", string(raw))
	if got := i.Problem(); strings.ContainsAny(got, "\n\x1b") || utf8.RuneCountInString(got) != maxProblem ||
		!strings.HasPrefix(got, "<html> <body> [31mBad gateway") {
		t.Errorf("long error kept as %q", got)
	}
}

func TestSetPublishes(t *testing.T) {
	i, bus, _ := setup(t, online())
	ctx := t.Context()

	if err := i.Set(ctx, porch, capability.On, false); err != nil {
		t.Fatal(err)
	}
	if got := bus.Sent(root + "/1000aa/set"); !slices.Equal(got, []string{`{"on":false}`}) {
		t.Errorf("sent = %v", got)
	}
	if bus.Published[len(bus.Published)-1].Retained {
		t.Errorf("/set was retained")
	}

	for name, call := range map[string]func() error{
		"unannounced capability": func() error { return i.Set(ctx, porch, capability.Brightness, 50.0) },
		"capability dropped as unknown": func() error {
			return i.Set(ctx, hall, "colour.hue", 1.0)
		},
		"device not found by the plugin": func() error { return i.Set(ctx, ghost, capability.On, true) },
		"undeclared device":              func() error { return i.Set(ctx, dev("other"), capability.On, true) },
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
	ctx := t.Context()
	if err := i.Set(ctx, porch, capability.On, false); err != nil {
		t.Fatal(err)
	}
	i.Forget()
	if err := i.Set(ctx, porch, capability.On, false); err == nil {
		t.Fatal("a write was taken with nothing announced")
	}
	publish(t, bus, root+"/devices", devicesJSON)
	if err := i.Set(ctx, porch, capability.On, false); err != nil {
		t.Fatal(err)
	}
}

func TestStopUnsubscribes(t *testing.T) {
	i, bus, rec := setup(t, online())
	i.Stop()
	publish(t, bus, root+"/1000aa/state", `{"on":true}`)
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("changes after Stop: %+v", got)
	}
}

func TestDiscoveredCountsUndeclared(t *testing.T) {
	retained := online()
	// 1000aa is declared, 1000cc isn't, and the plugin's own count is ignored.
	retained["devices"] = `{"devices":[],"undeclared":7,"discovered":[
		{"id":"1000aa","model":"MINIR4","capabilities":["on"],"name":"Porch light","room":"Porch"},
		{"id":"1000cc","capabilities":["on","colour.hue"],"name":"Desk lamp"},
		{"id":"1000cc"},
		{"id":"bad/id"}
	]}`
	i, bus, rec := setup(t, retained)

	if rec.undeclared != 1 {
		t.Errorf("undeclared = %d, want 1", rec.undeclared)
	}
	got, ok := i.Discovered()
	if !ok || len(got) != 2 || got[0].Name != "Porch light" || got[0].Room != "Porch" ||
		!slices.Equal(got[1].Capabilities, []string{capability.On}) {
		t.Errorf("Discovered() = %+v, %v", got, ok)
	}
	if len(rec.inventory) != 2 || rec.inventory[0].ID != "1000aa" || rec.inventory[0].Name != "Porch light" ||
		rec.inventory[1].ID != "1000cc" {
		t.Errorf("inventory = %+v", rec.inventory)
	}
	if r := i.Stats().Rejected; r[ReasonMalformed] != 2 || r[ReasonUnknownCapability] != 1 {
		t.Errorf("rejected = %v, want 2 malformed and 1 unknown_capability", r)
	}

	// Removing it is having it on record too.
	i.SetDevices(map[string]ident.ID{"1000aa": porch}, map[string]bool{"1000cc": true})
	if rec.undeclared != 0 {
		t.Errorf("undeclared after removing = %d, want 0", rec.undeclared)
	}

	// Declaring 1000cc leaves nothing undeclared.
	i.SetDevices(map[string]ident.ID{"1000aa": porch, "1000cc": hall}, nil)
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

func TestADeviceTypeIsTakenAndAnUnknownOneRejected(t *testing.T) {
	retained := online()
	retained["devices"] = `{"devices":[
		{"id":"1000aa","found":true,"capabilities":["on","brightness"],"deviceType":"DimmableLight"},
		{"id":"1000bb","found":true,"capabilities":["on"],"deviceType":"Toaster"},
		{"id":"1000ff","found":true,"capabilities":["on"]}
	],"discovered":[
		{"id":"1000aa","capabilities":["on","brightness"],"deviceType":"DimmableLight"},
		{"id":"1000cc","capabilities":["on"],"deviceType":"lamp"}
	]}`
	i, _, rec := setup(t, retained)

	if got := rec.discovered[porch]; got.DeviceType != devicetype.DimmableLight {
		t.Errorf("porch = %+v, want DimmableLight", got)
	}
	if got := rec.discovered[hall]; !got.Found || got.DeviceType != "" || !slices.Equal(got.Capabilities, []string{capability.On}) {
		t.Errorf("hall = %+v, want found with no device type", got)
	}
	if got := rec.discovered[ghost]; !got.Found || got.DeviceType != "" {
		t.Errorf("a device with no deviceType = %+v", got)
	}
	if len(rec.inventory) != 2 || rec.inventory[0].DeviceType != devicetype.DimmableLight || rec.inventory[1].DeviceType != "" {
		t.Errorf("inventory = %+v", rec.inventory)
	}
	if got := i.Stats().Rejected[ReasonUnknownDeviceType]; got != 2 {
		t.Errorf("unknown_device_type = %d, want 2: %v", got, i.Stats().Rejected)
	}
}

// Its lines name the plugin by name in the host's JSON log, as a filter on it expects (OIK-330).
func TestThePluginsLinesNameIt(t *testing.T) {
	var out lockedBuffer
	_, bus, _ := setupLogged(t, online(), slog.New(slog.NewJSONHandler(&out, nil)))
	publish(t, bus, root+"/status", `{"state":"offline","contract":1}`)
	for _, msg := range []string{"Plugin came online", "Plugin went offline"} {
		if !strings.Contains(out.String(), `"msg":"`+msg+`","plugin":"sonoff"`) {
			t.Errorf("no %q with plugin sonoff in:\n%s", msg, out.String())
		}
	}
}

// lockedBuffer is a log's output that any goroutine can write while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// What a device is comes through from its report and from the discovered list.
func TestWhatADeviceIsComesThrough(t *testing.T) {
	retained := online()
	retained["devices"] = `{"devices":[
		{"id":"1000aa","found":true,"manufacturer":"SONOFF","model":"MINIR4","serialNumber":"A1B2","firmwareVersion":"3.1.0","hardwareVersion":"2","capabilities":["on"]}
	],"discovered":[
		{"id":"1000cc","manufacturer":"Nanoleaf","model":"NL45","serialNumber":"N9","firmwareVersion":"4.0","hardwareVersion":"B","capabilities":["on"]}
	]}`
	i, _, rec := setup(t, retained)
	want := driver.Product{Manufacturer: "SONOFF", Model: "MINIR4", SerialNumber: "A1B2", FirmwareVersion: "3.1.0", HardwareVersion: "2"}
	if got := i.Announced()["1000aa"].Product; got != want {
		t.Errorf("the report's product is %+v, want %+v", got, want)
	}
	want = driver.Product{Manufacturer: "Nanoleaf", Model: "NL45", SerialNumber: "N9", FirmwareVersion: "4.0", HardwareVersion: "B"}
	if len(rec.inventory) != 1 || rec.inventory[0].Product != want {
		t.Errorf("the discovered list's inventory is %+v, want %+v", rec.inventory, want)
	}
}

// A report's field that's only space is one it leaves out, so the discovered list's fills it.
func TestAFieldThatsOnlySpaceIsLeftOut(t *testing.T) {
	got := ReportProduct(contractv1.DeviceReport{Manufacturer: " Aqara\n", SerialNumber: " \t"})
	if want := (driver.Product{Manufacturer: "Aqara"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if merged := got.Or(DiscoveredProduct(contractv1.DiscoveredDevice{SerialNumber: "N9"})); merged.SerialNumber != "N9" {
		t.Errorf("the discovered list's serial number didn't fill a blank one: %+v", merged)
	}
}
