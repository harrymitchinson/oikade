package oikade

import (
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/flow"
)

func TestCompileChecksFlows(t *testing.T) {
	on := []Change{Bool{Device: "hall", Capability: Open}}
	for name, tc := range map[string]struct {
		flow func(*Home)
		want string
	}{
		"no name":    {func(h *Home) { h.Flow("", nothing).When(on...) }, "needs a name"},
		"no trigger": {func(h *Home) { h.Flow("a", nothing) }, "a trigger is required"},
		"no run":     {func(h *Home) { h.Flow("a", nil).When(on...) }, "needs a body"},
		"negative":   {func(h *Home) { h.Flow("a", nothing).When(on...).Late(-time.Minute) }, "must not be negative"},
		"bad time":   {func(h *Home) { h.Flow("a", nothing).At(Clock("25:00")) }, "timers[0]"},
		"bad change": {
			func(h *Home) { h.Flow("a", nothing).When(Bool{Device: "hall", Capability: "no.such"}) }, "unknown capability",
		},
		"a name twice": {
			func(h *Home) { h.Flow("a", nothing).When(on...); h.Flow("a", nothing).When(on...) }, "declared twice",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := decide(tc.flow).compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLateness(t *testing.T) {
	if DefaultLate != flow.DefaultLate {
		t.Fatalf("the public default is %s and the runner's is %s", DefaultLate, flow.DefaultLate)
	}
	on := []Change{Bool{Device: "hall", Capability: Open}}
	valve := Bool{Device: "valve", Capability: On}
	for name, tc := range map[string]struct {
		decide func(*Home, *Flow)
		want   map[string]time.Duration
	}{
		// Flows below the override line make Normal claims, so a late one gives up.
		"normal": {
			func(h *Home, _ *Flow) { h.Decide(valve).OverrideFor(time.Hour).Flows().Default(true) },
			map[string]time.Duration{"normal": DefaultLate, "own": time.Hour, "leak": DefaultLate},
		},
		// Above it, a flow acts however late it is.
		"safety": {
			func(h *Home, _ *Flow) { h.Decide(valve).NoOverride().Flows().Default(true) },
			map[string]time.Duration{"normal": neverLate, "own": neverLate, "leak": neverLate},
		},
		// A flow of its own above the override line is the only one that does.
		"one flow": {
			func(h *Home, leak *Flow) { h.Decide(valve).Flows(leak).OverrideFor(time.Hour).Flows().Default(true) },
			map[string]time.Duration{"normal": DefaultLate, "own": time.Hour, "leak": neverLate},
		},
	} {
		home := NewHome(Options{})
		home.Flow("normal", nothing).When(on...)
		home.Flow("own", nothing).When(on...).Late(time.Hour)
		tc.decide(home, home.Flow("leak", nothing).When(on...))
		rules, _, err := home.compile()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			if want, ok := tc.want[r.Key.Name]; ok && r.StartingDeadline != want {
				t.Errorf("%s: %s: deadline %s, want %s", name, r.Key.Name, r.StartingDeadline, want)
			}
		}
	}
}
