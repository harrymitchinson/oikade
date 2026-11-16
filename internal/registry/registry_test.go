package registry

import (
	"bytes"
	"errors"
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
	d, changed, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Hall Lamp!", Room: "Hall", Capabilities: []string{"on"}})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if _, changed, _ := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Hall Lamp!", Capabilities: []string{"on"}}); changed {
		t.Error("the same report changed something")
	}
	if d.Name != "hall-lamp" || d.Room != "Hall" || d.Endpoint != FirstEndpoint {
		t.Errorf("got %+v", d)
	}
	if err := r.Rename("hall-lamp", "lamp"); err != nil {
		t.Fatal(err)
	}
	if err := r.Move("lamp", "Attic"); err != nil {
		t.Fatal(err)
	}
	again, _, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Renamed In The App", Room: "Elsewhere", Capabilities: []string{"on", "brightness"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != "lamp" || again.Room != "Attic" || again.Endpoint != d.Endpoint || len(again.Capabilities) != 2 {
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
	if err := r.Reserve("lamp", ""); err != nil {
		t.Fatal(err)
	}
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
	if err := r.Reserve("fan", ""); err == nil {
		t.Fatal("saved to a closed store")
	}
	if list := r.List(); list.Next != FirstEndpoint+1 || len(list.Devices) != 1 {
		t.Errorf("took up a change it couldn't save: %+v", list)
	}
}

func TestReplacingKeepsTheNameAndEndpoint(t *testing.T) {
	r, _ := open(t)
	old := report(t, r, "old", "lamp")
	fresh := report(t, r, "new", "new-lamp")
	if err := r.Replace("lamp", "new-lamp"); err != nil {
		t.Fatal(err)
	}
	d := get(t, r, "lamp")
	if d.ID != "new" || d.Endpoint != old.Endpoint {
		t.Errorf("got %+v", d)
	}
	if _, ok := r.Get("new-lamp"); ok {
		t.Error("the replacement's name stayed")
	}
	if byID, ok := r.ByID("sim", "new"); !ok || byID.Name != "lamp" {
		t.Errorf("its hardware is %+v, %v", byID, ok)
	}
	retired := slices.IndexFunc(r.List().Devices, func(d Device) bool { return d.Endpoint == fresh.Endpoint })
	if retired < 0 || !r.List().Devices[retired].Removed {
		t.Errorf("the replacement's endpoint wasn't kept: %+v", r.List())
	}
	if again := report(t, r, "new", "ignored"); again.Name != "lamp" || again.Endpoint != old.Endpoint {
		t.Errorf("the next report moved it: %+v", again)
	}
	back := report(t, r, "old", "old-lamp")
	if back.Endpoint == old.Endpoint || back.Endpoint == fresh.Endpoint {
		t.Errorf("the old hardware took endpoint %d", back.Endpoint)
	}

	if err := r.Reserve("porch", "Porch"); err != nil {
		t.Fatal(err)
	}
	reserved := get(t, r, "porch")
	if !reserved.Reserved() {
		t.Errorf("not reserved: %+v", reserved)
	}
	report(t, r, "paired", "paired")
	if err := r.Replace("porch", "paired"); err != nil {
		t.Fatal(err)
	}
	if d := get(t, r, "porch"); d.ID != "paired" || d.Endpoint != reserved.Endpoint || d.Room != "Porch" || d.Reserved() {
		t.Errorf("a reserved name's hardware: %+v", d)
	}
	if err := r.Replace("porch", "porch"); err == nil {
		t.Error("replaced a device with itself")
	}
	if err := r.Reserve("empty", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace("lamp", "empty"); err == nil {
		t.Error("moved onto no hardware")
	}
	if err := r.Replace("nothing", "lamp"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("got %v", err)
	}
}

func TestRenderIsDeterministicAndValidGo(t *testing.T) {
	r, _ := open(t)
	report(t, r, "z", "zebra-lamp")
	report(t, r, "a", "apple-lamp")
	if err := r.Reserve("porch", "Front\nconst Evil = 1\n"); err != nil {
		t.Fatal(err)
	}
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
	if err := r.Reserve("shed-fan", ""); err != nil {
		t.Fatal(err)
	}
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
	for _, name := range []string{"a-1", "a1"} {
		if err := r.Reserve(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Render(r.List(), "main"); err == nil || !strings.Contains(err.Error(), "A1") {
		t.Errorf("got %v", err)
	}
}

func TestChecksOnNames(t *testing.T) {
	r, _ := open(t)
	report(t, r, "a", "lamp")
	for _, name := range []string{"", "Lamp", "9lamp", "la mp", "lamp-", "la--mp", strings.Repeat("a", 64)} {
		if err := r.Reserve(name, ""); err == nil {
			t.Errorf("reserved %q", name)
		}
	}
	if err := r.Reserve("lamp", ""); err == nil {
		t.Error("reserved a name in use")
	}
	if err := r.Reserve("fan", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Rename("lamp", "fan"); err == nil {
		t.Error("renamed onto a name in use")
	}
	if err := r.Rename("nothing", "x"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("got %v", err)
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

func TestArbitrationStaysOnRecordAndWithTheName(t *testing.T) {
	r, values := open(t)
	settings := []byte(`[{"capability":"on"}]`)
	if _, _, err := r.Report(Report{ID: "a", Driver: "sim", Name: "valve", Control: settings}); err != nil {
		t.Fatal(err)
	}
	if _, changed, _ := r.Report(Report{ID: "a", Driver: "sim", Name: "valve"}); changed {
		t.Error("a report without settings changed something")
	}
	report(t, r, "b", "spare")
	if err := r.Replace("valve", "spare"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := reopened.Get("valve"); d.ID != "b" || !bytes.Equal(d.Control, settings) {
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
	if err := r.SetDeviceType("bulb", devicetype.MountedOnOffControl); err != nil {
		t.Fatal(err)
	}
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

	if err := r.SetDeviceType("bulb", ""); err != nil {
		t.Fatal(err)
	}
	if got := types(); !slices.Equal(got, []devicetype.Type{devicetype.OnOffLight}) {
		t.Errorf("going back gave %v, want the driver's", got)
	}
}

func TestSetDeviceTypeRefusesWhatCantBe(t *testing.T) {
	r, _ := open(t)
	if _, _, err := r.Report(Report{ID: "1", Driver: "sim", Name: "plug", Capabilities: []string{capability.On}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Report(Report{ID: "2", Driver: "sim", Name: "door", Capabilities: []string{capability.Open}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Report(Report{ID: "3", Driver: "sim", Name: "heater", Capabilities: []string{capability.On, capability.Setpoint}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("future", ""); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		device string
		typ    devicetype.Type
	}{
		"not a type":           {"plug", "Toaster"},
		"a sensor's type":      {"plug", devicetype.ContactSensor},
		"dimmable, no dimming": {"plug", devicetype.DimmableLight},
		"not on or off":        {"door", devicetype.OnOffLight},
		"a thermostat":         {"heater", devicetype.OnOffPlugInUnit},
		"no such device":       {"nothing", devicetype.OnOffLight},
	} {
		if err := r.SetDeviceType(tc.device, tc.typ); err == nil {
			t.Errorf("%s: set %s on %s", name, tc.typ, tc.device)
		}
	}
	if err := r.SetDeviceType("future", devicetype.DimmableLight); err != nil {
		t.Errorf("a reserved name, whose capabilities aren't known yet: %v", err)
	}
}

func TestADeviceTypeStaysWithItsNameThroughAReplace(t *testing.T) {
	r, _ := open(t)
	report(t, r, "old", "porch-light")
	if _, _, err := r.Report(Report{ID: "new", Driver: "sim", Name: "lamp-3", Capabilities: []string{capability.On},
		DeviceType: devicetype.OnOffLight}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDeviceType("lamp-3", devicetype.MountedOnOffControl); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace("porch-light", "lamp-3"); err != nil {
		t.Fatal(err)
	}
	d := get(t, r, "porch-light")
	if d.DeviceType != "" || d.DriverDeviceType != devicetype.OnOffLight {
		t.Errorf("after the replace: %+v, want no type of yours and the new hardware's driver's", d)
	}
}
