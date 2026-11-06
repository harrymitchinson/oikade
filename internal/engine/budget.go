package engine

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/schedule"
)

// checkpointEvery is how often a budget's run minutes alone are reported to status.today.
const checkpointEvery = 5 * time.Minute

// Reasons for the Planned condition.
const (
	ReasonPlanned = "Planned"
	// ReasonNoControl: the budget's target has no control entry, which its limits come from.
	ReasonNoControl = "NoControl"
	// ReasonInvalidBudget: the budget doesn't fit its target's control entry.
	ReasonInvalidBudget = "InvalidBudget"
	// ReasonEvaluationFailed: the minutes expression failed, so the day's budget is zero.
	ReasonEvaluationFailed = "EvaluationFailed"
)

// budgetHold is a compiled budget: how to compile it against its target's control entry, which only
// the runtime has, and the plan and Planned condition its status held when it loaded.
type budgetHold struct {
	compile func(*control.Entry) (*schedule.Budget, error)
	today   *homev1alpha1.BudgetDay
	planned *metav1.Condition
	// saves keeps the plan in the hold's saved state, for a hold with no status to keep it in.
	saves bool
}

// savedBudget is a plan as kept in HoldState.Budget.
type savedBudget struct {
	Today homev1alpha1.BudgetDay `json:"today"`
	// Failure is why the day's budget couldn't be evaluated.
	Failure string `json:"failure,omitempty"`
}

// BudgetReport is what the engine reports about a budget hold: status.today and the Planned
// condition.
type BudgetReport struct {
	Generation int64
	// Cleared is set when the Rule no longer has a budget, or is gone.
	Cleared bool
	// Today is nil until a plan is made.
	Today   *homev1alpha1.BudgetDay
	Planned metav1.ConditionStatus
	Reason  string
	Message string
}

// BudgetMetrics are the budget holds' gauges and counter, by namespace and rule.
type BudgetMetrics struct {
	Target    *prometheus.GaugeVec
	Scheduled *prometheus.GaugeVec
	Run       *prometheus.CounterVec
	// TodayRun is today's run minutes, as in status.today, to compare with Target.
	TodayRun *prometheus.GaugeVec
	Desired  *prometheus.GaugeVec
}

// NewBudgetMetrics returns the metrics Engine.Budget expects.
func NewBudgetMetrics() *BudgetMetrics {
	labels := []string{labelNamespace, labelRule}
	return &BudgetMetrics{
		Target: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "oikade_rule_budget_target_minutes",
			Help: "A budget hold's minutes for the current planning day.",
		}, labels),
		Scheduled: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "oikade_rule_budget_scheduled_minutes",
			Help: "How many of a budget hold's minutes the current day's plan places.",
		}, labels),
		Run: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oikade_rule_budget_run_minutes_total",
			Help: "Minutes a budget hold's target has been on and available.",
		}, labels),
		TodayRun: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "oikade_rule_budget_today_run_minutes",
			Help: "Minutes a budget hold's target has been on and available in the current planning day.",
		}, labels),
		Desired: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "oikade_rule_budget_desired",
			Help: "Whether a budget hold's plan wants its target on now: 1 or 0.",
		}, labels),
	}
}

// Collectors returns the metrics to register.
func (m *BudgetMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.Target, m.Scheduled, m.Run, m.TodayRun, m.Desired}
}

// planned sets a budget's metrics from its plan, and whether the plan wants the target on now.
func (m *BudgetMetrics) planned(key types.NamespacedName, p *budgetPlan, on bool) {
	if m == nil {
		return
	}
	m.Target.WithLabelValues(key.Namespace, key.Name).Set(float64(p.day.TargetMinutes))
	m.Scheduled.WithLabelValues(key.Namespace, key.Name).Set(float64(p.day.ScheduledMinutes))
	m.Desired.WithLabelValues(key.Namespace, key.Name).Set(float64(btoi(on)))
	m.TodayRun.WithLabelValues(key.Namespace, key.Name).Set(math.Floor(p.run.Minutes()))
}

// unplanned sets a budget's metrics when it can't plan: nothing is planned or wanted, and the
// run is kept for when it can plan again.
func (m *BudgetMetrics) unplanned(key types.NamespacedName) {
	if m == nil {
		return
	}
	m.Target.DeleteLabelValues(key.Namespace, key.Name)
	m.Scheduled.DeleteLabelValues(key.Namespace, key.Name)
	m.Desired.WithLabelValues(key.Namespace, key.Name).Set(0)
}

func (m *BudgetMetrics) delete(key types.NamespacedName) {
	if m == nil {
		return
	}
	m.Target.DeleteLabelValues(key.Namespace, key.Name)
	m.Scheduled.DeleteLabelValues(key.Namespace, key.Name)
	m.Run.DeleteLabelValues(key.Namespace, key.Name)
	m.TodayRun.DeleteLabelValues(key.Namespace, key.Name)
	m.Desired.DeleteLabelValues(key.Namespace, key.Name)
}

// budgetPlan is a budget hold's plan for one planning day, and its progress. Use it with e.mu held.
type budgetPlan struct {
	date string
	// device and capability are the target the plan was made for.
	device, capability string
	// failure is why the day's budget couldn't be evaluated. Empty when it could.
	failure string
	// end is when the planning day ends.
	end time.Time
	day schedule.Day
	run time.Duration
	// starts counts the blocks the hold has started, and started is the start of the last one.
	starts  int
	started time.Time
	// credited is when run was last brought up to date, and on whether the target was on and
	// available then.
	credited time.Time
	on       bool
}

// budgetState is a budget hold's state beyond its HoldState. Use it with e.mu held.
type budgetState struct {
	plan *budgetPlan
	// reported is the last BudgetReport, and checkpointed when its run minutes were reported.
	reported     *BudgetReport
	checkpointed time.Time
	// saved is the plan for HoldState.Budget, for a hold that saves it.
	saved string
	// compiled is the budget compiled for the Rule's compiledGeneration against compiledControl.
	compiled           *schedule.Budget
	compiledGeneration int64
	compiledControl    control.Entry
}

// budgetCondition plans the day if it hasn't been, credits the target's run, and reports
// whether the plan wants the target on now, and when that next changes. It never returns
// unknown: a budget that can't be planned keeps the target off. Call with e.mu held.
func (e *Engine) budgetCondition(key types.NamespacedName, s *state, now time.Time, devices map[string]map[string]any) (on bool, next time.Time) {
	h, hs := s.c.hold, s.hold
	if hs.budget == nil {
		hs.budget = &budgetState{}
	}
	bs := hs.budget
	dev := types.NamespacedName{Namespace: key.Namespace, Name: h.device}
	snap, _ := e.Store.Get(dev)
	// A driver that never reports availability counts as available.
	targetOn := snap.Discovered && (snap.Online == nil || *snap.Online) && capability.Float(snap.Values[h.capability]) != 0
	p := bs.plan
	if p != nil {
		// Credited even when the budget can't plan, so the run isn't lost meanwhile.
		p.credit(now, targetOn, e.Budget, key)
	}
	b, report := e.compileBudget(s, snap)
	if b == nil {
		e.Budget.unplanned(key)
		e.reportBudget(key, s, now, report)
		return false, time.Time{}
	}

	y, m, d, end := planningDay(now, e.Location, b.PlanningMinute)
	date := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	// Editing the budget waits for the next planning day, but a new target is planned for at once.
	if p == nil || p.date != date || p.device != h.device || p.capability != h.capability {
		prev := p
		p = e.restorePlan(key, h.budget, hs.saved.Budget, date, end, now)
		if p == nil {
			read := func(device, name string) *schedule.Reading {
				in, ok := e.Store.Get(types.NamespacedName{Namespace: key.Namespace, Name: device})
				v, has := in.Values[name]
				if !ok || !has {
					return nil
				}
				return &schedule.Reading{Value: capability.Float(v), At: in.LastSeen}
			}
			if devices == nil {
				devices = e.Store.Values(key.Namespace)
			}
			p = &budgetPlan{date: date, end: end, day: schedule.Plan(b.Input(y, m, d, now, e.Location, devices, read))}
			for _, err := range p.day.Errors {
				e.Log.Info("Budget planned with an error", "rule", key, "error", err.Error())
			}
			if len(p.day.Errors) > 0 {
				p.failure = p.day.Errors[0].Error()
			}
			e.Log.Info("Planned budget", "rule", key, "date", date, "source", p.day.TargetSource,
				"target", p.day.TargetMinutes, "scheduled", p.day.ScheduledMinutes, "blocks", len(p.day.Blocks))
		}
		p.device, p.capability = h.device, h.capability
		p.credited, p.on = now, targetOn
		if prev != nil && prev.device == h.device && prev.capability == h.capability && !now.Before(prev.end) {
			// The run since the last day ended belongs to this one.
			p.credited, p.on = prev.end, prev.on
			p.credit(now, targetOn, e.Budget, key)
		}
		bs.plan = p
		if e.Budget != nil {
			// Exported at 0 before any run is credited.
			e.Budget.Run.WithLabelValues(key.Namespace, key.Name)
		}
	}

	next = p.end
	for _, blk := range p.day.Blocks {
		switch {
		case !now.Before(blk.Start) && now.Before(blk.End):
			on, next = true, blk.End
			if !blk.Start.Equal(p.started) {
				p.starts, p.started = p.starts+1, blk.Start
			}
		case blk.Start.After(now) && blk.Start.Before(next):
			next = blk.Start
		}
	}

	e.Budget.planned(key, p, on)
	report = BudgetReport{Today: p.status(), Planned: metav1.ConditionTrue, Reason: ReasonPlanned,
		Message: planMessage(p.day)}
	if p.failure != "" {
		report.Planned, report.Reason, report.Message = metav1.ConditionFalse, ReasonEvaluationFailed, p.failure
	}
	e.reportBudget(key, s, now, report)
	return on, next
}

// compileBudget compiles the budget against its target's control entry, reusing the last one
// while neither changes. When it can't, it returns nil and the report to make. Call with e.mu
// held.
func (e *Engine) compileBudget(s *state, snap hub.Snapshot) (*schedule.Budget, BudgetReport) {
	h, bs := s.c.hold, s.hold.budget
	i := slices.IndexFunc(snap.Control, func(c control.Entry) bool { return c.Capability == h.capability })
	if i < 0 {
		bs.compiled = nil
		return nil, BudgetReport{Planned: metav1.ConditionFalse, Reason: ReasonNoControl,
			Message: "Device " + h.device + " has no control entry for " + h.capability + ", which a budget needs"}
	}
	entry := snap.Control[i]
	if bs.compiled != nil && bs.compiledGeneration == s.c.Generation && equality.Semantic.DeepEqual(bs.compiledControl, entry) {
		return bs.compiled, BudgetReport{}
	}
	b, err := h.budget.compile(&entry)
	if err != nil {
		bs.compiled = nil
		return nil, BudgetReport{Planned: metav1.ConditionFalse, Reason: ReasonInvalidBudget, Message: err.Error()}
	}
	bs.compiled, bs.compiledGeneration, bs.compiledControl = b, s.c.Generation, entry
	return b, BudgetReport{}
}

// restorePlan returns the plan status.today held when the Rule loaded, if it's for this planning
// day. The block in progress counts as started already. It returns nil otherwise. Run minutes not
// yet checkpointed, at most checkpointEvery of them, are lost.
func (e *Engine) restorePlan(key types.NamespacedName, bh *budgetHold, saved, date string, end, now time.Time) *budgetPlan {
	today, failure := bh.today, ""
	// The Planned condition was written with status.today, so a failed day stays failed.
	if c := bh.planned; c != nil && c.Reason == ReasonEvaluationFailed {
		failure = c.Message
	}
	if bh.saves && saved != "" {
		var s savedBudget
		if err := json.Unmarshal([]byte(saved), &s); err != nil {
			e.Log.Error(err, "Failed to read saved budget plan", "rule", key)
			return nil
		}
		today, failure = &s.Today, s.Failure
	}
	if today == nil || today.Date != date {
		return nil
	}
	p := &budgetPlan{
		date: date, end: end, run: time.Duration(today.RunMinutes) * time.Minute, starts: int(today.Starts),
		failure: failure,
	}
	p.day = schedule.Day{
		TargetSource:     schedule.TargetSource(today.TargetSource),
		TargetMinutes:    int(today.TargetMinutes),
		ScheduledMinutes: int(today.ScheduledMinutes),
		ShortfallMinutes: int(today.ShortfallMinutes),
	}
	for _, b := range today.Blocks {
		p.day.Blocks = append(p.day.Blocks, schedule.Block{Start: b.Start.Time, End: b.End.Time})
		if !now.Before(b.Start.Time) && now.Before(b.End.Time) {
			p.started = b.Start.Time
		}
	}
	return p
}

// credit adds the time since the last credit to run, if the target was on and available then,
// and records whether it is now. The engine credits when the target changes, so its state between
// credits is the one recorded at the last.
func (p *budgetPlan) credit(now time.Time, on bool, m *BudgetMetrics, key types.NamespacedName) {
	if p.on && now.After(p.credited) {
		until := now
		if p.end.Before(until) {
			until = p.end
		}
		d := until.Sub(p.credited)
		if d > 0 {
			p.run += d
			if m != nil {
				m.Run.WithLabelValues(key.Namespace, key.Name).Add(d.Minutes())
			}
		}
	}
	p.credited, p.on = now, on
}

func (p *budgetPlan) status() *homev1alpha1.BudgetDay {
	today := &homev1alpha1.BudgetDay{
		Date:             p.date,
		TargetSource:     string(p.day.TargetSource),
		TargetMinutes:    int32(p.day.TargetMinutes),
		ScheduledMinutes: int32(p.day.ScheduledMinutes),
		ShortfallMinutes: int32(p.day.ShortfallMinutes),
		RunMinutes:       int32(math.Floor(p.run.Minutes())),
		Starts:           int32(p.starts),
	}
	for _, b := range p.day.Blocks {
		today.Blocks = append(today.Blocks, homev1alpha1.BudgetBlock{Start: metav1.NewTime(b.Start), End: metav1.NewTime(b.End)})
	}
	return today
}

func planMessage(d schedule.Day) string {
	msg := "Planned " + strconv.Itoa(d.ScheduledMinutes) + " of " + strconv.Itoa(d.TargetMinutes) + " minutes"
	if d.ShortfallMinutes > 0 {
		msg += ", " + strconv.Itoa(d.ShortfallMinutes) + " short"
	}
	return msg
}

// reportBudget reports a budget hold's status, unless only its run minutes changed and they were
// reported less than checkpointEvery ago. Call with e.mu held.
func (e *Engine) reportBudget(key types.NamespacedName, s *state, now time.Time, r BudgetReport) {
	bs := s.hold.budget
	r.Generation = s.c.Generation
	if prev := bs.reported; prev != nil && sameBudget(*prev, r) {
		if prev.Today == nil || prev.Today.RunMinutes == r.Today.RunMinutes || now.Sub(bs.checkpointed) < checkpointEvery {
			return
		}
	}
	bs.reported, bs.checkpointed = &r, now
	if s.c.hold.budget.saves && r.Today != nil {
		saved := savedBudget{Today: *r.Today}
		if r.Reason == ReasonEvaluationFailed {
			saved.Failure = r.Message
		}
		raw, err := json.Marshal(saved)
		if err != nil {
			e.Log.Error(err, "Failed to save budget plan", "rule", key)
		} else {
			bs.saved = string(raw)
		}
	}
	if e.OnBudget != nil {
		e.OnBudget(key, r)
	}
}

// sameBudget compares two reports apart from run minutes.
func sameBudget(a, b BudgetReport) bool {
	if a.Generation != b.Generation || a.Planned != b.Planned || a.Reason != b.Reason || a.Message != b.Message ||
		(a.Today == nil) != (b.Today == nil) {
		return false
	}
	if a.Today == nil {
		return true
	}
	x, y := *a.Today, *b.Today
	x.RunMinutes, y.RunMinutes = 0, 0
	return x.Date == y.Date && x.TargetSource == y.TargetSource && x.TargetMinutes == y.TargetMinutes &&
		x.ScheduledMinutes == y.ScheduledMinutes && x.ShortfallMinutes == y.ShortfallMinutes &&
		x.Starts == y.Starts && slices.EqualFunc(x.Blocks, y.Blocks, func(p, q homev1alpha1.BudgetBlock) bool {
		return p.Start.Equal(&q.Start) && p.End.Equal(&q.End)
	})
}

// planningDay returns the date of the planning day that now is in, which starts at
// planningMinute past local midnight, and when it ends.
func planningDay(now time.Time, loc *time.Location, planningMinute int) (int, time.Month, int, time.Time) {
	l := now.In(loc)
	y, m, d := l.Date()
	if now.Before(time.Date(y, m, d, 0, planningMinute, 0, 0, loc)) {
		y, m, d = l.AddDate(0, 0, -1).Date()
	}
	return y, m, d, time.Date(y, m, d+1, 0, planningMinute, 0, 0, loc)
}

// clearBudget reports that a Rule no longer has a budget, and drops its metrics. Call with e.mu
// held.
func (e *Engine) clearBudget(key types.NamespacedName, generation int64) {
	e.Budget.delete(key)
	if e.OnBudget != nil {
		e.OnBudget(key, BudgetReport{Generation: generation, Cleared: true})
	}
}
