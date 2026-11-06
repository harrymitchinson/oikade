package framework

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/flow"
)

func TestCompileChecksFlows(t *testing.T) {
	run := func(*FlowRun) error { return nil }
	on := []Change{{Device: "hall", Capability: "sensor.contact-open"}}
	for name, tc := range map[string]struct {
		flow Flow
		want string
	}{
		"no name":    {Flow{On: on, Run: run}, "needs a name"},
		"no trigger": {Flow{Name: "a", Run: run}, "a trigger is required"},
		"no run":     {Flow{Name: "a", On: on}, "needs a body"},
		"negative":   {Flow{Name: "a", On: on, Run: run, Late: -time.Minute}, "must not be negative"},
		"bad time":   {Flow{Name: "a", At: []string{"25:00"}, Run: run}, "timers[0]"},
		"bad change": {
			Flow{Name: "a", On: []Change{{Device: "hall", Capability: "no.such"}}, Run: run}, "unknown capability",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Home{Flows: []Flow{tc.flow}}.compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}

	_, _, err := Home{
		Flows:   []Flow{{Name: "a", On: on, Run: run}},
		Moments: []Moment{{Name: "a", On: on, Do: []Action{Wait(time.Second)}}},
	}.compile()
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("a flow and a moment sharing a name: %v", err)
	}
}

func TestLateness(t *testing.T) {
	if DefaultLate != flow.DefaultLate {
		t.Fatalf("the public default is %s and the runner's is %s", DefaultLate, flow.DefaultLate)
	}
	on := []Change{{Device: "hall", Capability: "sensor.contact-open"}}
	rules, flows, err := Home{
		Flows: []Flow{
			{Name: "normal", On: on, Run: func(*FlowRun) error { return nil }},
			{Name: "safety", On: on, Priority: Safety, Run: func(*FlowRun) error { return nil }},
			{Name: "own", On: on, Late: time.Hour, Run: func(*FlowRun) error { return nil }},
		},
		Moments: []Moment{
			{Name: "moment", On: on, Do: []Action{Wait(time.Second)}},
			{
				Name: "safety-moment", On: on,
				Do: []Action{Set{Device: "valve", Capability: "switch.on", Value: false, Priority: Safety}},
			},
		},
	}.compile()
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
