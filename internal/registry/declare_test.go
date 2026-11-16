package registry

import (
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/devicetype"
)

func declareOK(t *testing.T, r *Registry, decls ...Declaration) Declared {
	t.Helper()
	got, err := r.Declare(decls)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDeclaringADeviceNamesPlacesAndTypesItByItsHardware(t *testing.T) {
	r, values := open(t)
	before := report(t, r, "0x1", "Living room lamp")
	got := declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: "Study", DeviceType: devicetype.OnOffLight})
	d := get(t, r, "reading-lamp")
	if !d.Declared || d.Room != "Study" || d.DeviceType != devicetype.OnOffLight || d.Endpoint != before.Endpoint {
		t.Errorf("got %+v, want it declared, in the study, a light, on its endpoint", d)
	}
	if got.Renamed["living-room-lamp"] != "reading-lamp" {
		t.Errorf("renamed %v", got.Renamed)
	}
	want := []string{"Renamed living-room-lamp to reading-lamp", "Moved reading-lamp to Study", "Set reading-lamp's device type to OnOffLight"}
	if !slices.Equal(got.Changes, want) {
		t.Errorf("changes %q, want %q", got.Changes, want)
	}
	again, err := Open(values)
	if err != nil || !get(t, again, "reading-lamp").Declared {
		t.Fatalf("declaration not saved: %v", err)
	}
	if same := declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: "Study", DeviceType: devicetype.OnOffLight}); len(same.Changes) != 0 || len(same.Renamed) != 0 {
		t.Errorf("declaring it again changed %+v", same)
	}
}

func TestADeviceNoLongerDeclaredGoesBackToItsAppsName(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Living room lamp")
	declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: "Study"}, Declaration{Name: "shed-fan"})
	got := declareOK(t, r)
	d := get(t, r, "living-room-lamp")
	if d.Declared || d.Room != "" || got.Renamed["reading-lamp"] != "living-room-lamp" {
		t.Errorf("got %+v, renamed %v", d, got.Renamed)
	}
	if _, ok := r.Get("shed-fan"); ok {
		t.Error("the reserved name stays")
	}
	for _, want := range []string{"Gave reading-lamp back its app's name, living-room-lamp", "Removed shed-fan"} {
		if !slices.ContainsFunc(got.Changes, func(c string) bool { return strings.HasPrefix(c, want) }) {
			t.Errorf("no %q in %q", want, got.Changes)
		}
	}
}

func TestADeclaredNameWaitsForItsHardware(t *testing.T) {
	r, _ := open(t)
	got := declareOK(t, r, Declaration{Name: "porch-light", Driver: "sim", ID: "0x9"}, Declaration{Name: "greenhouse-fan", Room: "Greenhouse"})
	if !slices.Equal(got.Unseen, []string{"porch-light"}) {
		t.Errorf("unseen %v", got.Unseen)
	}
	if !get(t, r, "greenhouse-fan").Reserved() {
		t.Error("greenhouse-fan isn't reserved")
	}
	declared := get(t, r, "porch-light")
	d := report(t, r, "0x9", "Hue lamp 3")
	if d.Name != "porch-light" || d.Endpoint != declared.Endpoint || len(d.Capabilities) == 0 {
		t.Errorf("the hardware came in as %+v, want porch-light on endpoint %d", d, declared.Endpoint)
	}
}

func TestADeclaredNameMovesOntoNewHardwareAndKeepsItsEndpoint(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Hall sensor")
	declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x1", Room: "Hall"})
	old := get(t, r, "hall-sensor")
	report(t, r, "0x2", "Sensor 7")
	got := declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x2", Room: "Hall"})
	d := get(t, r, "hall-sensor")
	if d.ID != "0x2" || d.Endpoint != old.Endpoint || d.Room != "Hall" {
		t.Errorf("got %+v, want 0x2 on endpoint %d", d, old.Endpoint)
	}
	if got.Absorbed["sensor-7"] != "hall-sensor" {
		t.Errorf("absorbed %v, want sensor-7's values to come across", got.Absorbed)
	}
	if _, ok := r.ByID("sim", "0x1"); ok {
		t.Error("the old hardware is still on record")
	}
	if _, ok := r.Get("sensor-7"); ok {
		t.Error("the new hardware's own entry stays")
	}
}

func TestDeclarationsDontDependOnTheirOrder(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		r, _ := open(t)
		report(t, r, "0x1", "Lamp")
		report(t, r, "0x2", "Other")
		decls := []Declaration{{Name: "old-lamp", Driver: "sim", ID: "0x1"}, {Name: "lamp", Driver: "sim", ID: "0x2"}}
		if reversed {
			slices.Reverse(decls)
		}
		declareOK(t, r, decls...)
		if get(t, r, "lamp").ID != "0x2" || get(t, r, "old-lamp").ID != "0x1" {
			t.Errorf("reversed %v: %+v", reversed, r.List())
		}
	}
}

func TestDeclaringRefusesWhatCantBe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		decls []Declaration
		want  string
	}{
		{"name twice", []Declaration{{Name: "a"}, {Name: "a"}}, "declared twice"},
		{"hardware twice", []Declaration{{Name: "a", Driver: "sim", ID: "0x1"}, {Name: "b", Driver: "sim", ID: "0x1"}}, "both declared"},
		{"bad name", []Declaration{{Name: "Porch Light"}}, "isn't a device name"},
		{"not a type", []Declaration{{Name: "a", DeviceType: "Toaster"}}, "isn't one of"},
		{"half hardware", []Declaration{{Name: "a", ID: "0x1"}}, "half its hardware"},
		{"undeclared name", []Declaration{{Name: "lamp", Driver: "sim", ID: "0x5"}}, "isn't declared"},
		{"type it can't be", []Declaration{{Name: "lamp", Driver: "sim", ID: "0x1", DeviceType: devicetype.DimmableLight}}, "no brightness"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := open(t)
			report(t, r, "0x1", "Lamp")
			before := r.List()
			if _, err := r.Declare(tc.decls); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
			if after := r.List(); len(after.Devices) != len(before.Devices) || after.Devices[0].Name != "lamp" || after.Next != before.Next {
				t.Errorf("a refused declaration changed the registry: %+v", after)
			}
		})
	}
}

func TestPlanChangesNothing(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Lamp")
	got, err := r.Plan([]Declaration{{Name: "desk-lamp", Driver: "sim", ID: "0x1"}})
	if err != nil || len(got.Changes) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, ok := r.Get("lamp"); !ok {
		t.Error("planning renamed the device")
	}
}

func TestADeclarationTakesUpANameReservedByHand(t *testing.T) {
	r, _ := open(t)
	if err := r.Reserve("greenhouse-fan", ""); err != nil {
		t.Fatal(err)
	}
	reserved := get(t, r, "greenhouse-fan")
	declareOK(t, r, Declaration{Name: "greenhouse-fan", Room: "Greenhouse"})
	if d := get(t, r, "greenhouse-fan"); !d.Declared || d.Endpoint != reserved.Endpoint || d.Room != "Greenhouse" {
		t.Errorf("got %+v", d)
	}
}

func TestDeclaringARemovedDeviceSaysItsBack(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Lamp")
	if err := r.Remove("lamp"); err != nil {
		t.Fatal(err)
	}
	got := declareOK(t, r, Declaration{Name: "lamp", Driver: "sim", ID: "0x1"})
	if !slices.Equal(got.Changes, []string{"Brought back lamp, which was removed"}) || !slices.Contains(got.Touched, "lamp") {
		t.Errorf("got %+v", got)
	}
}

func TestWhatADeclarationTouchesIsSaid(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Hall sensor")
	report(t, r, "0x3", "Other")
	declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x1"}, Declaration{Name: "shed-fan"})
	got := declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x9"})
	if !slices.Equal(got.Fresh, []string{"hall-sensor"}) || !slices.Equal(got.Removed, []string{"shed-fan"}) ||
		!slices.Equal(got.Touched, []string{"hall-sensor"}) {
		t.Errorf("got %+v, want hall-sensor fresh and touched, shed-fan removed, other untouched", got)
	}
}

func TestDeclaringChangesOnlyItsCopy(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Living room lamp")
	before := r.List()
	got, err := Declaring(before, []Declaration{{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: "Study"}})
	if err != nil {
		t.Fatal(err)
	}
	if i := live(got, "reading-lamp"); i < 0 || got.Devices[i].Room != "Study" {
		t.Errorf("got %+v", got.Devices)
	}
	if live(before, "living-room-lamp") < 0 || live(r.List(), "living-room-lamp") < 0 {
		t.Error("declaring changed the registry or the snapshot it was given")
	}
	if _, err := Declaring(before, []Declaration{{Name: "Bad Name", Driver: "sim", ID: "0x1"}}); err == nil {
		t.Error("took a name a device can't have")
	}
}

func TestAnUndeclaredPluginDeviceHasALineToPaste(t *testing.T) {
	lamp := Device{Name: "lamp", ID: "0x1", Driver: "plugin/zigbee"}
	if line, ok := Undeclared(lamp); !ok || line != `home.Device("lamp").Hardware("zigbee", "0x1")` {
		t.Errorf("got %q, %v", line, ok)
	}
	placed := Device{Name: "lamp", ID: "0x1", Driver: "plugin/zigbee", Room: "Hall", DeviceType: devicetype.OnOffLight}
	if line, _ := Undeclared(placed); line != `home.Device("lamp").Hardware("zigbee", "0x1").Room("Hall").Is(oikade.OnOffLight)` {
		t.Errorf("got %q, want its room and type kept", line)
	}
	for _, d := range []Device{
		{Name: "lamp", ID: "0x1", Driver: "plugin/zigbee", Declared: true},
		{Name: "lamp", ID: "0x1", Driver: "plugin/zigbee", Removed: true},
		{Name: "shed-fan"},
		{Name: "kettle", ID: "k", Driver: "simulated/simulator"},
	} {
		if line, ok := Undeclared(d); ok {
			t.Errorf("%+v has %q", d, line)
		}
	}
}
