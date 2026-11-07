package framework

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/engine"
)

// oikade types names a handle's field and its type from the registry, so each constant here must
// agree with it, and every capability there must have one.
func TestTheConstantsAreTheRegistrys(t *testing.T) {
	type constant struct {
		name, value string
		kind        capability.Kind
	}
	b := func(name string, c Bool) constant { return constant{name, string(c), capability.KindBool} }
	n := func(name string, c Number) constant { return constant{name, string(c), capability.KindNumber} }
	all := []constant{
		b("On", On), n("Brightness", Brightness), n("Temperature", Temperature), n("Humidity", Humidity),
		b("Open", Open), b("Occupied", Occupied), n("Battery", Battery), n("LinkQuality", LinkQuality),
	}
	values := make([]string, 0, len(all))
	for _, c := range all {
		spec, ok := capability.Lookup(c.value)
		switch {
		case !ok:
			t.Errorf("%s is %q, which the registry doesn't have", c.name, c.value)
		case spec.Go != c.name || spec.Kind != c.kind:
			t.Errorf("%s is a %s, and the registry has %q as %s, a %s", c.name, c.kind, c.value, spec.Go, spec.Kind)
		}
		values = append(values, c.value)
	}
	slices.Sort(values)
	if names := capability.Names(); !slices.Equal(values, names) {
		t.Errorf("the constants are %v, and the registry has %v", values, names)
	}
}

func TestASetReachesTheEngineWhole(t *testing.T) {
	heater := Handle[bool]{Device: "heater", Capability: On}
	for name, tc := range map[string]struct {
		set  Set
		want engine.Set
	}{
		"a write": {heater.To(true), engine.Set{Device: "heater", Capability: "on", Value: true}},
		"a claim": {
			heater.To(true).For(time.Hour).UntilNext().Priority(Override),
			engine.Set{
				Device: "heater", Capability: "on", Value: true, For: time.Hour, Until: engine.UntilNext,
				Priority: engine.Priority(Override),
			},
		},
		"until released": {
			heater.To(false).UntilReleased(),
			engine.Set{Device: "heater", Capability: "on", Value: false, Until: engine.UntilReleased},
		},
		"a room": {
			InRoom("hall", Brightness).To(20),
			engine.Set{Room: "hall", Capability: "brightness", Value: 20.0},
		},
		"a room's lights": {
			InRoom("hall", On).Types(OnOffLight, DimmableLight).To(false),
			engine.Set{
				Room: "hall", DeviceTypes: []devicetype.Type{devicetype.OnOffLight, devicetype.DimmableLight},
				Capability: "on", Value: false,
			},
		},
	} {
		if got := tc.set.toEngine(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, tc.want)
		}
	}
	want := engine.Change{Room: "hall", Capability: "occupied"}
	if got := InRoom("hall", Occupied).change(); got != want {
		t.Errorf("a room's change is %+v", got)
	}
	if got := heater.change(); got != (engine.Change{Device: "heater", Capability: "on"}) {
		t.Errorf("a handle's change is %+v", got)
	}
}

// Only a home that builds its own handles can get a type wrong, and a condition that does stops,
// which the engine logs.
func TestGettingAValueOfTheWrongTypePanics(t *testing.T) {
	wrong := Handle[float64]{Device: "door", Capability: "open"}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("no panic")
		}
	}()
	State{devices: map[string]map[string]any{"door": {"open": true}}}.Get(wrong)
}
