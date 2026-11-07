package devicetype

import (
	"slices"
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
)

func TestOf(t *testing.T) {
	on, dim := []string{capability.On}, []string{capability.On, capability.Brightness}
	for name, tc := range map[string]struct {
		yours, drivers Type
		caps           []string
		want           []Type
	}{
		"on alone is a plug":               {"", "", on, []Type{OnOffPlugInUnit}},
		"with a brightness a light":        {"", "", dim, []Type{DimmableLight}},
		"the driver's beats the default":   {"", OnOffLight, on, []Type{OnOffLight}},
		"yours beats the driver's":         {MountedOnOffControl, OnOffLight, on, []Type{MountedOnOffControl}},
		"an unknown driver's is ignored":   {"", "Toaster", on, []Type{OnOffPlugInUnit}},
		"nothing to dim drops dimmable":    {DimmablePlugInUnit, "", on, []Type{OnOffPlugInUnit}},
		"yours can leave dimming out":      {OnOffLight, "", dim, []Type{OnOffLight}},
		"sensors follow capabilities":      {"", "", []string{capability.Temperature, capability.Occupied, capability.Battery}, []Type{OccupancySensor, TemperatureSensor}},
		"a type means nothing without on":  {OnOffLight, OnOffLight, []string{capability.Open}, []Type{ContactSensor}},
		"a switch with a sensor is both":   {"", "", []string{capability.On, capability.Humidity}, []Type{OnOffPlugInUnit, HumiditySensor}},
		"battery alone is no device type":  {"", "", []string{capability.Battery}, nil},
		"a dimmable wall control":          {MountedDimmableLoadControl, "", dim, []Type{MountedDimmableLoadControl}},
		"a light without brightness is on": {"", DimmableLight, on, []Type{OnOffLight}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Of(tc.yours, tc.drivers, tc.caps); !slices.Equal(got, tc.want) {
				t.Errorf("Of(%q, %q, %v) = %v, want %v", tc.yours, tc.drivers, tc.caps, got, tc.want)
			}
		})
	}
}

func TestChoosable(t *testing.T) {
	for _, typ := range OnOff {
		if !Choosable(typ) {
			t.Errorf("%s isn't choosable", typ)
		}
		if sibling, ok := dimmed[typ]; ok && (!Choosable(sibling) || Dimmable(sibling)) {
			t.Errorf("%s dims down to %s", typ, sibling)
		}
	}
	for _, typ := range []Type{"", OccupancySensor, "onofflight"} {
		if Choosable(typ) {
			t.Errorf("%q is choosable", typ)
		}
	}
}
