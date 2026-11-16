package oikade

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/flow"
)

func TestCompileChecksFlows(t *testing.T) {
	run := func(*FlowRun) error { return nil }
	on := []Change{Bool{Device: "hall", Capability: Open}}
	for name, tc := range map[string]struct {
		flow Flow
		want string
	}{
		"no name":    {NewFlow("", run).On(on...), "needs a name"},
		"no trigger": {NewFlow("a", run), "a trigger is required"},
		"no run":     {NewFlow("a", nil).On(on...), "needs a body"},
		"negative":   {NewFlow("a", run).On(on...).Late(-time.Minute), "must not be negative"},
		"bad time":   {NewFlow("a", run).At(Clock("25:00")), "timers[0]"},
		"bad change": {
			NewFlow("a", run).On(Bool{Device: "hall", Capability: "no.such"}), "unknown capability",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := NewHome(Options{}, tc.flow).compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}

	_, _, err := NewHome(Options{},
		NewFlow("a", run).On(on...),
		NewMoment("a", Wait(time.Second)).On(on...),
	).compile()
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("a flow and a moment sharing a name: %v", err)
	}
}

func TestLateness(t *testing.T) {
	if DefaultLate != flow.DefaultLate {
		t.Fatalf("the public default is %s and the runner's is %s", DefaultLate, flow.DefaultLate)
	}
	on := []Change{Bool{Device: "hall", Capability: Open}}
	valve := Bool{Device: "valve", Capability: On}
	for name, tc := range map[string]struct {
		decide func(*Home)
		want   map[string]time.Duration
	}{
		// Flows below the override line make Normal claims, so a late one gives up.
		"normal": {
			func(h *Home) { h.Decide(valve).OverrideFor(time.Hour).Flows().Default(true) },
			map[string]time.Duration{
				"normal": DefaultLate, "own": time.Hour, "moment": DefaultLate, "valve-moment": DefaultLate,
			},
		},
		// Above it, the valve's moment and every flow act however late they are.
		"safety": {
			func(h *Home) { h.Decide(valve).NoOverride().Flows().Default(true) },
			map[string]time.Duration{
				"normal": neverLate, "own": neverLate, "moment": DefaultLate, "valve-moment": neverLate,
			},
		},
	} {
		home := NewHome(Options{},
			NewFlow("normal", func(*FlowRun) error { return nil }).On(on...),
			NewFlow("own", func(*FlowRun) error { return nil }).On(on...).Late(time.Hour),
			NewMoment("moment", Wait(time.Second)).On(on...),
			NewMoment("valve-moment", valve.To(false).For(time.Hour)).On(on...),
		)
		tc.decide(home)
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
