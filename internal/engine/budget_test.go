package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/schedule"
)

// pumpEntry is scenario 5's pump: on for at least an hour, resting half an hour, three starts a
// day, and a Home app tap holds for two hours.
func pumpEntry() control.Entry {
	e := entry(capability.On, "false", control.NamePreferOn, control.NameOverride, "2h")
	e.MinimumOn, e.MinimumOff = control.Duration(time.Hour), control.Duration(30*time.Minute)
	e.MaximumStartsPerDay = 3
	return e
}

// pumpBudget runs the pump 2, 3 or 5 hours a day by the water's temperature, preferring the night
// and never at breakfast.
var pumpBudget = Hold{Device: "pump", Capability: capability.On, Value: true, Budget: &schedule.Spec{Bands: &schedule.BandSpec{Device: "water-temp", Capability: capability.Temperature, MaxAge: time.Hour, MissingMinutes: 240, Ranges: []schedule.Band{{Below: new(12.0), Minutes: 120}, {Below: new(16.0), Minutes: 180}, {Minutes: 300}}}, Prefer: []schedule.PreferSpec{{Window: schedule.WindowSpec{Start: "22:00", End: "06:00"}, Score: 3}}, Forbid: []schedule.WindowSpec{{Start: "07:00", End: "08:30"}}}}

var pumpFrost = Hold{Device: "pump", Capability: capability.On, Value: true, Priority: PrioritySafety, When: below("outdoor-temp", capability.Temperature, 1.0)}

// setControlOf replaces one device's control entries, leaving the others' empty.
func (r *rig) setControlOf(dev string, entries ...control.Entry) {
	metas := map[ident.ID]hub.Meta{}
	for name := range holdDevices {
		metas[ident.ID{Name: name}] = hub.Meta{Instance: z2m}
	}
	metas[ident.ID{Name: dev}] = hub.Meta{Instance: z2m, Control: entries}
	r.store.SetDevices(z2m, metas)
}

// report reports a device value, as a driver does: seen, then changed.
func (r *rig) report(dev, c string, v any) {
	r.t.Helper()
	r.store.Seen(ident.ID{Name: dev}, r.clock.Now())
	r.change(dev, c, v)
}

func blocks(today *BudgetDay) string {
	s := make([]string, 0, len(today.Blocks))
	for _, b := range today.Blocks {
		s = append(s, b.Start.In(london).Format("15:04")+"-"+b.End.In(london).Format("15:04"))
	}
	return strings.Join(s, " ")
}

// expectBudgetMetrics checks a budget's target, scheduled and run minutes metrics.
func (r *rig) expectBudgetMetrics(name string, target, scheduled, run float64) {
	r.t.Helper()
	for metric, tc := range map[string]struct{ got, want float64 }{
		"oikade_rule_budget_target_minutes":    {testutil.ToFloat64(r.e.Budget.Target.WithLabelValues(name)), target},
		"oikade_rule_budget_scheduled_minutes": {testutil.ToFloat64(r.e.Budget.Scheduled.WithLabelValues(name)), scheduled},
		"oikade_rule_budget_run_minutes_total": {testutil.ToFloat64(r.e.Budget.Run.WithLabelValues(name)), run},
	} {
		if tc.got < tc.want-0.1 || tc.got > tc.want+0.1 {
			r.t.Errorf("%s = %v, want %v", metric, tc.got, tc.want)
		}
	}
}

func testScenario5PumpBudgetWithFrostAndAHomeAppTap(t *testing.T, states *storeBackend) {
	start := func(now string, pumpOn bool) *rig {
		budget := holdRule(t, "pump-daily", pumpBudget)
		return startRigWith(t, rigOptions{
			now: local(now), holds: states.holds(t), controls: states.controls(t),
			values:  map[string]map[string]any{"pump": {capability.On: pumpOn}, "outdoor-temp": {capability.Temperature: 5.0}},
			control: map[string][]control.Entry{"pump": {pumpEntry()}},
		}, budget, holdRule(t, "pump-frost", pumpFrost))
	}

	// The water is at 14°C just before midnight, so the day's budget is 3 hours, planned in the
	// night slots at midnight.
	r := start("2026-10-21 23:50 BST", false)
	r.report("water-temp", capability.Temperature, 14.0)
	r.at("2026-10-22 00:00 BST")
	b := r.budget("pump-daily")
	if b.Unplanned || b.Today == nil {
		t.Fatalf("00:00 report %+v, want a plan", b)
	}
	today := b.Today
	if today.Date != "2026-10-22" || today.TargetSource != "Bands" || today.TargetMinutes != 180 ||
		today.ScheduledMinutes != 180 || today.ShortfallMinutes != 0 {
		t.Fatalf("00:00 plan %+v, want 180 minutes from the bands", today)
	}
	for _, blk := range today.Blocks {
		if h := blk.Start.In(london).Hour(); h >= 6 && h < 22 {
			t.Errorf("block %s outside the preferred night", blocks(today))
		}
	}
	first := today.Blocks[0]
	if !first.Start.Equal(local("2026-10-22 00:00 BST")) {
		t.Fatalf("blocks %s, want the first at 00:00", blocks(today))
	}
	r.expect(call{"pump", capability.On, true})
	r.report("pump", capability.On, true)
	if w := winner(r.status("pump", capability.On)); w != "Rule/pump-daily Normal" {
		t.Errorf("00:00 winner %q", w)
	}
	if got := testutil.ToFloat64(r.e.Budget.Desired.WithLabelValues("pump-daily")); got != 1 {
		t.Errorf("oikade_rule_budget_desired %v at 00:00, want 1", got)
	}

	// The Home app turns it off at 01:00: a Matter override, which beats the budget.
	r.at("2026-10-22 01:00 BST")
	r.matter("pump", capability.On, false)
	r.report("pump", capability.On, false)
	if w := winner(r.status("pump", capability.On)); w != "Matter Override" {
		t.Errorf("01:00 winner %q, want the Home app's override", w)
	}

	// Frost at 01:30 forces it on through the override, as a Safety hold.
	r.at("2026-10-22 01:30 BST")
	r.report("outdoor-temp", capability.Temperature, 0.5)
	if w := winner(r.status("pump", capability.On)); w != "Rule/pump-frost Safety" {
		t.Errorf("01:30 winner %q, want the frost hold", w)
	}
	r.report("pump", capability.On, true)
	r.at("2026-10-22 01:45 BST")
	b = r.budget("pump-daily")
	// On 00:00-01:00 and 01:30-01:45, reported at most every 5 minutes.
	if b.Today.RunMinutes < 70 || b.Today.RunMinutes > 75 {
		t.Errorf("01:45 run minutes %d, want 70 to 75", b.Today.RunMinutes)
	}
	if b.Today.Starts != 1 {
		t.Errorf("01:45 starts %d, want 1", b.Today.Starts)
	}
	r.expectBudgetMetrics("pump-daily", 180, 180, 75)
	saved := b.Today
	r.stop()

	// A restart the same day, with the water now warmer, keeps the plan, the run minutes and the
	// starts it saved, rather than planning again.
	r = start("2026-10-22 01:50 BST", true)
	r.report("water-temp", capability.Temperature, 20.0)
	r.report("pump", capability.On, true)
	r.at("2026-10-22 01:56 BST")
	b = r.budget("pump-daily")
	if blocks(b.Today) != blocks(saved) || b.Today.TargetMinutes != 180 {
		t.Errorf("after a restart, plan %s of %d minutes, want %s of 180", blocks(b.Today), b.Today.TargetMinutes, blocks(saved))
	}
	if b.Today.Starts != 1 || b.Today.RunMinutes < saved.RunMinutes {
		t.Errorf("after a restart, starts %d and run %d, want 1 and at least %d", b.Today.Starts, b.Today.RunMinutes, saved.RunMinutes)
	}

	// The next day is planned afresh from the warmer water.
	r.at("2026-10-22 23:30 BST")
	r.report("water-temp", capability.Temperature, 20.0)
	r.at("2026-10-23 00:00 BST")
	b = r.budget("pump-daily")
	if b.Today.Date != "2026-10-23" || b.Today.TargetMinutes != 300 || b.Today.RunMinutes != 0 || b.Today.Starts != 1 {
		t.Errorf("next day %+v, want 300 minutes, no run yet and its first block started", b.Today)
	}
}

func TestBudgetWithoutAControlEntryStaysOff(t *testing.T) {
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, nil, nil, holdRule(t, "pump-daily", pumpBudget))
	r.at("2026-10-21 23:00 BST")
	r.expect()
	if b := r.budget("pump-daily"); !b.Unplanned || b.Reason != ReasonNoControl {
		t.Errorf("report %+v, want Planned=False NoControl", b)
	}
	if r.active("pump-daily") != 0 {
		t.Error("oikade_rule_active isn't 0 for a budget that can't plan")
	}
}

func TestBudgetMetricsAreExportedBeforeAnyRun(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 14:00 BST"),
		control: map[string][]control.Entry{"pump": {pumpEntry()}},
	}, holdRule(t, "pump-daily", pumpBudget))
	for _, c := range r.e.Budget.Collectors() {
		if n := testutil.CollectAndCount(c); n != 1 {
			t.Errorf("%d series for a metric, want 1 before any run is credited", n)
		}
	}
	r.expectBudgetMetrics("pump-daily", 240, 240, 0)
	if got := testutil.ToFloat64(r.e.Budget.TodayRun.WithLabelValues("pump-daily")); got != 0 {
		t.Errorf("oikade_rule_budget_today_run_minutes %v, want 0", got)
	}

	// The control entry goes: nothing is planned or wanted any more.
	r.setControlOf("pump")
	r.at("2026-10-21 14:01 BST")
	if n := testutil.CollectAndCount(r.e.Budget.Target); n != 0 {
		t.Errorf("%d target series once it can't plan, want none", n)
	}
	if got := testutil.ToFloat64(r.e.Budget.Desired.WithLabelValues("pump-daily")); got != 0 {
		t.Errorf("oikade_rule_budget_desired %v once it can't plan, want 0", got)
	}
}

func TestBudgetPlansLateAfterADownedStart(t *testing.T) {
	// Started at 14:00 with no plan: today is planned from now, so the night is gone.
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 14:00 BST"),
		values:  map[string]map[string]any{"pump": {capability.On: false}},
		control: map[string][]control.Entry{"pump": {pumpEntry()}},
	}, holdRule(t, "pump-daily", pumpBudget))
	b := r.budget("pump-daily")
	if b.Today == nil || b.Today.TargetSource != "Missing" || b.Today.TargetMinutes != 240 {
		t.Fatalf("report %+v, want the missing-input budget", b)
	}
	for _, blk := range b.Today.Blocks {
		if blk.Start.Before(local("2026-10-21 14:00 BST")) {
			t.Errorf("block %s starts before the plan was made", blocks(b.Today))
		}
	}
}

func TestEditingOutTheBudgetClearsItsStatus(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 08:00 BST"),
		control: map[string][]control.Entry{"pump": {pumpEntry()}},
	}, holdRule(t, "pump-nightly", pumpBudget))
	if r.budget("pump-nightly").Today == nil {
		t.Fatal("no plan")
	}
	edited := holdRule(t, "pump-nightly", Hold{Generation: 2, Device: "pump", Capability: capability.On, Value: true, Window: &Window{Start: At{Time: "22:00"}, End: At{Time: "06:00"}}})
	r.e.Load(edited)
	r.settle()
	if b := r.budget("pump-nightly"); !b.Cleared {
		t.Errorf("report %+v, want cleared", b)
	}
	if n := testutil.CollectAndCount(r.e.Budget.Target); n != 0 {
		t.Errorf("%d target gauges left, want none", n)
	}
}

func TestCreditCountsOnlyWhileOnAndAvailable(t *testing.T) {
	t0 := local("2026-10-21 10:00 BST")
	key := ident.ID{Name: "pump-daily"}
	for name, tc := range map[string]struct {
		steps []bool
		want  time.Duration
	}{
		"always on":          {[]bool{true, true, true}, 2 * time.Minute},
		"off throughout":     {[]bool{false, false, false}, 0},
		"on, then off":       {[]bool{true, false, false}, time.Minute},
		"off, then on":       {[]bool{false, true, true}, time.Minute},
		"on, gap, on":        {[]bool{true, false, true}, time.Minute},
		"past the day's end": {[]bool{true, true, true, true}, 2*time.Minute + 30*time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			p := &budgetPlan{end: t0.Add(2*time.Minute + 30*time.Second), credited: t0, on: tc.steps[0]}
			for i, on := range tc.steps[1:] {
				p.credit(t0.Add(time.Duration(i+1)*time.Minute), on, nil, key)
			}
			if p.run != tc.want {
				t.Errorf("run %s, want %s", p.run, tc.want)
			}
		})
	}
}

func TestPlanningDay(t *testing.T) {
	for _, tc := range []struct {
		now     string
		minute  int
		date    string
		endTime string
	}{
		{"2026-10-21 00:00 BST", 0, "2026-10-21", "2026-10-22 00:00 BST"},
		{"2026-10-21 23:59 BST", 0, "2026-10-21", "2026-10-22 00:00 BST"},
		{"2026-10-21 05:59 BST", 6 * 60, "2026-10-20", "2026-10-21 06:00 BST"},
		{"2026-10-21 06:00 BST", 6 * 60, "2026-10-21", "2026-10-22 06:00 BST"},
		// The clocks go back on 25 October: that day is 25 hours long.
		{"2026-10-25 12:00 GMT", 0, "2026-10-25", "2026-10-26 00:00 GMT"},
	} {
		y, m, d, end := planningDay(local(tc.now), london, tc.minute)
		if got := fmt.Sprintf("%04d-%02d-%02d", y, m, d); got != tc.date || !end.Equal(local(tc.endTime)) {
			t.Errorf("planningDay(%s, %d) = %s ending %s, want %s ending %s", tc.now, tc.minute, got, end, tc.date, tc.endTime)
		}
	}
}

func TestAFailedDayStaysFailedAfterARestart(t *testing.T) {
	failing := func(vars map[string]any, _ time.Time, _ *time.Location) (int, error) {
		if _, ok := vars["devices"].(map[string]map[string]any)["water-temp"][capability.Temperature]; !ok {
			return 0, errors.New("water-temp hasn't reported")
		}
		return 60, nil
	}
	states := newMemoryStates[HoldState]()
	start := func() *rig {
		return startRigWith(t, rigOptions{
			now: local("2026-10-21 08:00 BST"), holds: states,
			control: map[string][]control.Entry{"pump": {pumpEntry()}},
		}, holdRule(t, "pump-daily", Hold{Device: "pump", Capability: capability.On, Value: true, Budget: &schedule.Spec{Minutes: failing}}))
	}
	// water-temp has never reported, so the minutes fail.
	r := start()
	b := r.budget("pump-daily")
	if !b.Unplanned || b.Reason != ReasonEvaluationFailed {
		t.Fatalf("report %+v, want EvaluationFailed", b)
	}
	r.stop()
	r = start()
	if got := r.budget("pump-daily"); got.Reason != ReasonEvaluationFailed || got.Message != b.Message {
		t.Errorf("after a restart, report %+v, want the same failure", got)
	}
}

func TestANewTargetIsPlannedForAtOnce(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now: local("2026-10-21 08:00 BST"),
		control: map[string][]control.Entry{
			"pump": {pumpEntry()}, "towel-rail": {pumpEntry()},
		},
	}, holdRule(t, "pump-daily", pumpBudget))
	r.e.mu.Lock()
	planned := r.e.rules[ident.ID{Name: "pump-daily"}].hold.budget.plan
	r.e.mu.Unlock()
	edited := pumpBudget
	edited.Device, edited.Generation = "towel-rail", 2
	r.e.Load(holdRule(t, "pump-daily", edited))
	r.settle()
	r.e.mu.Lock()
	p := r.e.rules[ident.ID{Name: "pump-daily"}].hold.budget.plan
	r.e.mu.Unlock()
	if p == planned || p.device != "towel-rail" {
		t.Errorf("plan for %s, want a new one for towel-rail", p.device)
	}
}
