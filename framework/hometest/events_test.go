package hometest

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/framework"
)

func TestAPlanThatFailsIsAnEvent(t *testing.T) {
	pump := framework.Handle[bool]{Device: "pump", Capability: framework.On}
	home := framework.Home{
		Control: []framework.Control{{
			Default: pump.To(false), Resolve: framework.PreferOn,
			External: framework.ExternalOverride, OverrideHold: time.Hour,
		}},
		Holds: []framework.Hold{{
			Name: "pump", Set: pump.To(true),
			Budget: &framework.Budget{Minutes: func(framework.State) int { panic("no reading") }},
		}},
	}
	h := New(t, home, Options{Devices: []Device{{
		Name: "pump", Capabilities: []string{"on"}, Values: map[string]any{"on": false},
	}}})
	h.Advance(0)
	for _, e := range h.sim.Events() {
		if strings.Contains(e.Text, "couldn't plan") {
			return
		}
	}
	t.Fatalf("no event says so: %v", h.sim.Events())
}
