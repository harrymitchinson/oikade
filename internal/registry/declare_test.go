package registry

import (
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/devicetype"
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
	got := declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: new("study"), DeviceType: devicetype.OnOffLight})
	d := get(t, r, "reading-lamp")
	if !d.Declared || d.Room != "study" || d.DeviceType != devicetype.OnOffLight || d.Endpoint != before.Endpoint {
		t.Errorf("got %+v, want it declared, in the study, a light, on its endpoint", d)
	}
	if got.Renamed["living-room-lamp"] != "reading-lamp" {
		t.Errorf("renamed %v", got.Renamed)
	}
	want := []string{"Renamed living-room-lamp to reading-lamp", "Moved reading-lamp to study", "Set reading-lamp's device type to OnOffLight"}
	if !slices.Equal(got.Changes, want) {
		t.Errorf("changes %q, want %q", got.Changes, want)
	}
	again, err := Open(values)
	if err != nil || !get(t, again, "reading-lamp").Declared {
		t.Fatalf("declaration not saved: %v", err)
	}
	if same := declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: new("study"), DeviceType: devicetype.OnOffLight}); len(same.Changes) != 0 || len(same.Renamed) != 0 {
		t.Errorf("declaring it again changed %+v", same)
	}
}

func TestADeviceNoLongerDeclaredGoesBackToItsAppsName(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Living room lamp")
	declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: new("study")}, Declaration{Name: "shed-fan"})
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
	got := declareOK(t, r, Declaration{Name: "porch-light", Driver: "sim", ID: "0x9"}, Declaration{Name: "greenhouse-fan", Room: new("greenhouse")})
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

func TestADeclarationWithoutARoomKeepsItsAppsRoom(t *testing.T) {
	r, _ := open(t)
	if _, _, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Lamp", Room: "Living Room", Capabilities: []string{"on"}}); err != nil {
		t.Fatal(err)
	}
	declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1"})
	if d := get(t, r, "reading-lamp"); d.Room != "living-room" {
		t.Errorf("got room %q, want its app's, living-room", d.Room)
	}
	got := declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: new("")})
	if d := get(t, r, "reading-lamp"); d.Room != "" || !slices.Equal(got.Changes, []string{"Took reading-lamp out of its room"}) {
		t.Errorf("Room(\"\") left it in %q, with changes %q", d.Room, got.Changes)
	}
	got = declareOK(t, r, Declaration{Name: "reading-lamp", Driver: "sim", ID: "0x1"})
	if want := []string{"Moved reading-lamp to living-room, its app's room"}; !slices.Equal(got.Changes, want) {
		t.Errorf("changes %q, want %q", got.Changes, want)
	}
}

func TestDeclaredHardwareWithoutARoomTakesItsAppsWhenItReports(t *testing.T) {
	r, values := open(t)
	declareOK(t, r, Declaration{Name: "porch-light", Driver: "sim", ID: "0x8"}, Declaration{Name: "shed-light", Driver: "sim", ID: "0x9", Room: new("")})
	// The registry is read back between, as a host that restarted would.
	r, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	// porch-light's app gives it a room and no name.
	for _, report := range []Report{
		{ID: "0x8", Driver: "sim", Room: "Porch", Capabilities: []string{"on"}},
		{ID: "0x9", Driver: "sim", Name: "Light 9", Room: "Porch", Capabilities: []string{"on"}},
	} {
		if _, _, err := r.Report(report); err != nil {
			t.Fatal(err)
		}
	}
	if d := get(t, r, "porch-light"); d.Room != "porch" {
		t.Errorf("porch-light is in %q, want its app's room, porch", d.Room)
	}
	if d := get(t, r, "shed-light"); d.Room != "" {
		t.Errorf("shed-light, declared in no room, is in %q", d.Room)
	}
}

func TestADeclaredNameMovesOntoNewHardwareAndKeepsItsEndpoint(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Hall sensor")
	declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x1", Room: new("hall")})
	old := get(t, r, "hall-sensor")
	report(t, r, "0x2", "Sensor 7")
	got := declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x2", Room: new("hall")})
	d := get(t, r, "hall-sensor")
	if d.ID != "0x2" || d.Endpoint != old.Endpoint || d.Room != "hall" {
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

func TestDeclaringARemovedDeviceSaysItsBack(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Lamp")
	if err := r.Remove("lamp"); err != nil {
		t.Fatal(err)
	}
	got := declareOK(t, r, Declaration{Name: "lamp", Driver: "sim", ID: "0x1"})
	if !slices.Equal(got.Changes, []string{"Brought back lamp, which was removed"}) || !slices.Contains(got.Touched, "0x1") {
		t.Errorf("got %+v", got)
	}
}

func TestWhatADeclarationTouchesIsSaid(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Hall sensor")
	report(t, r, "0x3", "Other")
	declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x1"}, Declaration{Name: "shed-fan"})
	got := declareOK(t, r, Declaration{Name: "hall-sensor", Driver: "sim", ID: "0x9"})
	if !slices.Equal(got.Removed, []string{"shed-fan"}) || !slices.Equal(got.Touched, []string{"0x9"}) {
		t.Errorf("got %+v, want hall-sensor touched by its new hardware's key, shed-fan removed, other untouched", got)
	}
}

func TestDeclaringChangesOnlyItsCopy(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Living room lamp")
	before := r.List()
	got, err := Declaring(before, []Declaration{{Name: "reading-lamp", Driver: "sim", ID: "0x1", Room: new("study")}})
	if err != nil {
		t.Fatal(err)
	}
	if i := live(got, "reading-lamp"); i < 0 || got.Devices[i].Room != "study" {
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
	placed := Device{Name: "lamp", ID: "0x1", Driver: "plugin/zigbee", Room: "hall", DeviceType: devicetype.OnOffLight}
	if line, _ := Undeclared(placed); line != `home.Device("lamp").Hardware("zigbee", "0x1").Room("hall").Is(oikade.OnOffLight{})` {
		t.Errorf("got %q, want its room and type kept", line)
	}
	inAppsRoom := Device{Name: "lamp", ID: "0x1", Driver: "plugin/zigbee", Room: "living-room", AppRoom: "Living Room"}
	if line, _ := Undeclared(inAppsRoom); line != `home.Device("lamp").Hardware("zigbee", "0x1")` {
		t.Errorf("got %q, want no Room for a device in its app's room", line)
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

// Hardware declared by itself, as oikade.Device declares it, keeps its name, and takes its room, type
// and label.
func TestHardwareDeclaredByItselfKeepsItsName(t *testing.T) {
	r, _ := open(t)
	before, _, err := r.Report(Report{ID: "0x1", Driver: "plugin/hue", Name: "Salon lampe", Room: "Salon", Capabilities: []string{"on", "brightness"}})
	if err != nil {
		t.Fatal(err)
	}
	line := Declaration{
		Hardware: "hue/0x1", Driver: "plugin/hue", ID: "0x1", Room: new("cuisine"), DeviceType: devicetype.DimmableLight,
		Capabilities: []string{"brightness", "on"}, Label: "Lampe",
	}
	got := declareOK(t, r, line)
	d := get(t, r, "salon-lampe")
	if !d.Declared || d.Room != "cuisine" || d.DeviceType != devicetype.DimmableLight || d.Label != "Lampe" || d.Endpoint != before.Endpoint {
		t.Errorf("got %+v", d)
	}
	want := []string{"Moved salon-lampe to cuisine", "Set salon-lampe's device type to DimmableLight", `Labelled salon-lampe "Lampe" on Matter`}
	if !slices.Equal(got.Changes, want) || len(got.Renamed) != 0 || !slices.Equal(got.Touched, []string{"hue/0x1"}) {
		t.Errorf("got %+v, want the changes %q", got, want)
	}

	// A sensor's type follows from its capabilities, which the lamp lacks.
	sensor := Declaration{
		Hardware: "hue/0x1", Driver: "plugin/hue", ID: "0x1", Room: new(""), DeviceType: devicetype.OccupancySensor,
		Capabilities: []string{"occupied"},
	}
	if _, err := r.Plan([]Declaration{sensor}); err == nil || !strings.Contains(err.Error(), "hue/0x1 has no occupied") {
		t.Errorf("a lamp declared a sensor is %v", err)
	}

	// A declared name that takes its name gives it another.
	got = declareOK(t, r, line, Declaration{Name: "salon-lampe"})
	if get(t, r, "salon-lampe").ID != "" || get(t, r, "salon-lampe-2").ID != "0x1" {
		t.Errorf("got %+v", r.List())
	}

	got = declareOK(t, r)
	if d := get(t, r, "salon-lampe"); d.Declared || d.Room != "salon" || d.DeviceType != "" || d.Label != "" {
		t.Errorf("no longer declared, it's %+v", d)
	}
}

func TestHardwareDeclaredByItselfIsChecked(t *testing.T) {
	r, _ := open(t)
	if _, _, err := r.Report(Report{ID: "0x1", Driver: "plugin/hue", Name: "Lamp", Capabilities: []string{"on"}}); err != nil {
		t.Fatal(err)
	}
	report(t, r, "sim-1", "Sim")
	for name, tc := range map[string]struct {
		decls []Declaration
		want  string
	}{
		"a capability it lacks": {
			[]Declaration{{Hardware: "hue/0x1", Driver: "plugin/hue", ID: "0x1", Capabilities: []string{"on", "temperature"}}},
			"hue/0x1 has no temperature, which its line in your code gives it: it has on",
		},
		"no plugin's, and not on record": {[]Declaration{{Hardware: "sim-2"}}, "sim-2 isn't on record"},
		"twice": {
			[]Declaration{{Hardware: "sim-1"}, {Hardware: "sim-1"}}, "sim-1 is declared twice",
		},
		"both ways": {
			[]Declaration{{Hardware: "hue/0x1", Driver: "plugin/hue", ID: "0x1"}, {Name: "lamp", Driver: "plugin/hue", ID: "0x1"}},
			"hue/0x1 is declared by itself and as lamp: declare it once",
		},
		"no type": {[]Declaration{{Hardware: "sim-1", DeviceType: "Toaster"}}, `sim-1 is declared a "Toaster", which isn't a device type`},
	} {
		if _, err := r.Plan(tc.decls); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}

	// Hardware no plugin has reported yet waits for it.
	got := declareOK(t, r, Declaration{Hardware: "hue/0x9", Driver: "plugin/hue", ID: "0x9", Capabilities: []string{"on"}})
	if !slices.Equal(got.Unseen, []string{"hue/0x9"}) || get(t, r, "device-0x9").Key() != "hue/0x9" {
		t.Errorf("got %+v, and %+v", got, r.List())
	}
	if _, err := r.Plan([]Declaration{{Hardware: "sim-1", Capabilities: []string{"on"}}}); err != nil {
		t.Errorf("hardware of no plugin, on record, is refused: %v", err)
	}
}
