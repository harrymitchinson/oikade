package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/sun"
)

func goKey(name string) ident.ID { return ident.ID{Namespace: ns, Name: name} }

func humidity(v Vars) (bool, error) {
	devices, _ := v["devices"].(map[string]map[string]any)
	h, ok := devices["bathroom-sensor"][capability.Humidity].(float64)
	if !ok {
		return false, fmt.Errorf("no humidity yet")
	}
	return h > 70, nil
}

// transcript drives a rig through a day and records every write, and whether the hold is active,
// after each step.
func transcript(t *testing.T, r *rig) []string {
	t.Helper()
	var got []string
	seen := 0
	step := func(label string) {
		r.settle()
		calls := r.setter.got()
		for _, c := range calls[seen:] {
			got = append(got, fmt.Sprintf("%s: %s %s=%v", label, c.device, c.capability, c.value))
		}
		seen = len(calls)
		got = append(got, fmt.Sprintf("%s: active=%v", label, r.active("fan")))
	}
	step("start")
	r.change("bathroom-sensor", capability.Humidity, 75.0)
	step("humid")
	r.at("2026-06-15 12:01 BST")
	step("before onDelay")
	r.at("2026-06-15 12:02 BST")
	step("after onDelay")
	r.change("bathroom-sensor", capability.Humidity, 50.0)
	step("dry")
	r.at("2026-06-15 12:06 BST")
	step("before offDelay")
	r.at("2026-06-15 12:07 BST")
	step("after offDelay")
	r.change("bathroom-sensor", capability.Humidity, 90.0)
	r.at("2026-06-15 12:10 BST")
	step("humid again")
	r.at("2026-06-15 12:20 BST")
	step("window closed")
	return got
}

func TestGoHoldRunsItsDelaysAndWindow(t *testing.T) {
	r := startRig(t, local("2026-06-15 12:00 BST"), nil, map[string]map[string]any{"bathroom-sensor": {capability.Humidity: 40.0}}, nil)
	c, err := NewHold(goKey("fan"), Hold{
		Device: "bathroom-fan", Capability: capability.SwitchOn, Value: true,
		Window: &Window{Start: At{Time: "06:00"}, End: At{Time: "12:15"}},
		When:   humidity, OnDelay: 2 * time.Minute, OffDelay: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	got := strings.Join(transcript(t, r), "\n")
	for _, want := range []string{
		"before onDelay: active=0", "after onDelay: bathroom-fan switch.on=true", "after onDelay: active=1",
		"before offDelay: active=1", "after offDelay: bathroom-fan switch.on=false", "after offDelay: active=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}

func TestGoMomentRuns(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	c, err := NewMoment(goKey("motion-light"), Moment{
		Changes:   []Change{{Device: "hallway-motion", Capability: capability.Occupancy}},
		Condition: func(v Vars) (bool, error) { return v["trigger"] != nil && v["now"] != nil, nil },
		Do: []Action{
			Set{Device: "hallway-light", Capability: capability.LightOn, Value: true},
			Delay(time.Minute),
			Set{Device: "hallway-light", Capability: capability.LightOn, Value: false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.e.Load(c)
	h.motion(true)
	h.expectCalls(call{device: "hallway-light", capability: capability.LightOn, value: true})
	h.eventually("the delay's timer", h.clock.HasWaiters)
	h.clock.Step(time.Minute)
	h.expectCalls(
		call{device: "hallway-light", capability: capability.LightOn, value: true},
		call{device: "hallway-light", capability: capability.LightOn, value: false},
	)
}

func TestGoConditionErrorCountsAsAConditionError(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	c, err := NewMoment(goKey("motion-light"), Moment{
		Changes:   []Change{{Device: "hallway-motion", Capability: capability.Occupancy}},
		Condition: func(Vars) (bool, error) { return false, fmt.Errorf("not yet") },
		Do:        []Action{Set{Device: "hallway-light", Capability: capability.LightOn, Value: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.e.Load(c)
	h.motion(true)
	h.eventually("a condition error", func() bool { return h.runs(ResultConditionError) == 1 })
	if got := h.setter.got(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

func TestGoConditionPanicIsAConditionError(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	c, err := NewMoment(goKey("motion-light"), Moment{
		Changes:   []Change{{Device: "hallway-motion", Capability: capability.Occupancy}},
		Condition: func(Vars) (bool, error) { panic("no reading yet") },
		Do:        []Action{Set{Device: "hallway-light", Capability: capability.LightOn, Value: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.e.Load(c)
	h.motion(true)
	h.eventually("a condition error", func() bool { return h.runs(ResultConditionError) == 1 })
}

func TestGoHoldWhenPanicKeepsItsState(t *testing.T) {
	r := startRig(t, local("2026-06-15 12:00 BST"), nil, nil, nil)
	c, err := NewHold(goKey("fan"), Hold{
		Device: "bathroom-fan", Capability: capability.SwitchOn, Value: true,
		When: func(Vars) (bool, error) { panic("no reading yet") },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	r.settle()
	if got := r.setter.got(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

func TestGoRuleReplacedByAHigherGeneration(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	moment := func(generation int64, value bool) *Compiled {
		c, err := NewMoment(goKey("motion-light"), Moment{
			Changes: []Change{{Device: "hallway-motion", Capability: capability.Occupancy}}, Generation: generation,
			Do: []Action{Set{Device: "hallway-light", Capability: capability.LightOn, Value: value}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	h.e.Load(moment(0, true))
	h.e.Load(moment(2, false))
	h.motion(true)
	h.expectCalls(call{device: "hallway-light", capability: capability.LightOn, value: false})
}

func TestGoTimerIsNotMissedBeforeItLoads(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	c, err := NewMoment(goKey("motion-light"), Moment{
		Timers: []Timer{At{Time: "19:00"}}, StartingDeadline: 2 * time.Hour,
		Do: []Action{Set{Device: "hallway-light", Capability: capability.LightOn, Value: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.e.Load(c)
	h.eventually("the engine to arm", h.clock.HasWaiters)
	if got, missed := h.setter.got(), h.runs(ResultMissed); len(got) != 0 || missed != 0 {
		t.Fatalf("calls = %v, missed = %v, want neither", got, missed)
	}
}

func TestGoRuleKeepsNoCallerSlices(t *testing.T) {
	changes := []Change{{Device: "a", Capability: capability.Occupancy}}
	days := []time.Weekday{time.Monday}
	c, err := NewTrigger(goKey("flow"), Trigger{Changes: changes, Timers: []Timer{At{Time: "07:30", Days: days}}})
	if err != nil {
		t.Fatal(err)
	}
	changes[0].Device, days[0] = "b", time.Tuesday
	if !c.matches("a", "", capability.Occupancy) || c.timers[0].days[0] != time.Monday {
		t.Errorf("the compiled rule changed with its input: %+v", c)
	}
}

func TestNewTriggerMatches(t *testing.T) {
	c, err := NewTrigger(goKey("flow"), Trigger{
		Changes: []Change{{Room: "hall", Capability: capability.Occupancy}},
		Timers:  []Timer{At{Time: "07:30", Days: []time.Weekday{time.Monday}}, SunTime{Event: sun.Sunset, Offset: time.Hour}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.matches("any", "hall", capability.Occupancy) || c.matches("any", "attic", capability.Occupancy) {
		t.Error("the change trigger doesn't match as written")
	}
	if len(c.timers) != 2 || c.timers[0].minutes != 7*60+30 || c.timers[1].event != sun.Sunset {
		t.Errorf("timers = %+v", c.timers)
	}
}

func TestGoRulesAreChecked(t *testing.T) {
	on := []Change{{Device: "d", Capability: capability.Occupancy}}
	set := Set{Device: "d", Capability: capability.SwitchOn, Value: true}
	moment := func(m Moment) error { _, err := NewMoment(goKey("r"), m); return err }
	hold := func(h Hold) error { _, err := NewHold(goKey("r"), h); return err }
	always := func(Vars) (bool, error) { return true, nil }
	for _, tc := range []struct {
		err  error
		want string
	}{
		{moment(Moment{Do: []Action{set}}), "a trigger is required"},
		{moment(Moment{Changes: on}), "do is required"},
		{moment(Moment{Changes: []Change{{Capability: capability.Occupancy}}, Do: []Action{set}}), "changes[0]: exactly one of device or room"},
		{moment(Moment{Changes: []Change{{Device: "d", Capability: "nope"}}, Do: []Action{set}}), `unknown capability "nope"`},
		{moment(Moment{Timers: []Timer{At{Time: "25:00"}}, Do: []Action{set}}), `timers[0]: time "25:00" is not HH:MM`},
		{moment(Moment{Timers: []Timer{SunTime{Event: sun.Sunrise, Offset: 7 * time.Hour}}, Do: []Action{set}}), "not between -6h and 6h"},
		{moment(Moment{Changes: on, StartingDeadline: -1, Do: []Action{set}}), "startingDeadline must not be negative"},
		{moment(Moment{Changes: on, Mode: "Queued", Do: []Action{set}}), `unknown mode "Queued"`},
		{moment(Moment{Changes: on, Do: []Action{Delay(0)}}), "do[0]: delay must be positive"},
		{moment(Moment{Changes: on, Do: []Action{Release("")}}), "release needs a rule"},
		{moment(Moment{Changes: on, Do: []Action{nil}}), "do[0]: no action"},
		{moment(Moment{Changes: on, Do: []Action{Set{Device: "d", Capability: capability.Temperature, Value: 1.0}}}), "read-only"},
		{moment(Moment{Changes: on, Do: []Action{Set{Device: "d", Capability: capability.SwitchOn, Value: "on"}}}), "do[0]: "},
		{moment(Moment{Changes: on, Do: []Action{Set{Device: "d", Capability: capability.SwitchOn, Value: true, For: time.Hour, Until: UntilReleased}}}), "can't be combined"},
		{moment(Moment{Changes: on, Do: []Action{Set{Device: "d", Capability: capability.SwitchOn, Value: true, For: -time.Hour}}}), "for must be positive"},
		{moment(Moment{Changes: on, Do: []Action{Set{Device: "d", Capability: capability.SwitchOn, Value: true, Priority: "Top"}}}), `unknown priority "Top"`},
		{moment(Moment{Timers: []Timer{At{Time: "19:00", Days: []time.Weekday{7}}}, Do: []Action{set}}), "unknown day 7"},
		{moment(Moment{Timers: []Timer{SunTime{Event: sun.Sunset, Days: []time.Weekday{-1}}}, Do: []Action{set}}), "unknown day -1"},
		{hold(Hold{Device: "d", Capability: capability.SwitchOn, Value: true, Window: &Window{Start: At{Time: "06:00"}, End: At{Time: "07:00"}, Days: []time.Weekday{9}}}), "unknown day 9"},
		{hold(Hold{Device: "d", Capability: capability.SwitchOn, Value: true}), "a window or when is required"},
		{hold(Hold{Capability: capability.SwitchOn, Value: true, When: always}), "exactly one of device or room"},
		{hold(Hold{Device: "d", Capability: capability.SwitchOn, Value: true, Latch: true, Window: &Window{Start: At{Time: "06:00"}, End: At{Time: "07:00"}}}), "latch needs when"},
		{hold(Hold{Device: "d", Capability: capability.SwitchOn, Value: true, When: always, OnDelay: -1}), "onDelay must not be negative"},
		{hold(Hold{Device: "d", Capability: capability.SwitchOn, Value: true, Window: &Window{End: At{Time: "07:00"}}}), "hold.window: start: no time"},
		{hold(Hold{Device: "d", Capability: capability.SwitchOn, Value: true, Window: &Window{Start: At{Time: "06:00", Days: []time.Weekday{time.Monday}}, End: At{Time: "07:00"}}}), "days belong on the window"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("error = %v, want it to contain %q", tc.err, tc.want)
		}
	}
}
