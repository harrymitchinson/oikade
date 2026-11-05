package zigbee2mqtt

import (
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
)

func TestBindingsBindACapabilityOnce(t *testing.T) {
	state := func(property string) []expose {
		return []expose{{Name: "state", Type: exposeBinary, Property: property, ValueOn: "ON", ValueOff: "OFF"}}
	}
	got := bindings([]expose{
		{Type: "light", Features: state("state")},
		{Type: "switch", Features: state("state_relay")},
	})
	if len(got) != 1 || got[0].capability != capability.OnOff || got[0].property != "state" {
		t.Fatalf("got %+v, want on-off once, from the light", got)
	}
}
