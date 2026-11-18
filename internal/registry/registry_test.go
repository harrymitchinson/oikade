package registry

import (
	"bytes"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/store"
)

func open(t *testing.T) (*Registry, *store.Namespace) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	values := s.Namespace(Namespace)
	r, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	return r, values
}

func report(t *testing.T, r *Registry, id, name string) Device {
	t.Helper()
	d, _, err := r.Report(Report{ID: id, Driver: "sim", Name: name, Capabilities: []string{"on"}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func get(t *testing.T, r *Registry, name string) Device {
	t.Helper()
	d, ok := r.Get(name)
	if !ok {
		t.Fatalf("no device %s in %+v", name, r.List())
	}
	return d
}

func TestAPairedDeviceEntersOnceAndKeepsItsName(t *testing.T) {
	r, _ := open(t)
	d, changed, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Hall Lamp!", Room: "Living Room", Capabilities: []string{"on"}})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if _, changed, _ := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Hall Lamp!", Capabilities: []string{"on"}}); changed {
		t.Error("the same report changed something")
	}
	if d.Name != "hall-lamp" || d.Room != "living-room" || d.AppRoom != "Living Room" || d.Endpoint != FirstEndpoint {
		t.Errorf("got %+v", d)
	}
	declareOK(t, r, Declaration{Name: "lamp", Driver: "sim", ID: "0x1", Room: "attic"})
	again, _, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Renamed In The App", Room: "Elsewhere", Capabilities: []string{"on", "brightness"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != "lamp" || again.Room != "attic" || again.Endpoint != d.Endpoint || len(again.Capabilities) != 2 {
		t.Errorf("an app's names were followed, or capabilities weren't: %+v", again)
	}
}

func TestNamesFromAppsAreUniqueAndValid(t *testing.T) {
	r, _ := open(t)
	if a, b := report(t, r, "1", "Lamp"), report(t, r, "2", "lamp"); a.Name != "lamp" || b.Name != "lamp-2" {
		t.Errorf("got %s and %s", a.Name, b.Name)
	}
	for name, id := range map[string]string{"": "0x00124B", "2nd floor": "x", "---": "", "Ünï": "y"} {
		if err := checkName(Slug(name, id)); err != nil {
			t.Errorf("Slug(%q, %q) = %q: %v", name, id, Slug(name, id), err)
		}
	}
	if err := checkName(Slug(strings.Repeat("ab-", 40), "x")); err != nil {
		t.Error(err)
	}
}

func TestEndpointsAreNeverReused(t *testing.T) {
	r, _ := open(t)
	lamp, fan := report(t, r, "lamp", "lamp"), report(t, r, "fan", "fan")
	if err := r.Remove("lamp"); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("fan"); err != nil {
		t.Fatal(err)
	}
	declareOK(t, r, Declaration{Name: "lamp"})
	if got := get(t, r, "lamp").Endpoint; got == lamp.Endpoint || got == fan.Endpoint {
		t.Errorf("a new device took endpoint %d", got)
	}
	other := report(t, r, "other", "other")
	if other.Endpoint == lamp.Endpoint || other.Endpoint == fan.Endpoint {
		t.Errorf("new hardware took endpoint %d", other.Endpoint)
	}

	back := report(t, r, "fan", "ignored")
	if back.Endpoint != fan.Endpoint || back.Name != "fan" {
		t.Errorf("removed hardware came back as %+v, want %+v", back, fan)
	}
	again := report(t, r, "lamp", "ignored")
	if again.Endpoint != lamp.Endpoint || again.Name != "lamp-2" {
		t.Errorf("removed hardware whose name went came back as %+v", again)
	}
	seen := map[uint16]bool{}
	list := r.List()
	for _, d := range list.Devices {
		if seen[d.Endpoint] || d.Endpoint >= list.Next {
			t.Errorf("endpoint %d reused or past next in %+v", d.Endpoint, list)
		}
		seen[d.Endpoint] = true
	}
}

func TestTheRegistrySurvivesReopening(t *testing.T) {
	r, values := open(t)
	report(t, r, "lamp", "lamp")
	if err := r.Remove("lamp"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if d := report(t, reopened, "fan", "fan"); d.Endpoint != FirstEndpoint+1 {
		t.Errorf("the counter was lost: %+v", d)
	}
	if d := report(t, reopened, "lamp", "lamp"); d.Endpoint != FirstEndpoint {
		t.Errorf("the removed device was lost: %+v", d)
	}
}

func TestAnAppsRoomSavedBeforeRoomsHadNamesIsMadeIntoOne(t *testing.T) {
	_, values := open(t)
	raw := `{"next":4,"devices":[` +
		`{"name":"lamp","id":"1","driver":"sim","room":"Living Room","appRoom":"Living Room","endpoint":2},` +
		`{"name":"fan","id":"2","driver":"sim","room":"hall","declared":true,"endpoint":3}]}`
	if err := values.Put(stateKey, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	r, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if d := get(t, r, "lamp"); d.Room != "living-room" || d.AppRoom != "Living Room" {
		t.Errorf("got %+v", d)
	}
	if d := get(t, r, "fan"); d.Room != "hall" {
		t.Errorf("got %+v", d)
	}
}

func TestAnUnreadableRegistryIsAnError(t *testing.T) {
	_, values := open(t)
	for _, raw := range []string{"not json", `{"next":0,"devices":[]}`} {
		if err := values.Put(stateKey, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(values); err == nil {
			t.Errorf("%s opened as a registry", raw)
		}
	}
}

func TestAFailedSaveChangesNothing(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(s.Namespace(Namespace))
	if err != nil {
		t.Fatal(err)
	}
	report(t, r, "lamp", "lamp")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Declare([]Declaration{{Name: "fan"}}); err == nil {
		t.Fatal("saved to a closed store")
	}
	if list := r.List(); list.Next != FirstEndpoint+1 || len(list.Devices) != 1 {
		t.Errorf("took up a change it couldn't save: %+v", list)
	}
}

func TestRenderIsDeterministicAndValidGo(t *testing.T) {
	r, _ := open(t)
	report(t, r, "z", "zebra-lamp")
	report(t, r, "a", "apple-lamp")
	if _, _, err := r.Report(Report{ID: "p", Driver: "sim", Name: "Side\nconst Evil = 1\n", Room: "Front\nconst Evil = 1\n"}); err != nil {
		t.Fatal(err)
	}
	declareOK(t, r, Declaration{Name: "porch", Room: "front"})
	first, err := Render(r.List(), "home")
	if err != nil {
		t.Fatal(err)
	}
	shuffled := r.List()
	slices.Reverse(shuffled.Devices)
	second, err := Render(shuffled, "home")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("two renders differ:\n%s\n%s", first, second)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "devices.go", first, parser.ParseComments)
	if err != nil {
		t.Fatalf("%v\n%s", err, first)
	}
	if f.Name.Name != "home" {
		t.Errorf("package %s", f.Name.Name)
	}
	if strings.Contains(string(first), "\nconst Evil") {
		t.Errorf("a room wrote code:\n%s", first)
	}
	for _, want := range []string{
		"var AppleLamp = struct", `Name: "apple-lamp"`, "var ZebraLamp = struct",
		`On:     oikade.Bool{Device: "zebra-lamp", Capability: oikade.On}`,
		"var Porch = struct {\n\toikade.Device\n}{\n\tDevice: oikade.Device{Name: \"porch\"},\n}", "reserved",
	} {
		if !strings.Contains(string(first), want) {
			t.Errorf("no %q in:\n%s", want, first)
		}
	}
}

func TestRenderCleansWhatAnAppReports(t *testing.T) {
	file, err := Render(Snapshot{Next: 3, Devices: []Device{{
		Name: "lamp", Room: "Hall\nconst Evil = 1", Capabilities: []string{"a\nvar X = 2"}, Endpoint: 2,
	}}}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(file), "\nconst Evil") || strings.Contains(string(file), "\nvar X") {
		t.Errorf("wrote code:\n%s", file)
	}
}

func TestRenderLeavesOutRemovedDevices(t *testing.T) {
	r, _ := open(t)
	report(t, r, "a", "lamp")
	if err := r.Remove("lamp"); err != nil {
		t.Fatal(err)
	}
	file, err := Render(r.List(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(file), "var Lamp") || strings.Contains(string(file), "import") {
		t.Errorf("the device stayed:\n%s", file)
	}
}

func TestHandlesReadsBackWhatRenderWrote(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "lamp")
	declareOK(t, r, Declaration{Name: "shed-fan"})
	file, err := Render(r.List(), "home")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(file), "//oikade:") {
		t.Errorf("registry lines stayed:\n%s", file)
	}
	got, err := Handles(file)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got["lamp"], []string{"on"}) || got["shed-fan"] == nil || len(got) != 2 {
		t.Errorf("read back %v", got)
	}
	if _, err := Handles([]byte("not go")); err == nil {
		t.Error("read a file that isn't Go")
	}
}

func TestRenderRefusesNamesThatMeetInGo(t *testing.T) {
	r, _ := open(t)
	declareOK(t, r, Declaration{Name: "a-1"}, Declaration{Name: "a1"})
	if _, err := Render(r.List(), "main"); err == nil || !strings.Contains(err.Error(), "A1") {
		t.Errorf("got %v", err)
	}
}

func TestChecksOnNames(t *testing.T) {
	r, _ := open(t)
	report(t, r, "a", "lamp")
	for _, name := range []string{"", "Lamp", "9lamp", "la mp", "lamp-", "la--mp", strings.Repeat("a", 64)} {
		if _, err := r.Declare([]Declaration{{Name: name}}); err == nil {
			t.Errorf("reserved %q", name)
		}
	}
	if _, err := r.Declare([]Declaration{{Name: "lamp"}}); err == nil {
		t.Error("reserved a name in use")
	}
}

func TestEndpointsRunOut(t *testing.T) {
	s := Snapshot{Next: LastEndpoint}
	if got, err := allocate(&s); err != nil || got != LastEndpoint {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := allocate(&s); err == nil {
		t.Error("handed out an endpoint Matter doesn't have")
	}
}

func TestArbitrationStaysOnRecord(t *testing.T) {
	r, values := open(t)
	settings := []byte(`[{"capability":"on"}]`)
	if _, _, err := r.Report(Report{ID: "a", Driver: "sim", Name: "valve", Control: settings}); err != nil {
		t.Fatal(err)
	}
	if _, changed, _ := r.Report(Report{ID: "a", Driver: "sim", Name: "valve"}); changed {
		t.Error("a report without settings changed something")
	}
	reopened, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := reopened.Get("valve"); d.ID != "a" || !bytes.Equal(d.Control, settings) {
		t.Errorf("got %+v", d)
	}
}

func TestADeviceTypeIsYoursOverItsDriversAndSurvives(t *testing.T) {
	r, values := open(t)
	report := func(typ devicetype.Type, caps ...string) Device {
		t.Helper()
		d, _, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "bulb", Capabilities: caps, DeviceType: typ})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	types := func() []devicetype.Type { return get(t, r, "bulb").DeviceTypes() }

	report("", capability.On)
	if got := types(); !slices.Equal(got, []devicetype.Type{devicetype.OnOffPlugInUnit}) {
		t.Errorf("an undescribed on device is %v", got)
	}
	if _, changed, _ := r.Report(Report{ID: "0x1", Driver: "sim", Capabilities: []string{capability.On}, DeviceType: devicetype.OnOffLight}); !changed {
		t.Error("the driver's new device type wasn't taken up")
	}
	if got := types(); !slices.Equal(got, []devicetype.Type{devicetype.OnOffLight}) {
		t.Errorf("with the driver's type it's %v", got)
	}
	declareOK(t, r, Declaration{Name: "bulb", Driver: "sim", ID: "0x1", DeviceType: devicetype.MountedOnOffControl})
	report(devicetype.OnOffLight, capability.On)
	if got := types(); !slices.Equal(got, []devicetype.Type{devicetype.MountedOnOffControl}) {
		t.Errorf("yours didn't win: %v", got)
	}

	again, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := again.Get("bulb"); d.DeviceType != devicetype.MountedOnOffControl || d.DriverDeviceType != devicetype.OnOffLight {
		t.Errorf("after reopening: %+v", d)
	}

	declareOK(t, r) // No longer declared.
	if got := types(); !slices.Equal(got, []devicetype.Type{devicetype.OnOffLight}) {
		t.Errorf("going back gave %v, want the driver's", got)
	}
}

func TestWhatADeviceIsStaysOnRecordUntilItsDriverSaysOtherwise(t *testing.T) {
	r, values := open(t)
	on := []string{capability.On}
	reported := func(manufacturer, model string) (Device, bool) {
		t.Helper()
		d, changed, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "lamp", Capabilities: on, Manufacturer: manufacturer, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		return d, changed
	}
	reported("", "")
	if d, changed := reported(" IKEA of Sweden\n", "TRADFRI bulb E27"); !changed || d.Manufacturer != "IKEA of Sweden" || d.Model != "TRADFRI bulb E27" {
		t.Errorf("a first manufacturer and model gave %+v, changed %v", d, changed)
	}
	if d, changed := reported("", ""); changed || d.Manufacturer != "IKEA of Sweden" || d.Model != "TRADFRI bulb E27" {
		t.Errorf("a report that says nothing of them gave %+v, changed %v", d, changed)
	}
	if _, changed := reported("IKEA of Sweden", "TRADFRI bulb E27"); changed {
		t.Error("the same manufacturer and model changed something")
	}
	if d, changed := reported("", "TRADFRI bulb E27 WS"); !changed || d.Manufacturer != "IKEA of Sweden" || d.Model != "TRADFRI bulb E27 WS" {
		t.Errorf("a new model alone gave %+v, changed %v", d, changed)
	}
	again, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := again.Get("lamp"); d.Manufacturer != "IKEA of Sweden" || d.Model != "TRADFRI bulb E27 WS" {
		t.Errorf("after reopening: %+v", d)
	}
}

func TestWhatADeviceIsMovesWithItsHardware(t *testing.T) {
	r, _ := open(t)
	if _, _, err := r.Report(Report{ID: "0x2", Driver: "sim", Name: "Sensor 7", Manufacturer: "Aqara", Model: "WSDCGQ11LM"}); err != nil {
		t.Fatal(err)
	}
	declareOK(t, r, Declaration{Name: "hall-sensor", Room: "hall"})
	declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x2", Room: "hall"})
	if d := get(t, r, "hall-sensor"); d.Manufacturer != "Aqara" || d.Model != "WSDCGQ11LM" {
		t.Errorf("the name took the hardware without what it is: %+v", d)
	}
	declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x3", Room: "hall"})
	if d := get(t, r, "hall-sensor"); d.Manufacturer != "" || d.Model != "" {
		t.Errorf("hardware not on record kept the last one's manufacturer and model: %+v", d)
	}
}
