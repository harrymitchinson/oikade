package framework

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/flow"
)

func TestCompileChecksFlows(t *testing.T) {
	run := func(*FlowRun) error { return nil }
	on := []Change{Handle[bool]{Device: "hall", Capability: Open}}
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
			NewFlow("a", run).On(Handle[bool]{Device: "hall", Capability: "no.such"}), "unknown capability",
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
	on := []Change{Handle[bool]{Device: "hall", Capability: Open}}
	rules, flows, err := NewHome(Options{},
		NewFlow("normal", func(*FlowRun) error { return nil }).On(on...),
		NewFlow("safety", func(*FlowRun) error { return nil }).On(on...).Priority(Safety),
		NewFlow("own", func(*FlowRun) error { return nil }).On(on...).Late(time.Hour),
		NewMoment("moment", Wait(time.Second)).On(on...),
		NewMoment("safety-moment", Handle[bool]{Device: "valve", Capability: On}.To(false).Priority(Safety)).
			On(on...),
	).compile()
	if err != nil {
		t.Fatal(err)
	}
	if !flows["safety"].Safety || flows["normal"].Safety {
		t.Fatal("only a Safety flow never expires")
	}
	want := map[string]time.Duration{
		"normal": DefaultLate, "safety": neverLate, "own": time.Hour, "moment": DefaultLate, "safety-moment": neverLate,
	}
	for _, r := range rules {
		if r.StartingDeadline != want[r.Key.Name] {
			t.Errorf("%s: deadline %s", r.Key.Name, r.StartingDeadline)
		}
	}
}
