package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/sun"
)

func goKey(name string) ident.ID { return ident.ID{Name: name} }

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
		Device: "bathroom-fan", Capability: capability.On, Value: true,
		Window: &Window{Start: At{Time: "06:00"}, End: At{Time: "12:15"}},
		When:   humidity, OnDelay: 2 * time.Minute, OffDelay: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	got := strings.Join(transcript(t, r), "\n")
	for _, want := range []string{
		"before onDelay: active=0", "after onDelay: bathroom-fan on=true", "after onDelay: active=1",
		"before offDelay: active=1", "after offDelay: bathroom-fan on=false", "after offDelay: active=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}

func TestGoFlowRuns(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	c := flowRule(t, "motion-light", steps{
		Changes:   []Change{{Device: "hallway-motion", Capability: capability.Occupied}},
		Condition: func(v Vars) (bool, error) { return v["trigger"] != nil && v["now"] != nil, nil },
		Do: []Action{
			Set{Device: "hallway-light", Capability: capability.On, Value: true},
			Delay(time.Minute),
			Set{Device: "hallway-light", Capability: capability.On, Value: false},
		},
	})
	h.e.Load(c)
	h.motion(true)
	h.expectCalls(call{device: "hallway-light", capability: capability.On, value: true})
	h.eventually("the delay's timer", h.clock.HasWaiters)
	h.clock.Step(time.Minute)
	h.expectCalls(
		call{device: "hallway-light", capability: capability.On, value: true},
		call{device: "hallway-light", capability: capability.On, value: false},
	)
}

func TestGoConditionErrorCountsAsAConditionError(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	c := flowRule(t, "motion-light", steps{
		Changes:   []Change{{Device: "hallway-motion", Capability: capability.Occupied}},
		Condition: func(Vars) (bool, error) { return false, fmt.Errorf("not yet") },
		Do:        []Action{Set{Device: "hallway-light", Capability: capability.On, Value: true}},
	})
	h.e.Load(c)
	h.motion(true)
	h.eventually("a condition error", func() bool { return h.runs(ResultConditionError) == 1 })
	if got := h.setter.got(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

func TestGoConditionPanicIsAConditionError(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	c := flowRule(t, "motion-light", steps{
		Changes:   []Change{{Device: "hallway-motion", Capability: capability.Occupied}},
		Condition: func(Vars) (bool, error) { panic("no reading yet") },
		Do:        []Action{Set{Device: "hallway-light", Capability: capability.On, Value: true}},
	})
	h.e.Load(c)
	h.motion(true)
	h.eventually("a condition error", func() bool { return h.runs(ResultConditionError) == 1 })
}

func TestGoHoldWhenPanicKeepsItsState(t *testing.T) {
	r := startRig(t, local("2026-06-15 12:00 BST"), nil, nil, nil)
	c, err := NewHold(goKey("fan"), Hold{
		Device: "bathroom-fan", Capability: capability.On, Value: true,
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
	rule := func(generation int64, value bool) *Compiled {
		return flowRule(t, "motion-light", steps{
			Changes: []Change{{Device: "hallway-motion", Capability: capability.Occupied}}, Generation: generation,
			Do: []Action{Set{Device: "hallway-light", Capability: capability.On, Value: value}},
		})
	}
	h.e.Load(rule(0, true))
	h.e.Load(rule(2, false))
	h.motion(true)
	h.expectCalls(call{device: "hallway-light", capability: capability.On, value: false})
}

func TestGoTimerIsNotMissedBeforeItLoads(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 15, 20, 0, 0, 0, london))
	c := flowRule(t, "motion-light", steps{
		Timers: []Timer{At{Time: "19:00"}}, StartingDeadline: 2 * time.Hour,
		Do: []Action{Set{Device: "hallway-light", Capability: capability.On, Value: true}},
	})
	h.e.Load(c)
	h.eventually("the engine to arm", h.clock.HasWaiters)
	if got, missed := h.setter.got(), h.runs(ResultMissed); len(got) != 0 || missed != 0 {
		t.Fatalf("calls = %v, missed = %v, want neither", got, missed)
	}
}

func TestGoRuleKeepsNoCallerSlices(t *testing.T) {
	changes := []Change{{Device: "a", Capability: capability.Occupied}}
	days := []time.Weekday{time.Monday}
	c, err := NewTrigger(goKey("flow"), Trigger{Changes: changes, Timers: []Timer{At{Time: "07:30", Days: days}}})
	if err != nil {
		t.Fatal(err)
	}
	changes[0].Device, days[0] = "b", time.Tuesday
	if !c.matches("a", "", nil, capability.Occupied, nil) || c.timers[0].days[0] != time.Monday {
		t.Errorf("the compiled rule changed with its input: %+v", c)
	}
}

func TestNewTriggerMatches(t *testing.T) {
	c, err := NewTrigger(goKey("flow"), Trigger{
		Changes: []Change{{Room: "hall", Capability: capability.Occupied}},
		Timers:  []Timer{At{Time: "07:30", Days: []time.Weekday{time.Monday}}, SunTime{Event: sun.Sunset, Offset: time.Hour}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.matches("any", "hall", nil, capability.Occupied, nil) || c.matches("any", "attic", nil, capability.Occupied, nil) {
		t.Error("the change trigger doesn't match as written")
	}
	if len(c.timers) != 2 || c.timers[0].minutes != 7*60+30 || c.timers[1].event != sun.Sunset {
		t.Errorf("timers = %+v", c.timers)
	}
}

// A room's change limited to device types fires only for a device of one of them, and one without
// types for any device in the room.
func TestARoomChangeOfTypesMatchesOnlyThoseTypes(t *testing.T) {
	lights := []devicetype.Type{devicetype.OnOffLight, devicetype.DimmableLight}
	c, err := NewTrigger(goKey("flow"), Trigger{Changes: []Change{{Room: "hall", DeviceTypes: lights, Capability: capability.On}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		room  string
		types []devicetype.Type
		want  bool
	}{
		"a light":                 {"hall", []devicetype.Type{devicetype.DimmableLight}, true},
		"a plug":                  {"hall", []devicetype.Type{devicetype.OnOffPlugInUnit}, false},
		"a heater":                {"hall", []devicetype.Type{devicetype.Thermostat}, false},
		"a device with no type":   {"hall", nil, false},
		"a light in another room": {"attic", []devicetype.Type{devicetype.OnOffLight}, false},
	} {
		if got := c.matches("d", tc.room, tc.types, capability.On, nil); got != tc.want {
			t.Errorf("%s: matches = %v, want %v", name, got, tc.want)
		}
	}
}

func TestGoRulesAreChecked(t *testing.T) {
	on := []Change{{Device: "d", Capability: capability.Occupied}}
	flow := func(f Flow) error {
		f.Body = noop
		_, err := NewFlow(goKey("r"), f)
		return err
	}
	tr := func(t Trigger) Flow { return Flow{Trigger: t} }
	do := func(a Action) error { _, err := a.action(); return err }
	hold := func(h Hold) error { _, err := NewHold(goKey("r"), h); return err }
	always := func(Vars) (bool, error) { return true, nil }
	for _, tc := range []struct {
		err  error
		want string
	}{
		{flow(Flow{}), "a trigger is required"},
		{flow(tr(Trigger{Changes: []Change{{Capability: capability.Occupied}}})), "changes[0]: exactly one of device or room"},
		{flow(tr(Trigger{Changes: []Change{{Device: "d", Capability: "nope"}}})), `unknown capability "nope"`},
		{flow(tr(Trigger{Changes: []Change{{Room: "hall", DeviceTypes: []devicetype.Type{"Toaster"}, Capability: capability.On}}})), `unknown device type "Toaster"`},
		{flow(tr(Trigger{Changes: []Change{{Device: "d", DeviceTypes: []devicetype.Type{devicetype.OnOffLight}, Capability: capability.On}}})), "device types limit a room"},
		{flow(tr(Trigger{Timers: []Timer{At{Time: "25:00"}}})), `timers[0]: time "25:00" is not HH:MM`},
		{flow(tr(Trigger{Timers: []Timer{SunTime{Event: sun.Sunrise, Offset: 7 * time.Hour}}})), "not between -6h and 6h"},
		{flow(tr(Trigger{Changes: on, StartingDeadline: -1})), "startingDeadline must not be negative"},
		{flow(Flow{Changes: on, Mode: "Queued"}), `unknown mode "Queued"`},
		{flow(tr(Trigger{Timers: []Timer{At{Time: "19:00", Days: []time.Weekday{7}}}})), "unknown day 7"},
		{flow(tr(Trigger{Timers: []Timer{SunTime{Event: sun.Sunset, Days: []time.Weekday{-1}}}})), "unknown day -1"},
		{do(Delay(0)), "delay must be positive"},
		{do(Release("")), "release needs a rule"},
		{do(Set{Device: "d", Capability: capability.Temperature, Value: 1.0}), "read-only"},
		{do(Set{Device: "d", Capability: capability.On, Value: "on"}), "takes a bool"},
		{do(Set{Device: "d", Capability: capability.On, Value: true, For: time.Hour, Until: UntilReleased}), "can't be combined"},
		{do(Set{Device: "d", Capability: capability.On, Value: true, For: -time.Hour}), "for must be positive"},
		{do(Set{Device: "d", Capability: capability.On, Value: true, Priority: "Top"}), `unknown priority "Top"`},
		{hold(Hold{Device: "d", Capability: capability.On, Value: true, Window: &Window{Start: At{Time: "06:00"}, End: At{Time: "07:00"}, Days: []time.Weekday{9}}}), "unknown day 9"},
		{hold(Hold{Device: "d", Capability: capability.On, Value: true}), "a window or when is required"},
		{hold(Hold{Capability: capability.On, Value: true, When: always}), "exactly one of device, devices or room"},
		{hold(Hold{Device: "d", Capability: capability.On, Value: true, Latch: true, Window: &Window{Start: At{Time: "06:00"}, End: At{Time: "07:00"}}}), "latch needs when"},
		{hold(Hold{Device: "d", Capability: capability.On, Value: true, When: always, OnDelay: -1}), "onDelay must not be negative"},
		{hold(Hold{Device: "d", Capability: capability.On, Value: true, Window: &Window{End: At{Time: "07:00"}}}), "hold.window: start: no time"},
		{hold(Hold{Device: "d", Capability: capability.On, Value: true, Window: &Window{Start: At{Time: "06:00", Days: []time.Weekday{time.Monday}}, End: At{Time: "07:00"}}}), "days belong on the window"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("error = %v, want it to contain %q", tc.err, tc.want)
		}
	}
}

// A room rule can be limited to the heaters, so oikade check accepts Thermostat, and still refuses
// what isn't a type.
func TestARoomRuleTakesThermostat(t *testing.T) {
	heaters := Hold{Room: "study", DeviceTypes: []devicetype.Type{devicetype.Thermostat}, Capability: capability.On, Value: false,
		Window: &Window{Start: At{Time: "23:00"}, End: At{Time: "06:00"}}}
	if _, err := NewHold(ident.ID{Name: "study-off"}, heaters); err != nil {
		t.Errorf("a hold on the study's heaters: %v", err)
	}
	if _, err := (Set{Room: "study", DeviceTypes: []devicetype.Type{devicetype.Thermostat}, Capability: capability.On, Value: false}).action(); err != nil {
		t.Errorf("a set on the study's heaters: %v", err)
	}
	if _, err := (Set{Room: "study", DeviceTypes: []devicetype.Type{"Toaster"}, Capability: capability.On, Value: false}).action(); err == nil {
		t.Error("a set limited to Toaster was accepted")
	}
}
