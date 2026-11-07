package zigbee2mqtt

import (
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
)

// on is bound once, to the first property, so a second can't flip it.
func TestALightAndASwitchAtTheRootBindOnOnce(t *testing.T) {
	state := func(property string) expose {
		return expose{Name: "state", Type: exposeBinary, Property: property, ValueOn: "ON", ValueOff: "OFF"}
	}
	got := bindings([]expose{
		{Type: "light", Features: []expose{state("state")}},
		{Type: "switch", Features: []expose{state("state_backlight")}},
	})
	if len(got) != 1 || got[0].capability != capability.On || got[0].property != "state" {
		t.Fatalf("got %+v, want on bound to state alone", got)
	}
}

func TestALightSaysWhatItIs(t *testing.T) {
	state := expose{Name: "state", Type: exposeBinary, Property: "state", ValueOn: "ON", ValueOff: "OFF"}
	brightness := expose{Name: "brightness", Type: exposeNumeric, Property: "brightness"}
	for name, tc := range map[string]struct {
		exposes []expose
		want    devicetype.Type
	}{
		"a dimmable light":  {[]expose{{Type: "light", Features: []expose{state, brightness}}}, devicetype.DimmableLight},
		"an on/off light":   {[]expose{{Type: "light", Features: []expose{state}}}, devicetype.OnOffLight},
		"a switch":          {[]expose{{Type: "switch", Features: []expose{state}}}, ""},
		"a switch, first":   {[]expose{{Type: "switch", Features: []expose{state}}, {Type: "light", Features: []expose{state}}}, ""},
		"a light, first":    {[]expose{{Type: "light", Features: []expose{state}}, {Type: "switch", Features: []expose{state}}}, devicetype.OnOffLight},
		"a gang of a light": {[]expose{{Type: "light", Endpoint: "l1", Features: []expose{state}}}, ""},
		"a sensor":          {[]expose{{Type: exposeNumeric, Name: "temperature"}}, ""},
	} {
		if got := deviceType(tc.exposes); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
