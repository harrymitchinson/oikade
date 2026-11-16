package oikade

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
	b := func(name string, c Capability[bool]) constant { return constant{name, string(c), capability.KindBool} }
	n := func(name string, c Capability[float64]) constant {
		return constant{name, string(c), capability.KindNumber}
	}
	all := []constant{
		b("On", On), n("Brightness", Brightness), n("Temperature", Temperature), n("Humidity", Humidity),
		b("Open", Open), b("Occupied", Occupied), n("Battery", Battery), n("LinkQuality", LinkQuality),
		n("Setpoint", Setpoint),
		b("Locked", Locked), n("Position", Position),
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

func TestThermostatIsTheDeviceTypes(t *testing.T) {
	if devicetype.Type(Thermostat) != devicetype.Thermostat {
		t.Errorf("oikade.Thermostat is %q, and devicetype's is %q", Thermostat, devicetype.Thermostat)
	}
}

func TestASetReachesTheEngineWhole(t *testing.T) {
	heater := Bool{Device: "heater", Capability: On}
	for name, tc := range map[string]struct {
		set  Set
		want engine.Set
	}{
		"a write": {heater.To(true), engine.Set{Device: "heater", Capability: "on", Value: true}},
		"a claim": {
			heater.To(true).For(time.Hour).UntilNext(),
			engine.Set{Device: "heater", Capability: "on", Value: true, For: time.Hour, Until: engine.UntilNext},
		},
		"until released": {
			heater.To(false).UntilReleased(),
			engine.Set{Device: "heater", Capability: "on", Value: false, Until: engine.UntilReleased},
		},
		"a room": {
			room("hall", Everything).Brightness.To(20),
			engine.Set{Room: "hall", Capability: "brightness", Value: 20.0},
		},
		"a room's lights": {
			room("hall", Lights).On.To(false),
			engine.Set{
				Room: "hall", DeviceTypes: []devicetype.Type{devicetype.OnOffLight, devicetype.DimmableLight},
				Capability: "on", Value: false,
			},
		},
	} {
		if got := tc.set.toEngine(nil, "f"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, tc.want)
		}
	}
	want := []engine.Change{{Room: "hall", Capability: "occupied"}}
	if got := room("hall", Everything).Occupied.changes(); !reflect.DeepEqual(got, want) {
		t.Errorf("a room's change is %+v", got)
	}
	heaters := []engine.Change{{Room: "hall", DeviceTypes: []devicetype.Type{devicetype.Thermostat}, Capability: "on"}}
	if got := room("hall", Heaters).On.changes(); !reflect.DeepEqual(got, heaters) {
		t.Errorf("a room's heaters' change is %+v", got)
	}
	if got := heater.changes(); !reflect.DeepEqual(got, []engine.Change{{Device: "heater", Capability: "on"}}) {
		t.Errorf("a handle's change is %+v", got)
	}
	becomes := []engine.Change{{Device: "heater", Capability: "on", To: true}}
	if got := heater.Becomes(true).changes(); !reflect.DeepEqual(got, becomes) {
		t.Errorf("a Becomes is %+v", got)
	}
}

// Only a home that builds its own handles can get a type wrong, and a condition that does stops,
// which the engine logs.
func TestGettingAValueOfTheWrongTypePanics(t *testing.T) {
	wrong := Number{Device: "door", Capability: "open"}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("no panic")
		}
	}()
	Snapshot{devices: map[string]map[string]any{"door": {"open": true}}}.Get(wrong)
}

// A flow's or a moment's Set ranks where its decision's Flows line is: Safety above the override line.
func TestASetRanksAtItsDecisionsFlowsLine(t *testing.T) {
	heater, lamp := Bool{Device: "heater", Capability: On}, Bool{Device: "lamp", Capability: On}
	decided, err := decide(func(h *Home) {
		h.Decide(heater).Flows().OverrideFor(time.Hour).Default(false)
		h.Decide(lamp).OverrideFor(time.Hour).Flows().Default(false)
	}).compileDecisions()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		set  Set
		want engine.Priority
	}{
		{heater.To(true).For(time.Hour), engine.PrioritySafety},
		{lamp.To(true).For(time.Hour), ""},
		{Bool{Device: "fan", Capability: On}.To(true), ""},
	} {
		if got := tc.set.toEngine(decided, "f").(engine.Set).Priority; got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.set.device, got, tc.want)
		}
	}
}

// A flow's own Flows line ranks it before the line for every flow, so a leak flow can sit above a
// tap and the rest below it. A Set with no line to rank at is refused.
func TestAFlowRanksAtItsOwnLineOrTheLineForEveryFlow(t *testing.T) {
	valve, heater := Bool{Device: "valve", Capability: On}, Bool{Device: "heater", Capability: On}
	var leak *Flow
	h := decide(func(h *Home) {
		leak = h.Flow("leak", nothing).When(valve)
		h.Flow("bedtime", nothing).At(Clock("22:00"))
		h.Decide(valve).Flows(leak).OverrideFor(time.Hour).Flows().Default(true)
		h.Decide(heater).Flows(leak).Default(false)
	})
	decided, err := h.compileDecisions()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		flow, device string
		want         engine.Priority
		refused      bool
	}{
		{flow: "leak", device: "valve", want: engine.PrioritySafety},
		{flow: "bedtime", device: "valve"},
		{flow: "leak", device: "heater"},
		{flow: "bedtime", device: "heater", refused: true},
	} {
		if got := claimPriority(decided, tc.flow, tc.device, "on"); got != tc.want {
			t.Errorf("%s on %s: got %q, want %q", tc.flow, tc.device, got, tc.want)
		}
		err := needsFlowsLine(decided, tc.flow, tc.device, "on")
		if (err != nil) != tc.refused {
			t.Errorf("%s on %s: got %v", tc.flow, tc.device, err)
		}
	}
	m := h.manifest()
	if !reflect.DeepEqual(m.Control[0].Entry.Lines, []string{"Rule/leak", "override", "flows"}) {
		t.Errorf("got lines %v", m.Control[0].Entry.Lines)
	}
	_, flows, err := h.compile()
	if err != nil {
		t.Fatal(err)
	}
	// A flow a decision ranks above its override line acts however late it is.
	if !flows["leak"].Safety || flows["bedtime"].Safety {
		t.Errorf("leak is Safety %v, bedtime %v", flows["leak"].Safety, flows["bedtime"].Safety)
	}
}
