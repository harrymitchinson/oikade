package zigbee2mqtt

import (
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
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
