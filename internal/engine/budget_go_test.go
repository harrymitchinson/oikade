package engine

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/schedule"
)

// goPumpBudget is pumpBudget, written in Go.
func goPumpBudget() *schedule.Spec {
	twelve, sixteen := 12.0, 16.0
	return &schedule.Spec{
		Bands: &schedule.BandSpec{
			Device: "water-temp", Capability: capability.Temperature, MaxAge: time.Hour, MissingMinutes: 240,
			Ranges: []schedule.Band{{Below: &twelve, Minutes: 120}, {Below: &sixteen, Minutes: 180}, {Minutes: 300}},
		},
		Prefer: []schedule.PreferSpec{{Window: schedule.WindowSpec{Start: "22:00", End: "06:00"}, Score: 3}},
		Forbid: []schedule.WindowSpec{{Start: "07:00", End: "08:30"}},
	}
}

func goPumpHold(t *testing.T) *Compiled {
	t.Helper()
	c, err := NewHold(ident.ID{Namespace: ns, Name: "pump-daily"}, Hold{
		Device: "pump", Capability: capability.On, Value: true, Budget: goPumpBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pumpRig(t *testing.T, now string, b *storeBackend, rules ...*Compiled) *rig {
	t.Helper()
	return startRigWith(t, rigOptions{
		now: local(now), holds: b.holds(t), controls: b.controls(t),
		values:  map[string]map[string]any{"pump": {capability.On: false}},
		control: map[string][]control.Entry{"pump": {pumpEntry()}},
	}, rules...)
}

func TestBudgetInGoPlansTheSameAsARule(t *testing.T) {
	for _, water := range []float64{9, 14, 20} {
		rule := pumpRig(t, "2026-10-21 23:50 BST", newStoreBackend(t), holdRule(t, "pump-daily", pumpBudget))
		code := pumpRig(t, "2026-10-21 23:50 BST", newStoreBackend(t))
		code.e.Load(goPumpHold(t))
		for _, r := range []*rig{rule, code} {
			r.settle()
			r.report("water-temp", capability.Temperature, water)
			r.at("2026-10-22 00:00 BST")
		}
		a, b := rule.budget("pump-daily").Today, code.budget("pump-daily").Today
		if a == nil || b == nil {
			t.Fatalf("water %v: rule planned %+v, Go planned %+v", water, a, b)
		}
		if blocks(a) != blocks(b) || a.TargetMinutes != b.TargetMinutes || a.ScheduledMinutes != b.ScheduledMinutes ||
			a.TargetSource != b.TargetSource {
			t.Errorf("water %v: rule planned %d minutes in %s, Go planned %d in %s",
				water, a.TargetMinutes, blocks(a), b.TargetMinutes, blocks(b))
		}
	}
}

// A process restarts at every deploy. Planning the day again would run the budget twice.
func TestBudgetInGoKeepsItsPlanAcrossARestart(t *testing.T) {
	states := newStoreBackend(t)
	first := pumpRig(t, "2026-10-21 23:50 BST", states)
	first.e.Load(goPumpHold(t))
	first.settle()
	first.report("water-temp", capability.Temperature, 14.0)
	first.at("2026-10-22 00:00 BST")
	first.report("pump", capability.On, true)
	first.at("2026-10-22 00:30 BST")
	before := first.budget("pump-daily").Today
	if before == nil || before.RunMinutes == 0 || before.Starts == 0 {
		t.Fatalf("plan %+v, want a run under way", before)
	}
	first.stop()

	second := pumpRig(t, "2026-10-22 00:31 BST", states)
	second.e.Load(goPumpHold(t))
	second.settle()
	after := second.budget("pump-daily").Today
	if after == nil {
		t.Fatal("no plan after the restart")
	}
	if blocks(after) != blocks(before) || after.RunMinutes < before.RunMinutes || after.Starts != before.Starts {
		t.Errorf("after a restart %+v, want what it had: %+v", after, before)
	}
}

func TestNewHoldBudgetIsTheOnlyCondition(t *testing.T) {
	_, err := NewHold(ident.ID{Namespace: ns, Name: "x"}, Hold{
		Device: "pump", Capability: capability.On, Value: true, Budget: goPumpBudget(), Latch: true,
		When: func(Vars) (bool, error) { return true, nil },
	})
	if err == nil {
		t.Fatal("a budget with a when compiled")
	}
}
