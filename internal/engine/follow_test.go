package engine

import (
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/ident"
)

// followOf is a Follow of one device's capability, times by.
func followOf(device, name string, by float64) func(Vars) (float64, error) {
	return func(v Vars) (float64, error) {
		devices, _ := v["devices"].(map[string]map[string]any)
		x, ok := devices[device][name]
		if !ok {
			return 0, errors.New(device + " " + name + " isn't reported yet")
		}
		return capability.Float(x) * by, nil
	}
}

var blindFollows = Hold{Device: "bay-blind", Capability: capability.Position, Follow: followOf("outdoor-temp", capability.Temperature, 10)}

func TestAFollowedHoldWritesOnlyWhenTheRoundedValueChanges(t *testing.T) {
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, nil, nil, holdRule(t, "blind", blindFollows))
	r.expect()
	r.change("outdoor-temp", capability.Temperature, 2.0)
	r.expect(call{"bay-blind", capability.Position, int64(20)})
	r.change("outdoor-temp", capability.Temperature, 2.04)
	r.expect(call{"bay-blind", capability.Position, int64(20)})
	// 20.6 rounds to 21, but it's within three quarters of a step of what was written.
	r.change("outdoor-temp", capability.Temperature, 2.06)
	r.expect(call{"bay-blind", capability.Position, int64(20)})
	r.change("outdoor-temp", capability.Temperature, 2.08)
	r.expect(call{"bay-blind", capability.Position, int64(20)}, call{"bay-blind", capability.Position, int64(21)})
	// Back across the edge, a wobble isn't a write either.
	r.change("outdoor-temp", capability.Temperature, 2.04)
	r.change("outdoor-temp", capability.Temperature, 2.06)
	r.expect(call{"bay-blind", capability.Position, int64(20)}, call{"bay-blind", capability.Position, int64(21)})
	r.change("outdoor-temp", capability.Temperature, 20.0)
	// Kept within the capability's bounds.
	r.expect(call{"bay-blind", capability.Position, int64(20)}, call{"bay-blind", capability.Position, int64(21)},
		call{"bay-blind", capability.Position, int64(100)})
}

func TestAFollowedHoldsLastWriteSurvivesARestart(t *testing.T) {
	states := newMemoryStates[HoldState]()
	values := map[string]map[string]any{"outdoor-temp": {capability.Temperature: 1.5}}
	r := startRig(t, local("2026-10-21 08:00 BST"), states, values, nil, holdRule(t, "blind", blindFollows))
	r.expect(call{"bay-blind", capability.Position, int64(15)})
	r.stop()
	values["outdoor-temp"][capability.Temperature] = 1.52
	r = startRig(t, local("2026-10-21 09:00 BST"), states, values, nil, holdRule(t, "blind", blindFollows))
	r.expect()
	r.change("outdoor-temp", capability.Temperature, 1.6)
	r.expect(call{"bay-blind", capability.Position, int64(16)})
}

func TestAFollowedHoldsStepRounds(t *testing.T) {
	h := blindFollows
	h.Step = 5
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, map[string]map[string]any{"outdoor-temp": {capability.Temperature: 2.2}}, nil,
		holdRule(t, "blind", h))
	r.expect(call{"bay-blind", capability.Position, int64(20)})
	r.change("outdoor-temp", capability.Temperature, 1.8)
	r.expect(call{"bay-blind", capability.Position, int64(20)})
	r.change("outdoor-temp", capability.Temperature, 2.6)
	r.expect(call{"bay-blind", capability.Position, int64(20)}, call{"bay-blind", capability.Position, int64(25)})
}

// A Follow that gives NaN is as one whose reading isn't known: nothing is written.
func TestAFollowThatGivesNaNWritesNothing(t *testing.T) {
	h := Hold{Device: "bay-blind", Capability: capability.Position, Follow: func(Vars) (float64, error) { return math.NaN(), nil }}
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, map[string]map[string]any{"outdoor-temp": {capability.Temperature: 2.0}}, nil,
		holdRule(t, "blind", h))
	r.change("outdoor-temp", capability.Temperature, 3.0)
	r.expect()
}

func TestFollowNeedsANumberWithAStep(t *testing.T) {
	for _, h := range []Hold{
		{Device: "pump", Capability: capability.On, Follow: followOf("water-temp", capability.Temperature, 1)},
		{Device: "water-temp", Capability: capability.Temperature, Follow: followOf("water-temp", capability.Temperature, 1)},
		{Device: "bay-blind", Capability: capability.Position, Step: -1, Follow: followOf("water-temp", capability.Temperature, 1)},
		{Device: "bay-blind", Capability: capability.Position, Follow: followOf("water-temp", capability.Temperature, 1), When: func(Vars) (bool, error) { return true, nil }},
	} {
		if _, err := NewHold(ident.ID{Name: "bad"}, h); err == nil {
			t.Errorf("%+v compiled", h)
		}
	}
}

func TestAnAverageIsTimeWeighted(t *testing.T) {
	a := Average{Device: "water-temp", Capability: capability.Temperature, Over: time.Hour}
	var s AverageState
	at := local("2026-10-21 08:00 BST")
	ten, twenty := 10.0, 20.0
	s.Last, s.LastAt = &ten, at
	s.add(a, at.Add(45*time.Minute))
	s.Last = &twenty
	if v, _ := s.at(a, at.Add(time.Hour)); math.Abs(v-12.5) > 0.01 {
		t.Errorf("average at 09:00 = %v, want 12.5", v)
	}
	// A window later, the 10s have gone.
	if v, _ := s.at(a, at.Add(2*time.Hour)); math.Abs(v-20) > 0.01 {
		t.Errorf("average at 10:00 = %v, want 20", v)
	}
	if _, ok := (AverageState{}).at(a, at); ok {
		t.Error("an average with no reading has a value")
	}
}

func TestAnAverageSurvivesARestart(t *testing.T) {
	avg := Average{Device: "water-temp", Capability: capability.Temperature, Over: time.Hour}
	blind := Hold{Device: "bay-blind", Capability: capability.Position, Follow: followOf("water-temp", avg.Var(), 1)}
	opts := rigOptions{
		now: local("2026-10-21 08:00 BST"), holds: newMemoryStates[HoldState](), averages: []Average{avg},
		averageStates: newMemoryStates[AverageState](), values: map[string]map[string]any{"water-temp": {capability.Temperature: 10.0}},
	}
	r := startRigWith(t, opts, holdRule(t, "blind", blind))
	r.expect(call{"bay-blind", capability.Position, int64(10)})
	r.at("2026-10-21 08:30 BST")
	r.change("water-temp", capability.Temperature, 25.0)
	// A second reading in the same bucket is saved as the engine stops.
	r.change("water-temp", capability.Temperature, 20.0)
	r.stop()
	opts.now, opts.values = local("2026-10-21 09:00 BST"), map[string]map[string]any{"water-temp": {capability.Temperature: 20.0}}
	r = startRigWith(t, opts, holdRule(t, "blind", blind))
	// Half an hour at 10 and half at 20, rather than a fresh 20.
	r.expect(call{"bay-blind", capability.Position, int64(15)})
}

// A budget's when that can't be told counts as false, so the budget stops rather than running on
// past its slot.
func TestABudgetsUnknownWhenStopsIt(t *testing.T) {
	var unknown atomic.Bool
	h := pumpBudget
	h.When = func(Vars) (bool, error) {
		if unknown.Load() {
			return false, errors.New("the sensor hasn't reported")
		}
		return true, nil
	}
	r := startRigWith(t, rigOptions{
		now: local("2026-10-21 21:59 BST"), control: map[string][]control.Entry{"pump": {pumpEntry()}},
		values: map[string]map[string]any{"water-temp": {capability.Temperature: 10.0}},
	}, holdRule(t, "pump-daily", h))
	r.report("water-temp", capability.Temperature, 10.0)
	r.at("2026-10-21 22:30 BST")
	r.expect(call{"pump", capability.On, false}, call{"pump", capability.On, true})
	unknown.Store(true)
	r.at("2026-10-21 23:01 BST") // after the pump's MinimumOn, and before its planned slot ends
	r.expect(call{"pump", capability.On, false}, call{"pump", capability.On, true}, call{"pump", capability.On, false})
}
