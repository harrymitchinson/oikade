package registry

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/store"
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

func get(t *testing.T, r *Registry, key string) Device {
	t.Helper()
	d, ok := r.ByKey(key)
	if !ok {
		t.Fatalf("no device %s in %+v", key, r.List())
	}
	return d
}

func TestAPairedDeviceEntersOnceAndKeepsItsAppsName(t *testing.T) {
	r, _ := open(t)
	d, changed, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Hall Lamp!", Room: "Living Room", Capabilities: []string{"on"}})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if _, changed, _ := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Hall Lamp!", Capabilities: []string{"on"}}); changed {
		t.Error("the same report changed something")
	}
	if d.Key() != "sim/0x1" || d.AppName != "Hall Lamp!" || d.Room != "living-room" || d.AppRoom != "Living Room" ||
		d.Endpoint != FirstEndpoint {
		t.Errorf("got %+v", d)
	}
	declareOK(t, r, Declaration{Hardware: "sim/0x1", Room: "attic"})
	again, _, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Renamed In The App", Room: "Elsewhere", Capabilities: []string{"on", "brightness"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.AppName != "Hall Lamp!" || again.Room != "attic" || again.Endpoint != d.Endpoint || len(again.Capabilities) != 2 {
		t.Errorf("an app's names were followed, or capabilities weren't: %+v", again)
	}
}

func TestEndpointsAreNeverReused(t *testing.T) {
	r, _ := open(t)
	lamp, fan := report(t, r, "lamp", "lamp"), report(t, r, "fan", "fan")
	if err := r.Remove(lamp.Key()); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(fan.Key()); err != nil {
		t.Fatal(err)
	}
	declareOK(t, r, Declaration{Hardware: "hue/0x9", Driver: "plugin/hue", ID: "0x9"})
	if got := get(t, r, "hue/0x9").Endpoint; got == lamp.Endpoint || got == fan.Endpoint {
		t.Errorf("a new device took endpoint %d", got)
	}
	other := report(t, r, "other", "other")
	if other.Endpoint == lamp.Endpoint || other.Endpoint == fan.Endpoint {
		t.Errorf("new hardware took endpoint %d", other.Endpoint)
	}

	if back := report(t, r, "fan", "ignored"); back.Endpoint != fan.Endpoint || back.AppName != "fan" {
		t.Errorf("removed hardware came back as %+v, want %+v", back, fan)
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
	if err := r.Remove(report(t, r, "lamp", "lamp").Key()); err != nil {
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
	if d := get(t, r, "sim/1"); d.Room != "living-room" || d.AppRoom != "Living Room" {
		t.Errorf("got %+v", d)
	}
	if d := get(t, r, "sim/2"); d.Room != "hall" {
		t.Errorf("got %+v", d)
	}
}

// A name reserved for hardware before a device was its hardware has none, so it goes, and its
// endpoint isn't handed out again.
func TestAReservedNameSavedBeforeDevicesWereTheirHardwareGoes(t *testing.T) {
	_, values := open(t)
	raw := `{"next":4,"devices":[{"name":"greenhouse-fan","declared":true,"endpoint":2},` +
		`{"name":"lamp","id":"1","driver":"sim","endpoint":3}]}`
	if err := values.Put(stateKey, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	r, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if list := r.List(); len(list.Devices) != 1 || list.Devices[0].Key() != "sim/1" || list.Next != 4 {
		t.Errorf("got %+v", list)
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
	if _, err := r.Declare([]Declaration{{Hardware: "hue/0x9", Driver: "plugin/hue", ID: "0x9"}}); err == nil {
		t.Fatal("saved to a closed store")
	}
	if list := r.List(); list.Next != FirstEndpoint+1 || len(list.Devices) != 1 {
		t.Errorf("took up a change it couldn't save: %+v", list)
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
	if d, _ := reopened.ByKey("sim/a"); d.ID != "a" || !bytes.Equal(d.Control, settings) {
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
	types := func() []devicetype.Type { return get(t, r, "sim/0x1").DeviceTypes() }

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
	declareOK(t, r, Declaration{Hardware: "sim/0x1", DeviceType: devicetype.MountedOnOffControl})
	report(devicetype.OnOffLight, capability.On)
	if got := types(); !slices.Equal(got, []devicetype.Type{devicetype.MountedOnOffControl}) {
		t.Errorf("yours didn't win: %v", got)
	}

	again, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := again.ByKey("sim/0x1"); d.DeviceType != devicetype.MountedOnOffControl || d.DriverDeviceType != devicetype.OnOffLight {
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
	if d, _ := again.ByKey("sim/0x1"); d.Manufacturer != "IKEA of Sweden" || d.Model != "TRADFRI bulb E27 WS" {
		t.Errorf("after reopening: %+v", d)
	}
}

func TestAFirmwareUpdateIsTakenUpAndTheRestKept(t *testing.T) {
	r, _ := open(t)
	first := driver.Product{Manufacturer: "Bosch", Model: "BTH-RA", SerialNumber: "18FC26FFFE0A1B2C", FirmwareVersion: "3.05.09", HardwareVersion: "1"}
	if _, _, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "radiator", Product: first}); err != nil {
		t.Fatal(err)
	}
	d, changed, err := r.Report(Report{ID: "0x1", Driver: "sim", FirmwareVersion: "3.05.10\n"})
	if err != nil {
		t.Fatal(err)
	}
	want := first
	want.FirmwareVersion = "3.05.10"
	if !changed || d.Product != want {
		t.Errorf("got %+v, changed %v, want %+v", d.Product, changed, want)
	}
}

// A device is its hardware: a plugin's ID under the plugin's name, percent-encoded where it isn't
// safe in a ref, and with no space, so a case's key can hold it.
func TestADevicesKeyIsItsHardware(t *testing.T) {
	for _, tc := range []struct {
		d    Device
		want string
	}{
		{Device{Driver: "plugin/hue", ID: "0x00178801091863f2"}, "hue/0x00178801091863f2"},
		{Device{Driver: "plugin/overkiz", ID: "io://1234-5678/9 0"}, "overkiz/io:%2F%2F1234-5678%2F9%200"},
		{Device{Driver: "simulated/simulator", ID: "study-heater"}, "simulator/study-heater"},
	} {
		if got := tc.d.Key(); got != tc.want || strings.Contains(got, " ") {
			t.Errorf("%s's key is %q, want %q", tc.d.ID, got, tc.want)
		}
	}
}
