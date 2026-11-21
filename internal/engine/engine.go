package engine

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/clock"
	"oikade.dev/internal/control"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/sun"
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

const (
	labelRule = "rule"
)

// maxWait bounds how long Run sleeps, so a wall clock that steps forward, as NTP does on a
// Raspberry Pi without a clock battery, delays a fire by at most this.
const maxWait = time.Minute

// recheck is how often a Rule with no fire within a week looks again, as at a polar sunrise.
const recheck = 24 * time.Hour

// Schedule is what the engine reports about a Rule's at and sun triggers, or its hold.
type Schedule struct {
	Generation int64
	// Cleared is set when the Rule no longer has at or sun triggers, or is gone.
	Cleared bool
	// Next is the next fire, or when a hold next starts or ends. Zero means none within a week.
	Next time.Time
	// LastFire is the scheduled time of the last fire the engine ran or skipped. Zero means it
	// hasn't changed since the Rule loaded.
	LastFire time.Time
	// Hold is set for a Rule with a hold, which reports Active rather than fires.
	Hold   bool
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
	// Site is where sun triggers are computed for. Nil means they never fire. Set it before Init.
	Site *sun.Site
	// RuleMetrics, if set, gets the rules' metrics: their runs, next changes, holds and budgets, and
	// the changes the engine dropped. Set it before Init.
	RuleMetrics *metrics.Set
	// States keeps holds' state across restarts. Nil keeps it in memory only.
	States HoldStates
	// Controls keeps arbitrated capabilities' state across restarts. Nil keeps it in memory only.
	Controls ControlStates
	// Averages are the rolling averages conditions read, kept in AverageStates. Nil keeps them in
	// memory only.
	Averages      []Average
	AverageStates AverageStates
	// Arbitration, if set, takes this engine's claims, and it runs no arbiters itself.
	Arbitration Arbitration
	// ControlMetrics, if set, gets the winning level of each arbitrated capability.
	ControlMetrics *metrics.Set
	// OnStart, if set, is called when a Rule's actions start. It must not block.
	OnStart func(key ident.ID, t time.Time)
	// OnRun, if set, is called with each outcome Runs counts, perhaps with e.mu held. It must not
	// block.
	OnRun func(key ident.ID, result string)
	// OnSchedule, if set, is called with e.mu held when a Rule with at or sun triggers loads, fires
	// or goes, and when a hold's state or next change changes. It must not block.
	OnSchedule func(key ident.ID, s Schedule)
	// OnRoomReads, if set, is called with e.mu held when the rooms a hold's when has read grow, with
	// all of them. It must not block.
	OnRoomReads func(key ident.ID, reads []RoomRead)
	// OnStopped, if set, is called with e.mu held when why a hold couldn't tell whether it should be
	// active changes: a when it couldn't evaluate, or a budget it couldn't plan. "" means it can
	// again. It must not block.
	OnStopped func(key ident.ID, why string)
	// OnWhyNot, if set, is called with e.mu held when why a hold is inactive changed, as its WhyNot
	// says, or with "" once it's active or can't tell.
	OnWhyNot func(key ident.ID, why string)
	// OnBudget, if set, is called with e.mu held when a budget hold plans, or its status changes.
	// It must not block.
	OnBudget func(key ident.ID, r BudgetReport)
	// OnEnd, if set, hears a run that OnStart heard end, with its result. OnPark, if set, hears a run
	// begin waiting.
	OnEnd  func(key ident.ID, result string)
	OnPark Park

	metrics engineMetrics
	// busy counts the work Idle can't see in the engine's state: changes queued, a wake or save
	// asked for and not yet done, and goroutines working rather than parked.
	busy busyCount
	// armed is when Run's timer fires, or zero for none. Guarded by mu.
	armed time.Time
	// parked are the runs waiting on the clock or a change, by Park's id for them.
	parkMu  sync.Mutex
	parked  map[int]parking
	parkSeq int

	// events are reports, each the changes of one, from one device.
	events chan []driver.Update
	// wake makes Run look at the timers again.
	wake chan struct{}

	mu sync.Mutex
	// suns answers for Site, and is nil without one.
	suns *sunCache
	// membersGen counts changes to rooms' members and discoveries, so a group or room hold compares
	// its own only after one.
	membersGen uint64
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
	// holdWrites are the hold writes the current look at the rules has started, and triggers the
	// flows it set off, for startWrites.
	holdWrites []holdWrite
	averages   averages
	triggers   []pendingTrigger
	// bridgeWrites are Matter's and oikade commands' writes waiting for their device to report them.
	bridgeWrites map[ident.ID]map[string]bridgeWrite

	// saveMu orders saves of arbiters' state, so an older one never lands on a newer one. Take it
	// before mu. savedControls is what was last saved, by device.
	saveMu        sync.Mutex
	savedControls map[ident.ID]string
	// moving are devices whose claims MoveDevices is taking to a new name, which get no arbiters
	// meanwhile.
	moving map[ident.ID]bool
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
	// hold is nil for a flow.
	hold *holdState
}

// Init prepares the engine. Call it before Load, Changed or Run.
func (e *Engine) Init() {
	e.Log = logging.OrDiscard(e.Log)
	e.metrics = newMetrics(e.RuleMetrics, e.ControlMetrics)
	e.events = make(chan []driver.Update, 1024)
	e.wake = make(chan struct{}, 1)
	e.busy.quiet = make(chan struct{}, 1)
	e.parked = map[int]parking{}
	e.rules = map[ident.ID]*state{}
	e.loaded = map[ident.ID]bool{}
	if e.Clock == nil {
		e.Clock = clock.Real{}
	}
	if e.Location == nil {
		e.Location = time.Local
	}
	e.suns = newSunCache(e.Site)
	if e.States == nil {
		e.States = newMemoryStates[HoldState]()
	}
	if e.Controls == nil {
		e.Controls = newMemoryStates[ControlState]()
	}
	if e.AverageStates == nil {
		e.AverageStates = newMemoryStates[AverageState]()
	}
	e.arbs = map[ident.ID]map[string]*arb{}
	e.controlDue = map[ident.ID]bool{}
	e.saveDue = map[ident.ID]bool{}
	e.saveWake = make(chan struct{}, 1)
	e.bridgeWrites = map[ident.ID]map[string]bridgeWrite{}
	e.savedControls = map[ident.ID]string{}
	e.moving = map[ident.ID]bool{}
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
	if old != nil && old.c.hold != nil && c.hold == nil {
		e.metrics.active.Delete(ruleRef(c.Key))
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
	if first {
		if f, ok := s.c.lastFire(s.c.Since, now, e.Location, e.suns); ok {
			if now.Sub(f.At) <= s.c.StartingDeadline {
				s.due = f
			} else {
				e.Log.Info("Skipped missed trigger; it is older than the startingDeadline",
					"rule", ruleRef(key), "type", f.Typ, "scheduled", f.At)
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
	f, ok := s.c.nextFire(now, e.Location, e.suns)
	if !ok {
		s.next = fire{At: now.Add(recheck)}
		return time.Time{}
	}
	s.next = f
	return f.At
}

func (e *Engine) report(key ident.ID, sched Schedule) {
	if sched.Next.IsZero() || sched.Cleared {
		e.metrics.nextChange.Delete(ruleRef(key))
	} else {
		e.metrics.nextChange.Set(float64(sched.Next.Unix()), ruleRef(key))
	}
	if e.OnSchedule != nil {
		e.OnSchedule(key, sched)
	}
}

func (e *Engine) signal() {
	e.busy.Add(1)
	select {
	case e.wake <- struct{}{}:
	default:
		e.busy.Add(-1) // a wake is already asked for
	}
}

// goBusy runs f on its own goroutine, counted as busy until it ends or parks.
func (e *Engine) goBusy(f func()) {
	e.busy.Add(1)
	e.runs.Go(func() {
		defer e.busy.Add(-1)
		f()
	})
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
	_ = e.endClaims(context.Background(), ruleSource(key.Name), allLevels)
	// A Rule unloaded as invalid before it was deleted has a state too.
	if _, ok := e.States.Get(key); ok {
		e.goBusy(func() { e.deleteState(key) })
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
	if s != nil && s.hold != nil {
		// A hold loaded again starts with nothing stopped, so it isn't left to say so.
		e.stopped(key, s.hold, "")
	}
	// The saved plan stays, so a rule fixed the same day carries on with it.
	e.metrics.budget.delete(key)
	delete(e.rules, key)
	// A Rule loaded again, after a delete or an invalid edit, looks for missed fires again.
	delete(e.loaded, key)
	e.metrics.runs.DeleteMatching(labelRule, ruleRef(key))
	e.metrics.nextChange.Delete(ruleRef(key))
	e.metrics.active.Delete(ruleRef(key))
	e.signal()
	return s
}

// Changed implements driver.Sink. It never blocks the driver, and takes a report in as a whole.
func (e *Engine) Changed(changes ...driver.Update) {
	// A report is one device's: a call with several devices' changes is one report from each.
	for len(changes) > 0 {
		n := 1
		for n < len(changes) && changes[n].Device == changes[0].Device {
			n++
		}
		e.queue(changes[:n])
		changes = changes[n:]
	}
}

// queue puts one report on the events queue, or drops it when the engine is behind.
func (e *Engine) queue(changes []driver.Update) {
	e.busy.Add(1)
	select {
	case e.events <- changes:
	default:
		e.busy.Add(-1)
		if e.metrics.dropped != nil {
			e.metrics.dropped.Add(len(changes))
		}
		for _, u := range changes {
			e.Log.Info("Dropped capability change; the rule engine is behind", "device", u.Device, "capability", u.Capability)
		}
	}
}

// Run processes changes and fires timers until ctx is cancelled, then waits for runs in progress
// to stop. Changes don't touch the timer: only loads, removes and fires do.
func (e *Engine) Run(ctx context.Context) {
	// The saver outlives the runs, so it saves what their last writes changed.
	saveCtx, stopSaving := context.WithCancel(context.WithoutCancel(ctx))
	e.averages.mu.Lock()
	e.averages.ctx = saveCtx
	e.averages.mu.Unlock()
	saverDone := make(chan struct{})
	go func() { e.saveControls(saveCtx); close(saverDone) }()
	defer func() {
		e.runs.Wait()
		e.flushAverages(saveCtx)
		stopSaving()
		<-saverDone
	}()
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	woke := false
	for {
		if timer != nil {
			timer.Stop()
		}
		timer = e.arm(ctx)
		if woke {
			woke = false
			e.busy.Add(-1)
		}
		var fired <-chan time.Time
		if timer != nil {
			fired = timer.C()
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case changes := <-e.events:
				e.handle(ctx, changes)
				e.busy.Add(-1)
			case <-e.wake:
				woke = true
				break wait
			case <-fired:
				break wait
			}
		}
	}
}

// arm fires what is due, then returns a timer for the earliest fire still to come, or nil when
// there is none. The timer waits at most maxWait. When something is still due, it asks Run to wake
// and returns nil.
func (e *Engine) arm(ctx context.Context) clock.Timer {
	e.fireDue(ctx)
	next := e.earliest()
	if next.IsZero() {
		e.setArmed(time.Time{})
		return nil
	}
	now := e.Clock.Now()
	if d := next.Sub(now); d > 0 {
		timer := e.Clock.NewTimer(min(d, maxWait))
		e.setArmed(now.Add(min(d, maxWait)))
		return timer
	}
	// Still due: Run fires it again after taking the changes waiting, so something that never
	// stops being due can't shut them out, or the stop.
	e.setArmed(time.Time{})
	e.signal()
	return nil
}

// setArmed records when Run's timer fires, or that there's none, for Idle.
func (e *Engine) setArmed(at time.Time) {
	e.mu.Lock()
	e.armed = at
	e.mu.Unlock()
	e.busy.tell()
}

// earliest is when the next Rule or arbiter is due, or zero.
func (e *Engine) earliest() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.earliestLocked()
}

// earliestLocked is earliest, with e.mu held.
func (e *Engine) earliestLocked() time.Time {
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
			e.triggerAfterWrites(key, s, e.vars(map[string]any{triggerType: f.Typ}))
			sched.LastFire = f.At
		}
		sched.Next = e.reschedule(s, now)
		e.report(key, sched)
	}
	e.startWrites(ctx)
}

// triggerType is the key of a trigger's type in Vars.
const triggerType = "type"

func (e *Engine) vars(trigger map[string]any) Vars {
	v := e.conditionVars(e.Clock.Now(), nil)
	v["trigger"] = trigger
	return v
}

// home is the devices as conditions read them: their values, and where they are and
// what they are. Read it once for every hold a change evaluates.
type home struct {
	devices map[string]map[string]any
	members map[string]hub.Member
}

func (e *Engine) home() *home {
	devices := e.Store.Values()
	e.addAverages(devices, e.Clock.Now())
	return &home{devices: devices, members: e.Store.Members()}
}

// conditionVars are what a condition sees at now. h is the home's devices, or nil to read
// them.
func (e *Engine) conditionVars(now time.Time, h *home) Vars {
	if h == nil {
		h = e.home()
	}
	return Vars{
		"devices":  h.devices,
		"members":  h.members,
		"now":      now,
		"tz":       e.Location.String(),
		"location": e.Location,
		"site":     e.Site,
	}
}

// handle takes in one report, its changes all from one device. Every value is in the store before
// any rule looks, so a hold sees the whole report, and the holds' writes start once all have
// looked.
func (e *Engine) handle(ctx context.Context, changes []driver.Update) {
	dev := changes[0].Device
	var room string
	var types []devicetype.Type
	if snap, ok := e.Store.Get(dev); ok {
		room, types = snap.Room, snap.DeviceTypes
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, u := range changes {
		e.outside(ctx, u)
	}
	now := e.Clock.Now()
	e.observeAverages(changes, now)
	evaluated := false
	var devices *home
	// vars are each change's, read once a rule needs them.
	vars := make([]Vars, len(changes))
	for key, s := range e.rules {
		// A hold with when may read any device, and a budget credits its target's
		// run as it changes.
		if s.hold != nil && s.hold.loaded && (s.c.hold.when != nil || s.c.hold.follow != nil ||
			s.c.hold.budget != nil && s.c.hold.device == dev.Name) {
			if devices == nil {
				devices = e.home()
			}
			e.evaluate(ctx, key, s, now, devices)
			evaluated = true
			continue
		}
		for i, u := range changes {
			if !s.c.matches(u.Device.Name, room, types, u.Capability, u.Value) {
				continue
			}
			if vars[i] == nil {
				vars[i] = e.vars(map[string]any{
					triggerType: TypeChange, "device": u.Device.Name, "capability": u.Capability, "value": u.Value, "previous": u.Previous,
				})
			}
			e.triggerAfterWrites(key, s, vars[i])
		}
	}
	e.startWrites(ctx)
	if evaluated {
		// The evaluation may have moved a hold's next look earlier.
		e.signal()
	}
	close(e.changes)
	e.changes = make(chan struct{})
}

// membersChanged makes Run evaluate again the holds whose when may read a room, and the room holds,
// as handle does on a change.
func (e *Engine) membersChanged(dev ident.ID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	due := false
	e.membersGen++
	for _, s := range e.rules {
		// A hold with when may read the rooms, a room hold writes their members, and a group hold
		// writes a member once it's discovered.
		if s.hold != nil && s.hold.loaded && (s.c.hold.when != nil || s.c.hold.room != "" || slices.Contains(s.c.hold.devices, dev.Name)) {
			s.next = fire{At: now}
			due = true
		}
	}
	if due {
		e.signal()
	}
	// A flow waiting on a room looks again too.
	close(e.changes)
	e.changes = make(chan struct{})
}

// trigger runs the Rule if its condition holds and its mode allows. Call with e.mu held.
func (e *Engine) trigger(ctx context.Context, key ident.ID, s *state, vars Vars) {
	ok, err := s.c.eval(vars)
	switch {
	case err != nil:
		e.Log.Error("Failed to evaluate Rule condition", "err", err, "rule", ruleRef(key))
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

// start launches a run. Call with e.mu held. A rule with no body, from NewTrigger, has nothing to
// run.
func (e *Engine) start(ctx context.Context, key ident.ID, s *state) {
	body := s.c.body
	if body == nil {
		body = func(context.Context) string { return ResultSucceeded }
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
	runCtx, cancel := context.WithCancel(context.WithValue(ctx, inRun{}, true))
	// A run cancelled while it waits is busy again until it ends: it no longer waits for anything.
	// ended makes sure that's counted once, and only before the run ends.
	var ended atomic.Bool
	s.run++
	s.cancel = func() {
		if ended.CompareAndSwap(false, true) {
			e.busy.Add(1)
		}
		cancel()
	}
	run := s.run
	if e.OnStart != nil {
		e.OnStart(key, e.Clock.Now())
	}
	e.goBusy(func() {
		defer cancel()
		defer func() {
			if !ended.CompareAndSwap(false, true) {
				e.busy.Add(-1) // the cancel's
			}
		}()
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

// reportSkipped logs the Room members a rule left out when they differ from the last time's, so a
// rule that writes all day logs them once.
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
		e.Log.Info("Skipped Room members", "rule", ruleRef(key), "devices", skipped)
	}
}

// arbitrates is a Room member left out because it arbitrates the capability, for Apply's error.
func arbitrates(dev ident.ID, name string) string {
	return fmt.Sprintf("%s (arbitrates %s, so the set needs for or until)", dev.Name, name)
}

// otherType is a Room member left out because it isn't one of the rule's device types.
func otherType(dev ident.ID, snap hub.Snapshot) string {
	return fmt.Sprintf("%s (%s, not one of the rule's device types)", dev.Name, cmp.Or(devicetype.Join(snap.DeviceTypes), "no device type"))
}

// act runs one action, and returns whether it worked, and the Room members it left out because
// they arbitrate the capability and the set has no end.
func (e *Engine) act(ctx context.Context, key ident.ID, a action) (ok bool, arbitrated []string) {
	switch {
	case a.delay > 0:
		timer := e.Clock.NewTimer(a.delay)
		unpark := e.park(ctx, e.Clock.Now().Add(a.delay), nil)
		defer unpark()
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C():
		}
		return true, nil
	case ctx.Err() != nil:
		return true, nil
	case a.release != "":
		e.release(ident.ID{Name: a.release})
		_ = e.endClaims(ctx, ruleSource(a.release), allLevels)
		return true, nil
	case a.room != "":
		ok, others, arbitrated := e.setRoom(ctx, key, a)
		e.reportSkipped(key, others)
		return ok, arbitrated
	}
	// Apply has refused a set with no end on an arbitrated capability.
	dev := ident.ID{Name: a.device}
	if e.arbitrated(dev, a.capability) && e.claim(ctx, key, dev, a.capability, e.setClaim(key, a)) {
		return true, nil
	}
	if err := e.Setter.Set(writtenFor(ctx, ruleSource(key.Name)), dev, a.capability, a.value); err != nil {
		e.Log.Error("Failed to run Rule action", "err", err, "rule", ruleRef(key), "device", a.device, "capability", a.capability)
		return false, nil
	}
	return true, nil
}

// setRoom sets a capability on every discovered device in a room that reports it, in name order.
// Membership is read now, not when the run started. It tries every device and reports whether
// all of them succeeded. A member that arbitrates the capability gets a claim, or, when the set
// has no end, is left out and returned in arbitrated. A member of another type is left out and
// returned in others.
func (e *Engine) setRoom(ctx context.Context, key ident.ID, a action) (ok bool, others, arbitrated []string) {
	log := e.Log.With("rule", ruleRef(key))
	ok, matched := true, false
	for _, dev := range e.Store.Room(a.room) {
		snap, found := e.Store.Get(dev)
		if !found || !snap.Discovered || !slices.Contains(snap.Info.Capabilities, a.capability) {
			continue
		}
		if !snap.OfTypes(a.types) {
			others = append(others, otherType(dev, snap))
			continue
		}
		if ctx.Err() != nil {
			return ok, others, arbitrated
		}
		matched = true
		if snap.Controls(a.capability) {
			if !a.ends() {
				arbitrated = append(arbitrated, arbitrates(dev, a.capability))
				continue
			}
			if e.claim(ctx, key, dev, a.capability, e.setClaim(key, a)) {
				continue
			}
		}
		if err := e.Setter.Set(writtenFor(ctx, ruleSource(key.Name)), dev, a.capability, a.value); err != nil {
			log.Error("Failed to run Rule action", "err", err, "device", dev.Name, "room", a.room, "capability", a.capability)
			ok = false
		}
	}
	if !matched {
		log.Debug("Matched no Devices in Room", "room", a.room, "capability", a.capability)
	}
	return ok, others, arbitrated
}

// ends reports whether a set has an end, which a set on an arbitrated capability needs.
func (a action) ends() bool { return a.claimFor > 0 || a.until != "" }

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

type parking struct {
	wake  time.Time
	woken <-chan struct{}
}

// inRun marks a run's context: the run is counted as busy, so it stops counting while it waits.
type inRun struct{}

// park is Park for a wait with ctx: one outside a run, such as a test's, only tells OnPark.
func (e *Engine) park(ctx context.Context, wake time.Time, woken <-chan struct{}) (unpark func()) {
	if ctx.Value(inRun{}) == nil {
		return e.OnPark.Wait(wake, woken)
	}
	return e.Park(wake, woken)
}

// Park says a run of this engine's, such as a flow's, is waiting until wake, or until woken closes
// if that's not nil, and returns what the run calls when it stops waiting. While it waits, it isn't
// busy. OnPark hears it too. Call it only from a run the engine started.
func (e *Engine) Park(wake time.Time, woken <-chan struct{}) (unpark func()) {
	e.parkMu.Lock()
	id := e.parkSeq
	e.parkSeq++
	e.parked[id] = parking{wake, woken}
	e.parkMu.Unlock()
	e.busy.Add(-1)
	heard := e.OnPark.Wait(wake, woken)
	return func() {
		heard()
		e.busy.Add(1)
		e.parkMu.Lock()
		delete(e.parked, id)
		e.parkMu.Unlock()
	}
}

// ParkedUntil is the earliest time a parked run wakes, or zero if none is parked.
func (e *Engine) ParkedUntil() time.Time {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	var next time.Time
	for _, p := range e.parked {
		if next.IsZero() || p.wake.Before(next) {
			next = p.wake
		}
	}
	return next
}

// parkedWaiting says every parked run is still waiting: its time hasn't come and nothing woke it.
func (e *Engine) parkedWaiting() bool {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	now := e.Clock.Now()
	for _, p := range e.parked {
		if !p.wake.After(now) {
			return false
		}
		if p.woken != nil {
			select {
			case <-p.woken:
				return false
			default:
			}
		}
	}
	return true
}

// Wait is p, or nothing if p isn't set.
func (p Park) Wait(wake time.Time, woken <-chan struct{}) func() {
	if p == nil {
		return func() {}
	}
	return p(wake, woken)
}

// Idle reports that the engine has caught up with the clock: nothing is queued or asked for, every
// rule has looked, every write and save has finished, Run's timer is set for a time still to come,
// and every run is waiting for a time still to come or a change. Runs count only if they park
// through Park, as the engine's own waits and a flow's with OnPark set to it do.
func (e *Engine) Idle() bool {
	// The order matters. Whatever makes the state below busy again is counted in busy first, and a
	// run leaves parked only once it has counted itself busy, so busy is read last.
	if !e.stateIdle() || !e.parkedWaiting() {
		return false
	}
	return e.busy.Load() == 0 && len(e.events) == 0
}

// MaybeIdle receives after the engine may have become Idle: when nothing is busy any more, or Run
// sets its timer. It's for one waiter, which checks Idle, waits on it and checks again. A receive
// can be stale, but none sent after a check is lost.
func (e *Engine) MaybeIdle() <-chan struct{} { return e.busy.quiet }

// busyCount is Engine.busy, and tells MaybeIdle when it reaches zero.
type busyCount struct {
	n     atomic.Int64
	quiet chan struct{}
}

func (b *busyCount) Add(d int64) {
	if b.n.Add(d) == 0 {
		b.tell()
	}
}

func (b *busyCount) Load() int64 { return b.n.Load() }

// tell sends to MaybeIdle unless a send is waiting there already.
func (b *busyCount) tell() {
	select {
	case b.quiet <- struct{}{}:
	default:
	}
}

// stateIdle says Run's timer is set for a time still to come and no rule or arbiter is due.
func (e *Engine) stateIdle() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	if !e.earliestLocked().IsZero() && !e.armed.After(now) {
		return false // Run's timer has fired, or isn't set yet
	}
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
	e.metrics.runs.Inc(ruleRef(key), result)
	if e.OnRun != nil {
		e.OnRun(key, result)
	}
}
