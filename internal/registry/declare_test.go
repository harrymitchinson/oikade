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

func TestDeclaringPlacesTypesAndLabelsADeviceByItsHardware(t *testing.T) {
	r, _ := open(t)
	lamp := report(t, r, "0x1", "Lampe salon")
	got := declareOK(t, r, Declaration{
		Hardware: "sim/0x1", Room: "salon", DeviceType: devicetype.OnOffLight, Label: "Salon",
	})
	d := get(t, r, "sim/0x1")
	if d.Room != "salon" || d.DeviceType != devicetype.OnOffLight || d.Label != "Salon" || d.Endpoint != lamp.Endpoint ||
		d.AppName != "Lampe salon" {
		t.Errorf("got %+v", d)
	}
	want := []string{
		"Moved hardware/sim/0x1 to salon", "Set hardware/sim/0x1's device type to OnOffLight",
		`Labelled hardware/sim/0x1 "Salon" on Matter`,
	}
	if !slices.Equal(got.Changes, want) || !slices.Equal(got.Touched, []string{"sim/0x1"}) {
		t.Errorf("got %+v, want changes %q", got, want)
	}
	if again := declareOK(t, r, Declaration{Hardware: "sim/0x1", Room: "salon", DeviceType: devicetype.OnOffLight,
		Label: "Salon"}); len(again.Changes) != 0 || len(again.Touched) != 0 {
		t.Errorf("declaring the same again did %+v", again)
	}
}

func TestADeviceNoLongerDeclaredGoesBackToItsApps(t *testing.T) {
	r, _ := open(t)
	if _, _, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Hall sensor", Room: "Entrée", Capabilities: []string{"on"}}); err != nil {
		t.Fatal(err)
	}
	declareOK(t, r, Declaration{Hardware: "sim/0x1", Room: "porch", DeviceType: devicetype.OnOffPlugInUnit, Label: "Porch"})
	got := declareOK(t, r)
	if d := get(t, r, "sim/0x1"); d.Room != "entree" || d.DeviceType != "" || d.Label != "" {
		t.Errorf("got %+v", d)
	}
	want := []string{"Gave hardware/sim/0x1 back its app's room and its driver's type"}
	if !slices.Equal(got.Changes, want) {
		t.Errorf("got %q, want %q", got.Changes, want)
	}
}

// A declared device in no room is in none, whatever its app says.
func TestADeclaredDeviceWithoutARoomIsInNone(t *testing.T) {
	r, _ := open(t)
	if _, _, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Lamp", Room: "Study", Capabilities: []string{"on"}}); err != nil {
		t.Fatal(err)
	}
	got := declareOK(t, r, Declaration{Hardware: "sim/0x1"})
	if d := get(t, r, "sim/0x1"); d.Room != "" {
		t.Errorf("got %+v", d)
	}
	if !slices.Equal(got.Changes, []string{"Took hardware/sim/0x1 out of its room"}) {
		t.Errorf("got %q", got.Changes)
	}
}

// Other hardware on a line is another device: it gets an endpoint of its own, and the old hardware
// keeps its.
func TestOtherHardwareIsAnotherDevice(t *testing.T) {
	r, _ := open(t)
	old := report(t, r, "0x1", "Fan")
	declareOK(t, r, Declaration{Hardware: "sim/0x1", Room: "attic"})
	got := declareOK(t, r, Declaration{Hardware: "hue/0x2", Driver: "plugin/hue", ID: "0x2", Room: "attic"})
	swapped := get(t, r, "hue/0x2")
	if swapped.Endpoint == old.Endpoint || get(t, r, "sim/0x1").Endpoint != old.Endpoint {
		t.Errorf("the new hardware has endpoint %d, and the old %d", swapped.Endpoint, old.Endpoint)
	}
	if !slices.Equal(got.Unseen, []string{"hue/0x2"}) || !slices.Contains(got.Changes,
		"Put hardware/hue/0x2 on record") {
		t.Errorf("got %+v", got)
	}
}

// Hardware a line in your code names before its plugin reports it is on record, with its endpoint,
// and takes its app's name once it reports.
func TestDeclaredHardwareWaitsForItsPlugin(t *testing.T) {
	r, _ := open(t)
	declared := declareOK(t, r, Declaration{Hardware: "hue/0x9", Driver: "plugin/hue", ID: "0x9", Room: "kitchen",
		Capabilities: []string{"on"}})
	waiting := get(t, r, "hue/0x9")
	if !slices.Equal(declared.Unseen, []string{"hue/0x9"}) || waiting.Room != "kitchen" {
		t.Errorf("got %+v, and %+v", declared, waiting)
	}
	d, _, err := r.Report(Report{ID: "0x9", Driver: "plugin/hue", Name: "Kitchen ceiling", Room: "Hall", Capabilities: []string{"on"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Key() != "hue/0x9" || d.Endpoint != waiting.Endpoint || d.AppName != "Kitchen ceiling" || d.Room != "kitchen" {
		t.Errorf("paired, it's %+v", d)
	}
}

func TestDeclaringRefusesWhatCantBe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		decls []Declaration
		want  string
	}{
		{"twice", []Declaration{{Hardware: "sim/0x1"}, {Hardware: "sim/0x1"}}, "sim/0x1 is declared twice"},
		{"a room that isn't one", []Declaration{{Hardware: "sim/0x1", Room: "Living Room"}}, `"Living Room" isn't a room name`},
		{"not a type", []Declaration{{Hardware: "sim/0x1", DeviceType: "Toaster"}}, `"Toaster", which isn't a device type`},
		{
			"a type it can't be", []Declaration{{Hardware: "sim/0x1", DeviceType: devicetype.DimmableLight}},
			"sim/0x1 has no brightness",
		},
		{
			"a capability it lacks", []Declaration{{Hardware: "sim/0x1", Capabilities: []string{"on", "temperature"}}},
			"sim/0x1 has no temperature, which its line in your code gives it: it has on",
		},
		{"no plugin's, and not on record", []Declaration{{Hardware: "sim/0x2"}}, "sim/0x2 isn't on record: it's no plugin's"},
		{
			"the simulator's, and not on record",
			[]Declaration{{Hardware: "simulator/sim-2", Driver: "simulated/simulator", ID: "sim-2"}},
			"simulator/sim-2 isn't on record: it's no plugin's",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := open(t)
			report(t, r, "0x1", "Lamp")
			before := r.List()
			if _, err := r.Declare(tc.decls); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
			if after := r.List(); len(after.Devices) != len(before.Devices) || after.Devices[0].Room != before.Devices[0].Room ||
				after.Next != before.Next {
				t.Errorf("a refused declaration changed the registry: %+v", after)
			}
		})
	}
}

func TestPlanChangesNothing(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Lamp")
	got, err := r.Plan([]Declaration{{Hardware: "sim/0x1", Room: "study"}})
	if err != nil || len(got.Changes) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if d := get(t, r, "sim/0x1"); d.Room != "" {
		t.Errorf("planning moved the device: %+v", d)
	}
}

func TestDeclaringAForgottenDeviceSaysItsBack(t *testing.T) {
	r, _ := open(t)
	lamp := report(t, r, "0x1", "Lamp")
	if err := r.Remove(lamp.Key()); err != nil {
		t.Fatal(err)
	}
	got := declareOK(t, r, Declaration{Hardware: "sim/0x1"})
	if !slices.Equal(got.Changes, []string{"Brought back hardware/sim/0x1, which was forgotten"}) ||
		!slices.Equal(got.Touched, []string{"sim/0x1"}) {
		t.Errorf("got %+v", got)
	}
	if d := get(t, r, "sim/0x1"); d.Endpoint != lamp.Endpoint {
		t.Errorf("came back as %+v", d)
	}
}

func TestDeclaringChangesOnlyItsCopy(t *testing.T) {
	r, _ := open(t)
	report(t, r, "0x1", "Living room lamp")
	before := r.List()
	got, err := Declaring(before, []Declaration{{Hardware: "sim/0x1", Room: "study"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Devices[0].Room != "study" {
		t.Errorf("got %+v", got.Devices)
	}
	if before.Devices[0].Room != "" || get(t, r, "sim/0x1").Room != "" {
		t.Error("declaring changed the registry or the snapshot it was given")
	}
	if _, err := Declaring(before, []Declaration{{Hardware: "sim/0x1", Room: "Bad Room"}}); err == nil {
		t.Error("took a room that isn't one")
	}
}
