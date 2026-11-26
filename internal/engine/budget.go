package engine

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strconv"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/schedule"
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

// BudgetToday returns the plan and progress a budget hold last saved in Budget, and false if it
// has none. Its Date may be a day that has ended.
func (s HoldState) BudgetToday() (BudgetDay, bool) {
	var saved savedBudget
	if s.Budget == "" || json.Unmarshal([]byte(s.Budget), &saved) != nil {
		return BudgetDay{}, false
	}
	return saved.Today, true
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
	// 5 minutes. A stretch offline counts if the target was on as it went and its first report once
	// back says on.
	RunMinutes int32 `json:"runMinutes"`
	// MissedMinutes is how much of the planned blocks went by with the target off while the
	// budget's conditions were true, updated as RunMinutes is.
	MissedMinutes int32 `json:"missedMinutes,omitempty"`
	// PendingMinutes are the missed minutes not yet replanned, and ReplannedAt when the plan was
	// last made again for the blocks to come and the minutes missed.
	PendingMinutes int32     `json:"pendingMinutes,omitempty"`
	ReplannedAt    time.Time `json:"replannedAt,omitzero"`
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

// planned sets a budget's metrics from its plan, and whether the plan wants the target on now.
func (m budgetMetrics) planned(key ident.ID, p *budgetPlan, on bool) {
	rule := ruleRef(key)
	m.target.Set(float64(p.day.TargetMinutes), rule)
	m.scheduled.Set(float64(p.day.ScheduledMinutes), rule)
	m.desired.Set(float64(btoi(on)), rule)
	m.todayRun.Set(math.Floor(p.run.Minutes()), rule)
}

func (m budgetMetrics) credited(key ident.ID, run, missed time.Duration) {
	if run > 0 {
		m.run.Add(run.Minutes(), ruleRef(key))
	}
	if missed > 0 {
		m.missed.Add(missed.Minutes(), ruleRef(key))
	}
}

// unplanned sets a budget's metrics when it can't plan: nothing is planned or wanted, and the
// run is kept for when it can plan again.
func (m budgetMetrics) unplanned(key ident.ID) {
	rule := ruleRef(key)
	m.target.Delete(rule)
	m.scheduled.Delete(rule)
	m.desired.Set(0, rule)
}

func (m budgetMetrics) delete(key ident.ID) {
	rule := ruleRef(key)
	for _, v := range m.all() {
		v.Delete(rule)
	}
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
	// missed is how much of the planned blocks went by with the target off and the conditions true,
	// and pending how much of it hasn't been replanned. replanned is when it last was.
	missed, pending time.Duration
	replanned       time.Time
	// lastOff is when the target was last seen going off.
	lastOff time.Time
	// starts counts the blocks the hold has started, and started is the start of the last one.
	starts  int
	started time.Time
	// credited is when run was last brought up to date, on whether the target was on and available
	// then, and when whether the budget's conditions were true.
	credited time.Time
	on, when bool
	// away is the target's stretch offline, nil while it's online.
	away *awayStretch
}

// awayStretch is a stretch the target has been offline, which counts once it reports again.
type awayStretch struct {
	// on is whether the target was on as it went offline, and seen when it had last reported.
	on   bool
	seen time.Time
	// today is the stretch in this planning day, and earlier in the days before, which only the
	// metrics count.
	today, earlier stretch
}

type stretch struct {
	// length is how long the target was offline, and planned how much of that the plan wanted it on.
	length, planned time.Duration
}

// target is a budget's target as one look sees it.
type target struct {
	// on is its last reported value, which an offline device keeps.
	on, online bool
	seen       time.Time
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

// budgetCondition plans the day if it hasn't been, credits the target's run and its missed minutes,
// and reports whether the plan wants the target on now, and when that next changes. when is whether
// the budget's conditions are true now. It never returns unknown: a budget that can't be planned
// keeps the target off. Call with e.mu held.
func (e *Engine) budgetCondition(key ident.ID, s *state, now time.Time, devices *home, when bool) (on bool, next time.Time) {
	h, hs := s.c.hold, s.hold
	if hs.budget == nil {
		hs.budget = &budgetState{}
	}
	bs := hs.budget
	dev := ident.ID{Name: h.device}
	snap, _ := e.Store.Get(dev)
	// A driver that never reports availability counts as available.
	t := target{
		on:     snap.Discovered && capability.Float(snap.Values[h.capability]) != 0,
		online: snap.Online == nil || *snap.Online, seen: snap.LastSeen,
	}
	p := bs.plan
	// last is the plan as the last look left it, which says what the target did past its day's end.
	var last budgetPlan
	if p != nil {
		last = *p
		if p.away != nil {
			away := *p.away
			last.away = &away
		}
		// Credited even when the budget can't plan, so the run isn't lost meanwhile.
		p.credit(now, t, e.metrics.budget, key)
		p.when = when
	}
	b, report := e.compileBudget(s, snap)
	if b == nil {
		if p != nil {
			// It keeps its target off, which isn't its target missing a run.
			p.when = false
		}
		e.metrics.budget.unplanned(key)
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
			p = e.planDay(key, b, y, m, d, end, now, devices)
		}
		p.device, p.capability = h.device, h.capability
		p.credited, p.when = now, when
		if prev != nil && prev.device == h.device && prev.capability == h.capability && !now.Before(prev.end) {
			p.carry(prev, last)
		}
		p.credit(now, t, e.metrics.budget, key)
		p.when = when
		bs.plan = p
		// Exported at 0 before any is credited.
		e.metrics.budget.run.Add(0, ruleRef(key))
		e.metrics.budget.missed.Add(0, ruleRef(key))
	}

	if p.away == nil && p.pending > 0 && !p.inBlock(now) {
		e.replan(key, b, p, y, m, d, now)
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

	e.metrics.budget.planned(key, p, on)
	report = BudgetReport{Today: p.status(), Reason: ReasonPlanned,
		Message: planMessage(p.day)}
	if p.failure != "" {
		report.Unplanned, report.Reason, report.Message = true, ReasonEvaluationFailed, p.failure
	}
	e.reportBudget(key, s, now, report)
	return on, next
}

// planDay plans the planning day that starts on the given date, from now. Call with e.mu held.
func (e *Engine) planDay(key ident.ID, b *schedule.Budget, y int, m time.Month, d int, end, now time.Time, devices *home) *budgetPlan {
	read := func(device, name string) *schedule.Reading {
		in, ok := e.Store.Get(ident.ID{Name: device})
		v, has := in.Values[name]
		if avg, isAverage := e.averageOf(device, name, now); isAverage {
			// An average is as fresh as the device's last report, so a dead sensor's is stale.
			v, has = avg, true
		}
		if !ok || !has {
			return nil
		}
		return &schedule.Reading{Value: capability.Float(v), At: in.LastSeen}
	}
	vars := e.conditionVars(now, devices)
	date := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	p := &budgetPlan{date: date, end: end, day: schedule.Plan(b.Input(y, m, d, now, e.Location, vars, read))}
	for _, err := range p.day.Errors {
		e.Log.Info("Budget planned with an error", "rule", ruleRef(key), "err", err)
	}
	if len(p.day.Errors) > 0 {
		p.failure = p.day.Errors[0].Error()
	}
	e.Log.Info("Planned budget", "rule", ruleRef(key), "date", date, "source", p.day.TargetSource,
		"target", p.day.TargetMinutes, "scheduled", p.day.ScheduledMinutes, "blocks", len(p.day.Blocks))
	return p
}

// carry starts p where the day before, prev, ended, with the target as last left it: the time since
// belongs to p. A stretch offline over the day's end goes on, and its part before it counts in the
// totals once it settles.
func (p *budgetPlan) carry(prev *budgetPlan, last budgetPlan) {
	p.credited, p.on, p.when = prev.end, last.on, last.when
	if a := last.away; a != nil {
		p.away = &awayStretch{on: a.on, seen: a.seen}
		if open := prev.away; open != nil {
			p.away.earlier = stretch{
				length: open.earlier.length + open.today.length, planned: open.earlier.planned + open.today.planned,
			}
		}
	}
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

// restorePlan returns the plan saved in the hold's state if it's for this planning day, or else
// nil. The block in progress counts as started already. Run minutes not yet checkpointed, at most
// checkpointEvery of them, are lost.
func (e *Engine) restorePlan(key ident.ID, saved, date string, end, now time.Time) *budgetPlan {
	if saved == "" {
		return nil
	}
	var s savedBudget
	if err := json.Unmarshal([]byte(saved), &s); err != nil {
		e.Log.Error("Failed to read saved budget plan", "err", err, "rule", ruleRef(key))
		return nil
	}
	today, failure := s.Today, s.Failure
	if today.Date != date {
		return nil
	}
	p := &budgetPlan{
		date: date, end: end, run: time.Duration(today.RunMinutes) * time.Minute,
		missed: time.Duration(today.MissedMinutes) * time.Minute, pending: time.Duration(today.PendingMinutes) * time.Minute,
		replanned: today.ReplannedAt, starts: int(today.Starts), failure: failure,
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

// credit adds the time since the last credit to run if the target was on and available then, or
// the planned part of it to missed if the target was off and the conditions true, and records the
// target as it is now. The engine credits when the target changes, so its state between credits is
// the one recorded at the last. A stretch offline counts once the target reports again: as run if
// it was on at both ends, and otherwise as missed for its planned part.
func (p *budgetPlan) credit(now time.Time, t target, m budgetMetrics, key ident.ID) {
	var run, missed time.Duration
	if until := earlier(now, p.end); until.After(p.credited) {
		d, planned := until.Sub(p.credited), time.Duration(0)
		if p.when {
			planned = p.planned(p.credited, until)
		}
		switch {
		case p.away != nil:
			p.away.today.length += d
			p.away.today.planned += planned
		case p.on:
			run = d
		default:
			missed = planned
		}
	}
	switch a := p.away; {
	case a == nil && !t.online:
		p.away = &awayStretch{on: t.on, seen: t.seen}
	case a != nil && t.online && t.seen.After(a.seen):
		if a.on && t.on {
			run += a.today.length
			m.credited(key, a.earlier.length, 0)
		} else {
			missed += a.today.planned
			m.credited(key, 0, a.earlier.planned)
		}
		p.away = nil
	}
	p.run, p.missed, p.pending = p.run+run, p.missed+missed, p.pending+missed
	m.credited(key, run, missed)
	if p.on && t.online && !t.on {
		p.lastOff = now
	}
	p.credited, p.on = now, t.on && t.online && p.away == nil
}

// inBlock says whether a planned block is under way at t.
func (p *budgetPlan) inBlock(t time.Time) bool {
	return slices.ContainsFunc(p.day.Blocks, func(b schedule.Block) bool { return !t.Before(b.Start) && t.Before(b.End) })
}

// replan plans the rest of the day again, once no block is under way, for the blocks still to come
// and the minutes missed, keeping the blocks gone by. Less than a minute missed, as a slow write
// leaves, isn't made up. Call with e.mu held.
func (e *Engine) replan(key ident.ID, b *schedule.Budget, p *budgetPlan, y int, m time.Month, d int, now time.Time) {
	missed := int(p.pending / time.Minute)
	p.pending = 0
	if missed == 0 || p.failure != "" {
		return
	}
	toCome := int(p.planned(now, p.end) / time.Minute)
	lastOff := p.lastOff
	if p.on {
		// It's on as its block ends, so it's about to go off.
		lastOff = now
	}
	rest := schedule.Plan(b.Rest(y, m, d, now, e.Location, toCome+missed, p.starts, lastOff))
	for _, err := range rest.Errors {
		e.Log.Info("Budget replanned with an error", "rule", ruleRef(key), "err", err)
	}
	blocks := slices.DeleteFunc(slices.Clone(p.day.Blocks), func(b schedule.Block) bool { return b.End.After(now) })
	for _, blk := range rest.Blocks {
		// A run that carries straight on from the block just gone, with the target on, is that block,
		// not a new start.
		if n := len(blocks); p.on && n > 0 && blocks[n-1].End.Equal(blk.Start) {
			blocks[n-1].End = blk.End
			continue
		}
		blocks = append(blocks, blk)
	}
	p.day.Blocks = blocks
	p.day.ScheduledMinutes += rest.ScheduledMinutes - toCome - missed
	p.day.ShortfallMinutes = max(0, p.day.TargetMinutes-p.day.ScheduledMinutes)
	p.replanned = now
	e.Log.Info("Replanned budget", "rule", ruleRef(key), "missed", missed, "scheduled", rest.ScheduledMinutes,
		"blocks", len(rest.Blocks))
}

// planned is how much of the time from start to end the plan's blocks cover.
func (p *budgetPlan) planned(start, end time.Time) time.Duration {
	var d time.Duration
	for _, b := range p.day.Blocks {
		if s, e := later(start, b.Start), earlier(end, b.End); e.After(s) {
			d += e.Sub(s)
		}
	}
	return d
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (p *budgetPlan) status() *BudgetDay {
	today := &BudgetDay{
		Date:             p.date,
		TargetSource:     string(p.day.TargetSource),
		TargetMinutes:    int32(p.day.TargetMinutes),
		ScheduledMinutes: int32(p.day.ScheduledMinutes),
		ShortfallMinutes: int32(p.day.ShortfallMinutes),
		RunMinutes:       int32(math.Floor(p.run.Minutes())),
		MissedMinutes:    int32(math.Floor(p.missed.Minutes())),
		PendingMinutes:   int32(math.Floor(p.pending.Minutes())),
		ReplannedAt:      p.replanned,
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

// reportBudget reports a budget hold's status, unless only its run, missed and pending minutes
// changed and they were reported less than checkpointEvery ago. Call with e.mu held.
func (e *Engine) reportBudget(key ident.ID, s *state, now time.Time, r BudgetReport) {
	bs := s.hold.budget
	r.Generation = s.c.Generation
	if prev := bs.reported; prev != nil && sameBudget(*prev, r) {
		x, y := prev.Today, r.Today
		if x == nil || x.RunMinutes == y.RunMinutes && x.MissedMinutes == y.MissedMinutes && x.PendingMinutes == y.PendingMinutes ||
			now.Sub(bs.checkpointed) < checkpointEvery {
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
			e.Log.Error("Failed to save budget plan", "err", err, "rule", ruleRef(key))
		} else {
			bs.saved = string(raw)
		}
	}
	if e.OnBudget != nil {
		e.OnBudget(key, r)
	}
}

// sameBudget compares two reports apart from run, missed and pending minutes.
func sameBudget(a, b BudgetReport) bool {
	if a.Generation != b.Generation || a.Unplanned != b.Unplanned || a.Reason != b.Reason || a.Message != b.Message ||
		(a.Today == nil) != (b.Today == nil) {
		return false
	}
	if a.Today == nil {
		return true
	}
	x, y := *a.Today, *b.Today
	x.RunMinutes, y.RunMinutes, x.MissedMinutes, y.MissedMinutes, x.PendingMinutes, y.PendingMinutes = 0, 0, 0, 0, 0, 0
	return x.Date == y.Date && x.TargetSource == y.TargetSource && x.TargetMinutes == y.TargetMinutes &&
		x.ScheduledMinutes == y.ScheduledMinutes && x.ShortfallMinutes == y.ShortfallMinutes &&
		x.Starts == y.Starts && x.ReplannedAt.Equal(y.ReplannedAt) && slices.EqualFunc(x.Blocks, y.Blocks, func(p, q BudgetBlock) bool {
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
	e.metrics.budget.delete(key)
	if e.OnBudget != nil {
		e.OnBudget(key, BudgetReport{Generation: generation, Cleared: true})
	}
}
