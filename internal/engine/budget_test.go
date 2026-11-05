package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

// pumpEntry is scenario 5's pump: on for at least an hour, resting half an hour, three starts a
// day, and a Home app tap holds for two hours.
func pumpEntry() homev1alpha1.CapabilityControl {
	e := entry(capability.OnOff, "false", homev1alpha1.ControlResolvePreferOn, homev1alpha1.ControlExternalOverride, "2h")
	starts := int32(3)
	e.MinimumOn, e.MinimumOff = &metav1.Duration{Duration: time.Hour}, &metav1.Duration{Duration: 30 * time.Minute}
	e.MaximumStartsPerDay = &starts
	return e
}

// pumpBudget runs the pump 2, 3 or 5 hours a day by the water's temperature, preferring the night
// and never at breakfast.
const pumpBudget = `
  while:
    budget:
      minutes:
        bands:
          input: {device: water-temp, capability: temperature, maxAge: 1h}
          missingMinutes: 240
          ranges: [{below: 12, minutes: 120}, {below: 16, minutes: 180}, {minutes: 300}]
      prefer: [{window: {start: "22:00", end: "06:00"}, score: 3}]
      forbid: [{window: {start: "07:00", end: "08:30"}}]
  hold: {device: pump, capability: on-off, value: true}
`

const pumpFrost = `
  priority: Safety
  while: {when: 'devices["outdoor-temp"]["temperature"] < 1.0'}
  hold: {device: pump, capability: on-off, value: true}
`

// setControlOf replaces one device's control entries, leaving the others' empty.
func (r *rig) setControlOf(dev string, entries ...homev1alpha1.CapabilityControl) {
	metas := map[types.NamespacedName]hub.Meta{}
	for name := range holdDevices {
		metas[types.NamespacedName{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m}
	}
	metas[types.NamespacedName{Namespace: ns, Name: dev}] = hub.Meta{Instance: z2m, Control: entries}
	r.store.SetDevices(z2m, metas)
}

// report reports a device value, as a driver does: seen, then changed.
func (r *rig) report(dev, c string, v any) {
	r.t.Helper()
	r.store.Seen(types.NamespacedName{Namespace: ns, Name: dev}, r.clock.Now())
	r.change(dev, c, v)
}

func blocks(today *homev1alpha1.BudgetDay) string {
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
		"oikade_rule_budget_target_minutes":    {testutil.ToFloat64(r.e.Budget.Target.WithLabelValues(ns, name)), target},
		"oikade_rule_budget_scheduled_minutes": {testutil.ToFloat64(r.e.Budget.Scheduled.WithLabelValues(ns, name)), scheduled},
		"oikade_rule_budget_run_minutes_total": {testutil.ToFloat64(r.e.Budget.Run.WithLabelValues(ns, name)), run},
	} {
		if tc.got < tc.want-0.1 || tc.got > tc.want+0.1 {
			r.t.Errorf("%s = %v, want %v", metric, tc.got, tc.want)
		}
	}
}

func TestScenario5PumpBudgetWithFrostAndAHomeAppTap(t *testing.T) {
	url := mqtttest.StartBroker(t)
	start := func(now string, today *homev1alpha1.BudgetDay, pumpOn bool) *rig {
		budget := holdRule(t, "pump-daily", pumpBudget)
		budget.Status.Today = today
		return startRigWith(t, rigOptions{
			now: local(now), holds: busStates(t, url), controls: controlStates(t, url),
			values:  map[string]map[string]any{"pump": {capability.OnOff: pumpOn}, "outdoor-temp": {capability.Temperature: 5.0}},
			control: map[string][]homev1alpha1.CapabilityControl{"pump": {pumpEntry()}},
		}, budget, holdRule(t, "pump-frost", pumpFrost))
	}

	// The water is at 14°C just before midnight, so the day's budget is 3 hours, planned in the
	// night slots at midnight.
	r := start("2026-10-21 23:50 BST", nil, false)
	r.report("water-temp", capability.Temperature, 14.0)
	r.at("2026-10-22 00:00 BST")
	b := r.budget("pump-daily")
	if b.Planned != metav1.ConditionTrue || b.Today == nil {
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
	if !first.Start.Time.Equal(local("2026-10-22 00:00 BST")) {
		t.Fatalf("blocks %s, want the first at 00:00", blocks(today))
	}
	r.expect(call{"pump", capability.OnOff, true})
	r.report("pump", capability.OnOff, true)
	if w := winner(r.status("pump", capability.OnOff)); w != "Rule/pump-daily Normal" {
		t.Errorf("00:00 winner %q", w)
	}
	if got := testutil.ToFloat64(r.e.Budget.Desired.WithLabelValues(ns, "pump-daily")); got != 1 {
		t.Errorf("oikade_rule_budget_desired %v at 00:00, want 1", got)
	}

	// The Home app turns it off at 01:00: a Matter override, which beats the budget.
	r.at("2026-10-22 01:00 BST")
	r.matter("pump", capability.OnOff, false)
	r.report("pump", capability.OnOff, false)
	if w := winner(r.status("pump", capability.OnOff)); w != "Matter Override" {
		t.Errorf("01:00 winner %q, want the Home app's override", w)
	}

	// Frost at 01:30 forces it on through the override, as a Safety hold.
	r.at("2026-10-22 01:30 BST")
	r.report("outdoor-temp", capability.Temperature, 0.5)
	if w := winner(r.status("pump", capability.OnOff)); w != "Rule/pump-frost Safety" {
		t.Errorf("01:30 winner %q, want the frost hold", w)
	}
	r.report("pump", capability.OnOff, true)
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
	// starts from status.today, rather than planning again.
	r = start("2026-10-22 01:50 BST", saved, true)
	r.report("water-temp", capability.Temperature, 20.0)
	r.report("pump", capability.OnOff, true)
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
	if b := r.budget("pump-daily"); b.Planned != metav1.ConditionFalse || b.Reason != ReasonNoControl {
		t.Errorf("report %+v, want Planned=False NoControl", b)
	}
	if r.active("pump-daily") != 0 {
		t.Error("oikade_rule_active isn't 0 for a budget that can't plan")
	}
}

func TestBudgetMetricsAreExportedBeforeAnyRun(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 14:00 BST"),
		control: map[string][]homev1alpha1.CapabilityControl{"pump": {pumpEntry()}},
	}, holdRule(t, "pump-daily", pumpBudget))
	for _, c := range r.e.Budget.Collectors() {
		if n := testutil.CollectAndCount(c); n != 1 {
			t.Errorf("%d series for a metric, want 1 before any run is credited", n)
		}
	}
	r.expectBudgetMetrics("pump-daily", 240, 240, 0)
	if got := testutil.ToFloat64(r.e.Budget.TodayRun.WithLabelValues(ns, "pump-daily")); got != 0 {
		t.Errorf("oikade_rule_budget_today_run_minutes %v, want 0", got)
	}

	// The control entry goes: nothing is planned or wanted any more.
	r.setControlOf("pump")
	r.at("2026-10-21 14:01 BST")
	if n := testutil.CollectAndCount(r.e.Budget.Target); n != 0 {
		t.Errorf("%d target series once it can't plan, want none", n)
	}
	if got := testutil.ToFloat64(r.e.Budget.Desired.WithLabelValues(ns, "pump-daily")); got != 0 {
		t.Errorf("oikade_rule_budget_desired %v once it can't plan, want 0", got)
	}
}

func TestBudgetPlansLateAfterADownedStart(t *testing.T) {
	// Started at 14:00 with no plan: today is planned from now, so the night is gone.
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 14:00 BST"),
		values:  map[string]map[string]any{"pump": {capability.OnOff: false}},
		control: map[string][]homev1alpha1.CapabilityControl{"pump": {pumpEntry()}},
	}, holdRule(t, "pump-daily", pumpBudget))
	b := r.budget("pump-daily")
	if b.Today == nil || b.Today.TargetSource != "Missing" || b.Today.TargetMinutes != 240 {
		t.Fatalf("report %+v, want the missing-input budget", b)
	}
	for _, blk := range b.Today.Blocks {
		if blk.Start.Time.Before(local("2026-10-21 14:00 BST")) {
			t.Errorf("block %s starts before the plan was made", blocks(b.Today))
		}
	}
}

func TestEditingOutTheBudgetClearsItsStatus(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 08:00 BST"),
		control: map[string][]homev1alpha1.CapabilityControl{"pump": {pumpEntry()}},
	}, holdRule(t, "pump-nightly", pumpBudget))
	if r.budget("pump-nightly").Today == nil {
		t.Fatal("no plan")
	}
	edited := holdRule(t, "pump-nightly", `
  while: {window: {start: "22:00", end: "06:00"}}
  hold: {device: pump, capability: on-off, value: true}
`)
	edited.Generation = 2
	c, err := Compile(edited)
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
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
	key := types.NamespacedName{Namespace: ns, Name: "pump-daily"}
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

func TestAStalePlanIsClearedOnLoad(t *testing.T) {
	// The budget was removed while the runtime was down: its plan is still in status.
	rule := holdRule(t, "pump-daily", `
  while: {window: {start: "22:00", end: "06:00"}}
  hold: {device: pump, capability: on-off, value: true}
`)
	rule.Status.Today = &homev1alpha1.BudgetDay{Date: "2026-10-21", TargetMinutes: 120}
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, nil, nil, rule)
	if b := r.budget("pump-daily"); !b.Cleared {
		t.Errorf("report %+v, want cleared", b)
	}
}

func TestAFailedDayStaysFailedAfterARestart(t *testing.T) {
	rule := holdRule(t, "pump-daily", `
  while: {budget: {minutes: {expression: 'devices["water-temp"]["temperature"] > 10.0 ? 60 : 30'}}}
  hold: {device: pump, capability: on-off, value: true}
`)
	start := func(today *homev1alpha1.BudgetDay, planned *metav1.Condition) *rig {
		r := rule.DeepCopy()
		r.Status.Today = today
		if planned != nil {
			r.Status.Conditions = []metav1.Condition{*planned}
		}
		return startRigWith(t, rigOptions{
			now:     local("2026-10-21 08:00 BST"),
			control: map[string][]homev1alpha1.CapabilityControl{"pump": {pumpEntry()}},
		}, r)
	}
	// water-temp has never reported, so the expression fails.
	r := start(nil, nil)
	b := r.budget("pump-daily")
	if b.Planned != metav1.ConditionFalse || b.Reason != ReasonEvaluationFailed {
		t.Fatalf("report %+v, want EvaluationFailed", b)
	}
	r.stop()
	r = start(b.Today, &metav1.Condition{Type: homev1alpha1.ConditionPlanned, Status: b.Planned, Reason: b.Reason, Message: b.Message})
	if got := r.budget("pump-daily"); got.Reason != ReasonEvaluationFailed || got.Message != b.Message {
		t.Errorf("after a restart, report %+v, want the same failure", got)
	}
}

func TestANewTargetIsPlannedForAtOnce(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now: local("2026-10-21 08:00 BST"),
		control: map[string][]homev1alpha1.CapabilityControl{
			"pump": {pumpEntry()}, "towel-rail": {pumpEntry()},
		},
	}, holdRule(t, "pump-daily", pumpBudget))
	r.e.mu.Lock()
	planned := r.e.rules[types.NamespacedName{Namespace: ns, Name: "pump-daily"}].hold.budget.plan
	r.e.mu.Unlock()
	edited := holdRule(t, "pump-daily", strings.Replace(pumpBudget, "device: pump,", "device: towel-rail,", 1))
	edited.Generation = 2
	c, err := Compile(edited)
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	r.settle()
	r.e.mu.Lock()
	p := r.e.rules[types.NamespacedName{Namespace: ns, Name: "pump-daily"}].hold.budget.plan
	r.e.mu.Unlock()
	if p == planned || p.device != "towel-rail" {
		t.Errorf("plan for %s, want a new one for towel-rail", p.device)
	}
}
