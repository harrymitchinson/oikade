package engine

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/sun"
)

// Run results, as reported in oikade_rule_runs_total.
const (
	ResultSucceeded      = "succeeded"
	ResultFailed         = "failed"
	ResultCancelled      = "cancelled"
	ResultBusy           = "busy"
	ResultConditionFalse = "condition_false"
	ResultConditionError = "condition_error"
	// ResultMissed is an at or sun trigger whose time passed while the host was down, longer
	// ago than the Rule's startingDeadline.
	ResultMissed = "missed"
)

// Metric labels.
const (
	labelNamespace = "namespace"
	labelRule      = "rule"
)

// ReasonNoLocation is the Scheduled reason for a sun trigger with no location.
const ReasonNoLocation = "NoLocation"

// maxWait bounds how long Run sleeps, so a wall clock that steps forward, as NTP does on a
// Raspberry Pi without a clock battery, delays a fire by at most this.
const maxWait = time.Minute

// recheck is how often a Rule with no fire within a week looks again, as at a polar sunrise.
const recheck = 24 * time.Hour

// Schedule is what the engine reports about a Rule's at and sun triggers, or its hold.
type Schedule struct {
	// Generation is the Rule generation the schedule is for.
	Generation int64
	// Cleared is set when the Rule no longer has at or sun triggers, or is gone.
	Cleared bool
	// Next is the next fire, or when a hold next starts or ends. Zero means none within a week.
	Next time.Time
	// LastFire is the scheduled time of the last fire the engine ran or skipped. Zero means it
	// hasn't changed since the Rule loaded.
	LastFire time.Time
	// Reason is ReasonNoLocation when sun triggers or sun windows can't fire, and empty otherwise.
	Reason string
	// Hold is set for a Rule with a hold, which reports Active rather than fires.
	Hold bool
	// Active is whether the hold is active.
	Active bool
}

// Engine is a driver.Sink that runs Rules on capability changes.
type Engine struct {
	hub.NopSink
	Store    *hub.Store
	Setter   driver.Setter
	Clock    clock.Clock
	Location *time.Location
	Log      *slog.Logger
	// Site is where sun triggers are computed for. Nil means they never fire.
	Site *sun.Site
	// Runs counts trigger outcomes by namespace, rule and result.
	Runs *prometheus.CounterVec
	// NextChange is the next fire's Unix time, by namespace and rule.
	NextChange *prometheus.GaugeVec
	// Dropped counts changes dropped because the engine fell behind.
	Dropped prometheus.Counter
	// Active is 1 while a Rule's hold is active, by namespace and rule.
	Active *prometheus.GaugeVec
	// States keeps holds' state across restarts. Nil keeps it in memory only.
	States HoldStates
	// Controls keeps arbitrated capabilities' state across restarts. Nil keeps it in memory only.
	Controls ControlStates
	// Arbitration, if set, takes this engine's claims, and it runs no arbiters itself.
	Arbitration Arbitration
	// ControlLevel is the winning level of each arbitrated capability.
	ControlLevel *prometheus.GaugeVec
	// OnStart, if set, is called when a Rule's actions start. It must not block.
	OnStart func(key ident.ID, t time.Time)
	// OnRun, if set, is called with each outcome Runs counts, perhaps with e.mu held. It must not block.
	OnRun func(key ident.ID, result string)
	// OnSchedule, if set, is called with e.mu held when a Rule with at or sun triggers loads or
	// fires. It must not block.
	OnSchedule func(key ident.ID, s Schedule)
	// OnBudget, if set, is called with e.mu held when a budget hold plans, or its status changes.
	// It must not block.
	OnBudget func(key ident.ID, r BudgetReport)
	// Budget are the budget holds' metrics. Nil exports none.
	Budget *BudgetMetrics
	// OnEnd, if set, hears a run that OnStart heard end, with its result. OnPark, if set, hears a run
	// begin waiting. Together they let a simulated home tell a run that's waiting from one that's
	// working.
	OnEnd  func(key ident.ID, result string)
	OnPark Park

	events chan driver.Update
	// wake makes Run look at the timers again.
	wake chan struct{}

	mu sync.Mutex
	// skipped is the Room members each rule last left out.
	skipped map[ident.ID][]string
	rules   map[ident.ID]*state
	// loaded holds every Rule loaded since Init, so only the first load looks for missed fires.
	loaded map[ident.ID]bool
	runs   sync.WaitGroup

	// arbs are the arbitrated capabilities by device, and controlDue the devices to look at again.
	arbs       map[ident.ID]map[string]*arb
	controlDue map[ident.ID]bool
	// saveDue are the devices whose arbiters saveControls saves next.
	saveDue  map[ident.ID]bool
	saveWake chan struct{}
	// changes closes after a change has been handled, for flows waiting on a condition.
	changes chan struct{}
	// bridgeWrites are the Matter bridges' writes waiting for their device to report them.
	bridgeWrites map[ident.ID]map[string]bridgeWrite

	// saveMu orders saves of arbiters' state, so an older one never lands on a newer one. Take it
	// before mu. savedControls is what was last saved, by device.
	saveMu        sync.Mutex
	savedControls map[ident.ID]string
}

type state struct {
	c *Compiled
	// cancel stops the run in progress; nil when idle.
	cancel context.CancelFunc
	// run identifies the latest run, so a cancelled run doesn't mark a newer one idle.
	run uint64
	// next is the next timer fire, and due a missed one to run now. Zero At means none. A next
	// with no type is a recheck, which only looks for the next fire again. For a hold, next is
	// when to evaluate it again.
	next, due fire
	// hold is nil for a moment.
	hold *holdState
}

// NewRunsCounter returns the counter Engine.Runs expects.
func NewRunsCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "oikade_rule_runs_total",
		Help: "Rule trigger outcomes.",
	}, []string{labelNamespace, labelRule, "result"})
}

// NewNextChangeGauge returns the gauge Engine.NextChange expects.
func NewNextChangeGauge() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "oikade_rule_next_change_timestamp_seconds",
		Help: "When a Rule's next at or sun trigger fires, or its hold next starts or ends, as a Unix time.",
	}, []string{labelNamespace, labelRule})
}

// NewDroppedCounter returns the counter Engine.Dropped expects.
func NewDroppedCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oikade_rule_events_dropped_total",
		Help: "Capability changes the rule engine dropped because it fell behind.",
	})
}

// Init prepares the engine. Call it before Load, Changed or Run.
func (e *Engine) Init() {
	e.Log = logging.OrDiscard(e.Log)
	e.events = make(chan driver.Update, 1024)
	e.wake = make(chan struct{}, 1)
	e.rules = map[ident.ID]*state{}
	e.loaded = map[ident.ID]bool{}
	if e.Clock == nil {
		e.Clock = clock.Real{}
	}
	if e.Location == nil {
		e.Location = time.Local
	}
	if e.States == nil {
		e.States = newMemoryStates[HoldState]()
	}
	if e.Controls == nil {
		e.Controls = newMemoryStates[ControlState]()
	}
	e.arbs = map[ident.ID]map[string]*arb{}
	e.controlDue = map[ident.ID]bool{}
	e.saveDue = map[ident.ID]bool{}
	e.saveWake = make(chan struct{}, 1)
	e.bridgeWrites = map[ident.ID]map[string]bridgeWrite{}
	e.savedControls = map[ident.ID]string{}
	e.changes = make(chan struct{})
}

// Load adds or replaces a Rule. A run in progress for an older generation is cancelled;
// loading the same generation again is a no-op.
func (e *Engine) Load(c *Compiled) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.rules[c.Key]; s != nil {
		if s.c.Generation == c.Generation {
			return
		}
		if s.cancel != nil {
			s.cancel()
		}
	}
	if c.sinceLoad {
		c.Since, c.sinceLoad = e.Clock.Now(), false
	}
	old := e.rules[c.Key]
	reported := old != nil && (len(old.c.timers) > 0 || old.c.hold != nil)
	s := &state{c: c}
	e.rules[c.Key] = s
	first := !e.loaded[c.Key]
	e.loaded[c.Key] = true
	switch {
	case c.hold != nil:
		// Run evaluates it, once the saved states are in. A new generation keeps the old one's
		// state, and any write it has running.
		s.hold = &holdState{}
		if old != nil && old.hold != nil {
			s.hold = old.hold
		}
		s.next = fire{At: e.Clock.Now()}
	case len(c.timers) > 0:
		e.schedule(c.Key, s, first)
	case reported:
		e.report(c.Key, Schedule{Generation: c.Generation, Cleared: true})
	}
	if old != nil && old.c.hold != nil && c.hold == nil && e.Active != nil {
		e.Active.DeleteLabelValues(c.Key.Namespace, c.Key.Name)
	}
	if old != nil && old.c.hold != nil && old.c.hold.budget != nil && (c.hold == nil || c.hold.budget == nil) {
		e.clearBudget(c.Key, c.Generation)
		if s.hold != nil {
			s.hold.budget = nil
		}
	}
	e.signal()
}

// schedule works out a Rule's next fire and, on its first load, the missed fire to run. Call
// with e.mu held.
func (e *Engine) schedule(key ident.ID, s *state, first bool) {
	now := e.Clock.Now()
	sched := Schedule{Generation: s.c.Generation}
	if s.c.needsSite() && e.Site == nil {
		sched.Reason = ReasonNoLocation
		e.Log.Info("Sun triggers won't fire; set --latitude and --longitude", "rule", key)
	}
	if first {
		if f, ok := s.c.lastFire(s.c.Since, now, e.Location, e.Site); ok {
			if now.Sub(f.At) <= s.c.StartingDeadline {
				s.due = f
			} else {
				e.Log.Info("Skipped missed trigger; it is older than the startingDeadline",
					"rule", key, "type", f.Typ, "scheduled", f.At)
				e.count(key, ResultMissed)
				sched.LastFire = f.At
			}
		}
	}
	sched.Next = e.reschedule(s, now)
	e.report(key, sched)
}

// reschedule sets the Rule's next fire after now and returns it, or zero when there is none
// within a week. Call with e.mu held.
func (e *Engine) reschedule(s *state, now time.Time) time.Time {
	f, ok := s.c.nextFire(now, e.Location, e.Site)
	if !ok {
		s.next = fire{At: now.Add(recheck)}
		return time.Time{}
	}
	s.next = f
	return f.At
}

func (e *Engine) report(key ident.ID, sched Schedule) {
	if e.NextChange != nil {
		if sched.Next.IsZero() || sched.Cleared {
			e.NextChange.DeleteLabelValues(key.Namespace, key.Name)
		} else {
			e.NextChange.WithLabelValues(key.Namespace, key.Name).Set(float64(sched.Next.Unix()))
		}
	}
	if e.OnSchedule != nil {
		e.OnSchedule(key, sched)
	}
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Remove drops a deleted Rule, cancels its run and forgets its hold's state. A hold that is
// active leaves its device as it is.
func (e *Engine) Remove(key ident.ID) {
	s := e.unload(key)
	e.mu.Lock()
	if s != nil && s.hold != nil {
		// A write still running deletes the state again once it ends.
		s.hold.deleted = true
	}
	e.mu.Unlock()
	// A deleted Rule's claims end with it.
	_ = e.endClaims(context.Background(), key.Namespace, ruleSource(key.Name), allLevels)
	// A Rule unloaded as invalid before it was deleted has a state too.
	if _, ok := e.States.Get(key); ok {
		e.runs.Go(func() { e.deleteState(key) })
	}
}

// Unload drops a Rule that is no longer valid and cancels its run. Its hold's state is kept, so
// a fixed Rule carries on.
func (e *Engine) Unload(key ident.ID) {
	e.unload(key)
}

// unload drops a Rule and returns its state, if it was loaded.
func (e *Engine) unload(key ident.ID) *state {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.rules[key]
	if s != nil && s.cancel != nil {
		s.cancel()
	}
	if s != nil && (len(s.c.timers) > 0 || s.c.hold != nil) {
		e.report(key, Schedule{Generation: s.c.Generation, Cleared: true})
	}
	// The saved plan stays, so a rule fixed the same day carries on with it.
	e.Budget.delete(key)
	delete(e.rules, key)
	// A Rule loaded again, after a delete or an invalid edit, looks for missed fires again.
	delete(e.loaded, key)
	if e.Runs != nil {
		e.Runs.DeletePartialMatch(prometheus.Labels{labelNamespace: key.Namespace, labelRule: key.Name})
	}
	if e.NextChange != nil {
		e.NextChange.DeleteLabelValues(key.Namespace, key.Name)
	}
	if e.Active != nil {
		e.Active.DeleteLabelValues(key.Namespace, key.Name)
	}
	e.signal()
	return s
}

// Changed implements driver.Sink. It never blocks the driver.
func (e *Engine) Changed(u driver.Update) {
	select {
	case e.events <- u:
	default:
		if e.Dropped != nil {
			e.Dropped.Inc()
		}
		e.Log.Info("Dropped capability change; the rule engine is behind", "device", u.Device, "capability", u.Capability)
	}
}

// Run processes changes and fires timers until ctx is cancelled, then waits for runs in progress
// to stop. Changes don't touch the timer: only loads, removes and fires do.
func (e *Engine) Run(ctx context.Context) {
	// The saver outlives the runs, so it saves what their last writes changed.
	saveCtx, stopSaving := context.WithCancel(context.WithoutCancel(ctx))
	saverDone := make(chan struct{})
	go func() { e.saveControls(saveCtx); close(saverDone) }()
	defer func() {
		e.runs.Wait()
		stopSaving()
		<-saverDone
	}()
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if timer != nil {
			timer.Stop()
		}
		timer = e.arm(ctx)
		var fired <-chan time.Time
		if timer != nil {
			fired = timer.C()
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case u := <-e.events:
				e.handle(ctx, u)
			case <-e.wake:
				break wait
			case <-fired:
				break wait
			}
		}
	}
}

// arm fires what is due, then returns a timer for the earliest fire still to come, or nil when
// there is none. The timer waits at most maxWait.
func (e *Engine) arm(ctx context.Context) clock.Timer {
	for {
		e.fireDue(ctx)
		next := e.earliest()
		if next.IsZero() {
			return nil
		}
		if d := next.Sub(e.Clock.Now()); d > 0 {
			return e.Clock.NewTimer(min(d, maxWait))
		}
	}
}

// earliest returns the earliest next fire of any Rule, or zero.
func (e *Engine) earliest() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	var next time.Time
	for _, s := range e.rules {
		if at := s.next.At; !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	for _, arbs := range e.arbs {
		for _, ar := range arbs {
			if at := ar.next; !at.IsZero() && (next.IsZero() || at.Before(next)) {
				next = at
			}
		}
	}
	if len(e.controlDue) > 0 {
		next = e.Clock.Now()
	}
	return next
}

// fireDue fires the missed fires found on load, and every timer whose time has come. A timer more
// than one fire behind, as after a long pause, fires once.
func (e *Engine) fireDue(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	for dev := range e.controlDue {
		delete(e.controlDue, dev)
		e.syncControls(ctx, dev)
	}
	for dev, arbs := range e.arbs {
		for name, ar := range arbs {
			if !ar.next.IsZero() && !now.Before(ar.next) {
				ar.next = time.Time{}
				e.decide(ctx, dev, name, ar)
			}
		}
	}
	for key, s := range e.rules {
		if s.hold != nil {
			if !s.next.At.IsZero() && !now.Before(s.next.At) {
				e.evaluate(ctx, key, s, now, nil)
			}
			continue
		}
		var f fire
		switch {
		case !s.due.At.IsZero():
			f, s.due = s.due, fire{}
		case !s.next.At.IsZero() && !now.Before(s.next.At):
			f = s.next
		default:
			continue
		}
		sched := Schedule{Generation: s.c.Generation}
		if f.Typ != "" {
			e.trigger(ctx, key, s, e.vars(key.Namespace, map[string]any{triggerType: f.Typ}))
			sched.LastFire = f.At
		}
		sched.Next = e.reschedule(s, now)
		e.report(key, sched)
	}
}

// triggerType is the key of a trigger's type in Vars.
const triggerType = "type"

func (e *Engine) vars(namespace string, trigger map[string]any) Vars {
	return Vars{
		"devices": e.Store.Values(namespace),
		"trigger": trigger,
		"now":     e.Clock.Now(),
		"tz":      e.Location.String(),
	}
}

func (e *Engine) handle(ctx context.Context, u driver.Update) {
	vars := e.vars(u.Device.Namespace, map[string]any{
		triggerType: TypeChange, "device": u.Device.Name, "capability": u.Capability, "value": u.Value, "previous": u.Previous,
	})
	var room string
	if snap, ok := e.Store.Get(u.Device); ok {
		room = snap.Room
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.outside(ctx, u)
	now := e.Clock.Now()
	evaluated := false
	var devices map[string]map[string]any
	for key, s := range e.rules {
		if key.Namespace != u.Device.Namespace {
			continue
		}
		// A hold with when may read any device in its namespace, and a budget credits its target's
		// run as it changes.
		if s.hold != nil && s.hold.loaded && (s.c.hold.when != nil || s.c.hold.budget != nil && s.c.hold.device == u.Device.Name) {
			if devices == nil {
				devices = e.Store.Values(u.Device.Namespace)
			}
			e.evaluate(ctx, key, s, now, devices)
			evaluated = true
			continue
		}
		if s.c.matches(u.Device.Name, room, u.Capability) {
			e.trigger(ctx, key, s, vars)
		}
	}
	if evaluated {
		// The evaluation may have moved a hold's next look earlier.
		e.signal()
	}
	close(e.changes)
	e.changes = make(chan struct{})
}

// trigger runs the Rule if its condition holds and its mode allows. Call with e.mu held.
func (e *Engine) trigger(ctx context.Context, key ident.ID, s *state, vars Vars) {
	ok, err := s.c.eval(vars)
	switch {
	case err != nil:
		e.Log.Error("Failed to evaluate Rule condition", "err", err, "rule", key)
		e.count(key, ResultConditionError)
		return
	case !ok:
		e.count(key, ResultConditionFalse)
		return
	}
	if s.cancel != nil {
		if s.c.Mode == ModeSingle {
			e.count(key, ResultBusy)
			return
		}
		s.cancel()
	}
	e.start(ctx, key, s)
}

// start launches a run. Call with e.mu held.
func (e *Engine) start(ctx context.Context, key ident.ID, s *state) {
	c := s.c
	body := func(ctx context.Context) string { return e.execute(ctx, key, c) }
	if c.body != nil {
		body = c.body
	}
	e.launch(ctx, key, s, body)
}

// Resume continues a flow's run from before a restart, in place of a new one, so triggers treat it
// as a run in progress. Call it after Load and before Run. It reports false for an unknown flow.
func (e *Engine) Resume(ctx context.Context, key ident.ID, body Body) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.rules[key]
	if s == nil {
		return false
	}
	e.launch(ctx, key, s, body)
	return true
}

// launch runs body as the rule's run in progress. Call with e.mu held.
func (e *Engine) launch(ctx context.Context, key ident.ID, s *state, body Body) {
	runCtx, cancel := context.WithCancel(ctx)
	s.run++
	s.cancel = cancel
	run := s.run
	if e.OnStart != nil {
		e.OnStart(key, e.Clock.Now())
	}
	e.runs.Go(func() {
		defer cancel()
		result := body(runCtx)
		e.count(key, result)
		if e.OnEnd != nil {
			defer e.OnEnd(key, result)
		}

		e.mu.Lock()
		defer e.mu.Unlock()
		if cur := e.rules[key]; cur == s && s.run == run {
			s.cancel = nil
		}
	})
}

// execute runs the actions in order. A failing device action stops the run; a room action that
// fails on some devices lets the run continue, so a dead bulb can't leave the rest of the room on,
// and the run counts as failed at the end.
func (e *Engine) execute(ctx context.Context, key ident.ID, c *Compiled) string {
	result, skipped := e.runActions(ctx, key, c)
	e.reportSkipped(key, skipped)
	return result
}

// reportSkipped logs the Room members a run left out when they differ from the last run's, so a
// moment that fires all day logs them once.
func (e *Engine) reportSkipped(key ident.ID, skipped []string) {
	slices.Sort(skipped)
	skipped = slices.Compact(skipped)
	e.mu.Lock()
	same := slices.Equal(e.skipped[key], skipped)
	if !same {
		if e.skipped == nil {
			e.skipped = map[ident.ID][]string{}
		}
		e.skipped[key] = skipped
	}
	e.mu.Unlock()
	if !same && len(skipped) > 0 {
		e.Log.Info("Skipped Room members", "rule", key, "devices", skipped)
	}
}

// arbitrates is a Room member left out because it arbitrates the capability, for the skipped log.
func arbitrates(dev ident.ID, name string) string {
	return fmt.Sprintf("%s (arbitrates %s, so the set needs for or until)", dev.Name, name)
}

// otherType is a Room member left out because it isn't one of the rule's device types.
func otherType(dev ident.ID, snap hub.Snapshot) string {
	return fmt.Sprintf("%s (%s, not one of the rule's device types)", dev.Name, or(devicetype.Join(snap.DeviceTypes), "no device type"))
}

func or(s, otherwise string) string {
	if s == "" {
		return otherwise
	}
	return s
}

// runActions is execute, returning the Room members left out as well.
func (e *Engine) runActions(ctx context.Context, key ident.ID, c *Compiled) (result string, skipped []string) {
	log := e.Log.With("rule", key)
	failed := false
	for _, a := range c.actions {
		if a.delay > 0 {
			timer := e.Clock.NewTimer(a.delay)
			unpark := e.OnPark.Wait(e.Clock.Now().Add(a.delay), nil)
			select {
			case <-ctx.Done():
				timer.Stop()
				unpark()
				return ResultCancelled, skipped
			case <-timer.C():
			}
			unpark()
			continue
		}
		if ctx.Err() != nil {
			return ResultCancelled, skipped
		}
		if a.release != "" {
			e.release(ident.ID{Namespace: key.Namespace, Name: a.release})
			_ = e.endClaims(ctx, key.Namespace, ruleSource(a.release), allLevels)
			continue
		}
		if a.room != "" {
			ok, left := e.setRoom(ctx, key, a)
			if !ok {
				failed = true
			}
			skipped = append(skipped, left...)
			if ctx.Err() != nil {
				return ResultCancelled, skipped
			}
			continue
		}
		dev := ident.ID{Namespace: key.Namespace, Name: a.device}
		if e.arbitrated(dev, a.capability) {
			if !a.ends() {
				log.Info("Skipped set on an arbitrated capability; it needs for or until", "device", a.device, "capability", a.capability)
				continue
			}
			if e.claim(ctx, key, dev, a.capability, e.setClaim(key, a)) {
				continue
			}
		}
		if err := e.Setter.Set(ctx, dev, a.capability, a.value); err != nil {
			log.Error("Failed to run Rule action", "err", err, "device", a.device, "capability", a.capability)
			return ResultFailed, skipped
		}
	}
	if failed {
		return ResultFailed, skipped
	}
	return ResultSucceeded, skipped
}

// setRoom sets a capability on every discovered device in a room that reports it, in name order.
// Membership is read now, not when the run started. It tries every device and reports whether
// all of them succeeded. A member that arbitrates the capability gets a claim, or, when the set
// has no end, is left out and returned.
func (e *Engine) setRoom(ctx context.Context, key ident.ID, a action) (ok bool, skipped []string) {
	log := e.Log.With("rule", key)
	ok, matched := true, false
	for _, dev := range e.Store.Room(key.Namespace, a.room) {
		snap, found := e.Store.Get(dev)
		if !found || !snap.Discovered || !slices.Contains(snap.Info.Capabilities, a.capability) {
			continue
		}
		if !snap.OfTypes(a.types) {
			skipped = append(skipped, otherType(dev, snap))
			continue
		}
		if ctx.Err() != nil {
			return ok, skipped
		}
		matched = true
		if snap.Controls(a.capability) {
			if !a.ends() {
				skipped = append(skipped, arbitrates(dev, a.capability))
				continue
			}
			if e.claim(ctx, key, dev, a.capability, e.setClaim(key, a)) {
				continue
			}
		}
		if err := e.Setter.Set(ctx, dev, a.capability, a.value); err != nil {
			log.Error("Failed to run Rule action", "err", err, "device", dev.Name, "room", a.room, "capability", a.capability)
			ok = false
		}
	}
	if !matched {
		log.Debug("Matched no Devices in Room", "room", a.room, "capability", a.capability)
	}
	return ok, skipped
}

// ends reports whether a set has an end, which a set on an arbitrated capability needs.
func (a action) ends() bool { return a.claimFor > 0 || a.until != "" }

// setClaim is a set's claim on an arbitrated capability.
func (e *Engine) setClaim(key ident.ID, a action) control.Claim {
	c := control.Claim{
		Source: ruleSource(key.Name), Level: claimLevel(a.priority), Value: capability.Float(a.value),
		UntilNext: a.until == UntilNext,
	}
	if a.claimFor > 0 {
		c.Expires = e.Clock.Now().Add(a.claimFor)
	}
	return c
}

// Park hears that a run is waiting until wake, or until woken closes if that's not nil, and returns
// what the run calls when it stops. It must not block.
type Park func(wake time.Time, woken <-chan struct{}) (unpark func())

// Wait is p, or nothing if p isn't set.
func (p Park) Wait(wake time.Time, woken <-chan struct{}) func() {
	if p == nil {
		return func() {}
	}
	return p(wake, woken)
}

// Idle reports that the engine has caught up with the clock: nothing is queued, every rule has looked
// and every write has finished. waiters says whether something waits on the clock, which is how a
// timer just armed shows. A run's own progress isn't covered: see OnEnd and OnPark.
func (e *Engine) Idle(waiters func() bool) bool {
	e.mu.Lock()
	empty := len(e.rules) == 0 && len(e.arbs) == 0
	e.mu.Unlock()
	if len(e.events) > 0 || !empty && !e.earliest().IsZero() && !waiters() {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	if len(e.controlDue) > 0 {
		return false
	}
	for _, arbs := range e.arbs {
		for _, ar := range arbs {
			if ar.writing || !ar.next.IsZero() && !ar.next.After(now) {
				return false
			}
		}
	}
	for _, s := range e.rules {
		if s.hold != nil && (!s.hold.loaded || s.hold.writing || s.next.At.IsZero()) {
			return false
		}
		if !s.next.At.IsZero() && !s.next.At.After(now) {
			return false
		}
	}
	return true
}

// Earliest is when the engine next has something to do without a device changing. Zero means
// nothing.
func (e *Engine) Earliest() time.Time { return e.earliest() }

func (e *Engine) count(key ident.ID, result string) {
	if e.Runs != nil {
		e.Runs.WithLabelValues(key.Namespace, key.Name, result).Inc()
	}
	if e.OnRun != nil {
		e.OnRun(key, result)
	}
}
