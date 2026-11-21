package oikadetest

import (
	"strings"
	"testing"
	"time"

	"oikade.dev"
)

func TestAPlanThatFailsIsAnEvent(t *testing.T) {
	pump := oikade.Bool{Device: "pump", Capability: oikade.On}
	home := oikade.NewHome(oikade.Options{})
	home.Decide(pump).
		OverrideFor(time.Hour).
		Budget("daily", true, oikade.NewBudget(func(oikade.Snapshot) int { panic("no reading") })).
		Default(false)
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
