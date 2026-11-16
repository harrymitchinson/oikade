package hometest

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade"
)

func TestAPlanThatFailsIsAnEvent(t *testing.T) {
	pump := oikade.Bool{Device: "pump", Capability: oikade.On}
	home := oikade.NewHome(oikade.Options{},
		oikade.NewControl(pump.To(false), oikade.PreferOn, oikade.ExternalOverride).
			OverrideHold(time.Hour),
		oikade.NewHold("pump", pump.To(true)).
			Budget(oikade.NewBudget(func(oikade.Snapshot) int { panic("no reading") })),
	)
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
