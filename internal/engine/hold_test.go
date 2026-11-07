package engine

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/sun"
)

func TestCompileHold(t *testing.T) {
	c := holdRule(t, "r", Hold{Device: "workshop-heater", Capability: capability.SwitchOn, Value: false, Priority: PrioritySafety, Window: &Window{Start: SunTime{Event: sun.Sunset, Offset: -15 * time.Minute}, End: At{Time: "23:30"}, Days: []time.Weekday{time.Friday, time.Saturday}}, When: above("workshop-temperature", capability.Temperature, 45), OnDelay: time.Minute, OffDelay: 5 * time.Minute, Latch: true})
	h := c.hold
	if h == nil {
		t.Fatal("no hold")
	}
	if h.device != "workshop-heater" || h.capability != "switch.on" || h.value != false || h.priority != PrioritySafety {
		t.Errorf("hold target: %+v", h)
	}
	if h.when == nil || !h.latch || h.onDelay != time.Minute || h.offDelay != 5*time.Minute {
		t.Errorf("hold timing: %+v", h)
	}
	w := h.window
	if w.start.event != sun.Sunset || w.start.offset != -15*time.Minute || w.end.minutes != 23*60+30 {
		t.Errorf("window: %+v", w)
	}
	if len(w.days) != 2 || w.days[0] != time.Friday {
		t.Errorf("days: %v", w.days)
	}
	if len(c.changes) != 0 || len(c.timers) != 0 || len(c.actions) != 0 {
		t.Error("a hold has no triggers or actions")
	}
}

func TestCompileClaims(t *testing.T) {
	c := momentRule(t, "r", Moment{Changes: []Change{{Device: "b", Capability: capability.ContactOpen}}, Do: []Action{Set{Device: "rail", Capability: capability.SwitchOn, Value: true, For: time.Hour, Until: UntilNext, Priority: PriorityOverride}, Set{Device: "rail", Capability: capability.SwitchOn, Value: false, Until: UntilReleased}, Release("other")}})
	if a := c.actions[0]; a.claimFor != time.Hour || a.until != UntilNext || a.priority != PriorityOverride {
		t.Errorf("first set: %+v", a)
	}
	if a := c.actions[1]; a.until != UntilReleased {
		t.Errorf("second set: %+v", a)
	}
	if a := c.actions[2]; a.release != "other" {
		t.Errorf("release: %+v", a)
	}
}

// A release clears another Rule's latch; it writes nothing itself.
func TestReleaseActionDoesNotSet(t *testing.T) {
	setter := &fakeSetter{}
	result, _ := runRoom(t, setter, nil,
		Release("workshop-overheat"),
		set("hallway-light", "light.on", "true"))
	if result != ResultSucceeded {
		t.Fatalf("result = %s, want %s", result, ResultSucceeded)
	}
	if got := setter.got(); len(got) != 1 || got[0].device != "hallway-light" {
		t.Fatalf("calls = %v, want only the set", got)
	}
}
