package engine

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/schedule"
)

// checkpointEvery is how often a budget's run minutes alone are reported and saved.
const checkpointEvery = 5 * time.Minute

// Reasons in a BudgetReport.
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
// the host has.
type budgetHold struct {
	compile func(*control.Entry) (*schedule.Budget, error)
}

// savedBudget is a plan as kept in HoldState.Budget.
type savedBudget struct {
	Today BudgetDay `json:"today"`
	// Failure is why the day's budget couldn't be evaluated.
	Failure string `json:"failure,omitempty"`
}

// BudgetBlock is a planned run.
type BudgetBlock struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// BudgetDay is a budget hold's plan for a planning day, and how it's gone so far.
type BudgetDay struct {
	// Date is the planning day, YYYY-MM-DD, local time.
	Date string `json:"date"`
	// TargetSource is where the day's minutes came from.
	TargetSource string `json:"targetSource"`
	// TargetMinutes is the day's budget, rounded up to whole slots.
	TargetMinutes int32 `json:"targetMinutes"`
	// ScheduledMinutes is how much of it the plan places.
	ScheduledMinutes int32 `json:"scheduledMinutes"`
	// ShortfallMinutes is how much of it doesn't fit, within the forbid windows and the control
	// entry's limits.
	ShortfallMinutes int32         `json:"shortfallMinutes"`
	Blocks           []BudgetBlock `json:"blocks,omitempty"`
	// RunMinutes is how long the target has been on and available today, updated at most every
	// 5 minutes.
	RunMinutes int32 `json:"runMinutes"`
	// Starts counts the planned runs the hold has started today.
	Starts int32 `json:"starts"`
}

// BudgetReport is what the engine reports about a budget hold.
type BudgetReport struct {
	Generation int64
	// Cleared is set when the rule no longer has a budget, or is gone.
	Cleared bool
	// Today is nil until a plan is made.
	Today *BudgetDay
	// Unplanned is set when the budget couldn't be planned. Reason and Message say why.
	Unplanned bool
	Reason    string
	Message   string
}

// BudgetMetrics are the budget holds' gauges and counter, by namespace and rule.
type BudgetMetrics struct {
	Target    *prometheus.GaugeVec
	Scheduled *prometheus.GaugeVec
	Run       *prometheus.CounterVec
	// TodayRun is today's run minutes, to compare with Target.
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
func (m *BudgetMetrics) planned(key ident.ID, p *budgetPlan, on bool) {
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
func (m *BudgetMetrics) unplanned(key ident.ID) {
	if m == nil {
		return
	}
	m.Target.DeleteLabelValues(key.Namespace, key.Name)
	m.Scheduled.DeleteLabelValues(key.Namespace, key.Name)
	m.Desired.WithLabelValues(key.Namespace, key.Name).Set(0)
}

func (m *BudgetMetrics) delete(key ident.ID) {
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
	// saved is the plan for HoldState.Budget.
	saved string
	// compiled is the budget compiled for the Rule's compiledGeneration against compiledControl.
	compiled           *schedule.Budget
	compiledGeneration int64
	compiledControl    control.Entry
}

// budgetCondition plans the day if it hasn't been, credits the target's run, and reports
// whether the plan wants the target on now, and when that next changes. It never returns
// unknown: a budget that can't be planned keeps the target off. Call with e.mu held.
func (e *Engine) budgetCondition(key ident.ID, s *state, now time.Time, devices map[string]map[string]any) (on bool, next time.Time) {
	h, hs := s.c.hold, s.hold
	if hs.budget == nil {
		hs.budget = &budgetState{}
	}
	bs := hs.budget
	dev := ident.ID{Namespace: key.Namespace, Name: h.device}
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
		p = e.restorePlan(key, hs.saved.Budget, date, end, now)
		if p == nil {
			read := func(device, name string) *schedule.Reading {
				in, ok := e.Store.Get(ident.ID{Namespace: key.Namespace, Name: device})
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
	report = BudgetReport{Today: p.status(), Reason: ReasonPlanned,
		Message: planMessage(p.day)}
	if p.failure != "" {
		report.Unplanned, report.Reason, report.Message = true, ReasonEvaluationFailed, p.failure
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
		return nil, BudgetReport{Unplanned: true, Reason: ReasonNoControl,
			Message: "Device " + h.device + " has no control entry for " + h.capability + ", which a budget needs"}
	}
	entry := snap.Control[i]
	if bs.compiled != nil && bs.compiledGeneration == s.c.Generation && reflect.DeepEqual(bs.compiledControl, entry) {
		return bs.compiled, BudgetReport{}
	}
	b, err := h.budget.compile(&entry)
	if err != nil {
		bs.compiled = nil
		return nil, BudgetReport{Unplanned: true, Reason: ReasonInvalidBudget, Message: err.Error()}
	}
	bs.compiled, bs.compiledGeneration, bs.compiledControl = b, s.c.Generation, entry
	return b, BudgetReport{}
}

// restorePlan returns the plan saved in the hold's state, if it's for this planning day. The block in progress counts as started already. It returns nil otherwise. Run minutes not
// yet checkpointed, at most checkpointEvery of them, are lost.
func (e *Engine) restorePlan(key ident.ID, saved, date string, end, now time.Time) *budgetPlan {
	if saved == "" {
		return nil
	}
	var s savedBudget
	if err := json.Unmarshal([]byte(saved), &s); err != nil {
		e.Log.Error("Failed to read saved budget plan", "err", err, "rule", key)
		return nil
	}
	today, failure := s.Today, s.Failure
	if today.Date != date {
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
		p.day.Blocks = append(p.day.Blocks, schedule.Block{Start: b.Start, End: b.End})
		if !now.Before(b.Start) && now.Before(b.End) {
			p.started = b.Start
		}
	}
	return p
}

// credit adds the time since the last credit to run, if the target was on and available then,
// and records whether it is now. The engine credits when the target changes, so its state between
// credits is the one recorded at the last.
func (p *budgetPlan) credit(now time.Time, on bool, m *BudgetMetrics, key ident.ID) {
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

func (p *budgetPlan) status() *BudgetDay {
	today := &BudgetDay{
		Date:             p.date,
		TargetSource:     string(p.day.TargetSource),
		TargetMinutes:    int32(p.day.TargetMinutes),
		ScheduledMinutes: int32(p.day.ScheduledMinutes),
		ShortfallMinutes: int32(p.day.ShortfallMinutes),
		RunMinutes:       int32(math.Floor(p.run.Minutes())),
		Starts:           int32(p.starts),
	}
	for _, b := range p.day.Blocks {
		today.Blocks = append(today.Blocks, BudgetBlock{Start: b.Start, End: b.End})
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
func (e *Engine) reportBudget(key ident.ID, s *state, now time.Time, r BudgetReport) {
	bs := s.hold.budget
	r.Generation = s.c.Generation
	if prev := bs.reported; prev != nil && sameBudget(*prev, r) {
		if prev.Today == nil || prev.Today.RunMinutes == r.Today.RunMinutes || now.Sub(bs.checkpointed) < checkpointEvery {
			return
		}
	}
	bs.reported, bs.checkpointed = &r, now
	if r.Today != nil {
		saved := savedBudget{Today: *r.Today}
		if r.Reason == ReasonEvaluationFailed {
			saved.Failure = r.Message
		}
		raw, err := json.Marshal(saved)
		if err != nil {
			e.Log.Error("Failed to save budget plan", "err", err, "rule", key)
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
	if a.Generation != b.Generation || a.Unplanned != b.Unplanned || a.Reason != b.Reason || a.Message != b.Message ||
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
		x.Starts == y.Starts && slices.EqualFunc(x.Blocks, y.Blocks, func(p, q BudgetBlock) bool {
		return p.Start.Equal(q.Start) && p.End.Equal(q.End)
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
func (e *Engine) clearBudget(key ident.ID, generation int64) {
	e.Budget.delete(key)
	if e.OnBudget != nil {
		e.OnBudget(key, BudgetReport{Generation: generation, Cleared: true})
	}
}
