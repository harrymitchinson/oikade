package hometest

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/framework"
)

func TestAPlanThatFailsIsAnEvent(t *testing.T) {
	pump := framework.Handle[bool]{Device: "pump", Capability: framework.On}
	home := framework.NewHome(framework.Options{},
		framework.NewControl(pump.To(false), framework.PreferOn, framework.ExternalOverride).
			OverrideHold(time.Hour),
		framework.NewHold("pump", pump.To(true)).
			Budget(framework.NewBudget(func(framework.State) int { panic("no reading") })),
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
