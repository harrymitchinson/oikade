/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
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
	// ResultMissed is an at or sun trigger whose time passed while the runtime was down, longer
	// ago than the Rule's startingDeadline.
	ResultMissed = "missed"
)

// Metric labels.
const (
	labelNamespace = "namespace"
	labelRule      = "rule"
)

// ReasonNoLocation is the Scheduled reason for a sun trigger on a runtime without a location.
const ReasonNoLocation = "NoLocation"

// Schedule is what the engine reports about a Rule's at and sun triggers.
type Schedule struct {
	// Next is the next fire. Zero means none within a week.
	Next time.Time
	// LastFire is the scheduled time of the last fire the engine ran or skipped. Zero means it
	// hasn't changed since the Rule loaded.
	LastFire time.Time
	// Reason is ReasonNoLocation when sun triggers can't fire, and empty otherwise.
	Reason string
}

// Engine is a driver.Sink that runs Rules on capability changes.
type Engine struct {
	hub.NopSink
	Store    *hub.Store
	Setter   driver.Setter
	Clock    clock.Clock
	Location *time.Location
	Log      logr.Logger
	// Site is where sun triggers are computed for. Nil means they never fire.
	Site *sun.Site
	// Runs counts trigger outcomes by namespace, rule and result.
	Runs *prometheus.CounterVec
	// NextChange is the next fire's Unix time, by namespace and rule.
	NextChange *prometheus.GaugeVec
	// Dropped counts changes dropped because the engine fell behind.
	Dropped prometheus.Counter
	// OnStart, if set, is called when a Rule's actions start. It must not block.
	OnStart func(key types.NamespacedName, t time.Time)
	// OnSchedule, if set, is called with e.mu held when a Rule with at or sun triggers loads or
	// fires. It must not block.
	OnSchedule func(key types.NamespacedName, s Schedule)

	events chan driver.Update
	// wake makes Run look at the timers again.
	wake chan struct{}

	mu    sync.Mutex
	rules map[types.NamespacedName]*state
	// loaded holds every Rule loaded since Init, so only the first load looks for missed fires.
	loaded map[types.NamespacedName]bool
	runs   sync.WaitGroup
}

type state struct {
	c *Compiled
	// cancel stops the run in progress; nil when idle.
	cancel context.CancelFunc
	// run identifies the latest run, so a cancelled run doesn't mark a newer one idle.
	run uint64
	// next is the next timer fire, and due a missed one to run now. Zero At means none.
	next, due fire
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
		Help: "When a Rule's next at or sun trigger fires, as a Unix time.",
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
	e.events = make(chan driver.Update, 1024)
	e.wake = make(chan struct{}, 1)
	e.rules = map[types.NamespacedName]*state{}
	e.loaded = map[types.NamespacedName]bool{}
	if e.Clock == nil {
		e.Clock = clock.RealClock{}
	}
	if e.Location == nil {
		e.Location = time.Local
	}
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
	s := &state{c: c}
	e.rules[c.Key] = s
	first := !e.loaded[c.Key]
	e.loaded[c.Key] = true
	if len(c.timers) > 0 {
		e.schedule(c.Key, s, first)
	}
	e.signal()
}

// schedule works out a Rule's next fire and, on its first load, the missed fire to run. Call
// with e.mu held.
func (e *Engine) schedule(key types.NamespacedName, s *state, first bool) {
	now := e.Clock.Now()
	sched := Schedule{}
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
	s.next, _ = s.c.nextFire(now, e.Location, e.Site)
	sched.Next = s.next.At
	e.report(key, sched)
}

func (e *Engine) report(key types.NamespacedName, sched Schedule) {
	if e.NextChange != nil {
		if sched.Next.IsZero() {
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

// Remove drops a Rule and cancels its run.
func (e *Engine) Remove(key types.NamespacedName) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.rules[key]; s != nil && s.cancel != nil {
		s.cancel()
	}
	delete(e.rules, key)
	if e.Runs != nil {
		e.Runs.DeletePartialMatch(prometheus.Labels{labelNamespace: key.Namespace, labelRule: key.Name})
	}
	if e.NextChange != nil {
		e.NextChange.DeleteLabelValues(key.Namespace, key.Name)
	}
	e.signal()
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
// to stop.
func (e *Engine) Run(ctx context.Context) {
	defer e.runs.Wait()
	for {
		e.fireDue(ctx)
		timer, due := e.timer()
		if due {
			continue
		}
		var fired <-chan time.Time
		if timer != nil {
			fired = timer.C()
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case u := <-e.events:
			e.handle(ctx, u)
		case <-e.wake:
		case <-fired:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// timer returns a timer for the earliest fire, or nil when there is none. It reports true instead
// when that fire is already due.
func (e *Engine) timer() (clock.Timer, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var next time.Time
	for _, s := range e.rules {
		if at := s.next.At; !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	if next.IsZero() {
		return nil, false
	}
	d := next.Sub(e.Clock.Now())
	if d <= 0 {
		return nil, true
	}
	return e.Clock.NewTimer(d), false
}

// fireDue fires the missed fires found on load, and every timer whose time has come. A timer more
// than one fire behind, as after a long pause, fires once.
func (e *Engine) fireDue(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	for key, s := range e.rules {
		var f fire
		switch {
		case !s.due.At.IsZero():
			f, s.due = s.due, fire{}
		case !s.next.At.IsZero() && !now.Before(s.next.At):
			f = s.next
		default:
			continue
		}
		e.trigger(ctx, key, s, e.vars(key.Namespace, map[string]any{"type": f.Typ}))
		s.next, _ = s.c.nextFire(now, e.Location, e.Site)
		e.report(key, Schedule{Next: s.next.At, LastFire: f.At})
	}
}

func (e *Engine) vars(namespace string, trigger map[string]any) map[string]any {
	return map[string]any{
		"devices": e.Store.Values(namespace),
		"trigger": trigger,
		"now":     e.Clock.Now(),
		"tz":      e.Location.String(),
	}
}

func (e *Engine) handle(ctx context.Context, u driver.Update) {
	vars := e.vars(u.Device.Namespace, map[string]any{
		"type": TypeChange, "device": u.Device.Name, "capability": u.Capability, "value": u.Value, "previous": u.Previous,
	})
	var room string
	if snap, ok := e.Store.Get(u.Device); ok {
		room = snap.Room
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for key, s := range e.rules {
		if key.Namespace != u.Device.Namespace || !s.c.matches(u.Device.Name, room, u.Capability) {
			continue
		}
		e.trigger(ctx, key, s, vars)
	}
}

// trigger runs the Rule if its condition holds and its mode allows. Call with e.mu held.
func (e *Engine) trigger(ctx context.Context, key types.NamespacedName, s *state, vars map[string]any) {
	ok, err := s.c.eval(vars)
	switch {
	case err != nil:
		e.Log.Error(err, "Failed to evaluate Rule condition", "rule", key)
		e.count(key, ResultConditionError)
		return
	case !ok:
		e.count(key, ResultConditionFalse)
		return
	}
	if s.cancel != nil {
		if s.c.Mode == homev1alpha1.RuleModeSingle {
			e.count(key, ResultBusy)
			return
		}
		s.cancel()
	}
	e.start(ctx, key, s)
}

// start launches a run. Call with e.mu held.
func (e *Engine) start(ctx context.Context, key types.NamespacedName, s *state) {
	runCtx, cancel := context.WithCancel(ctx)
	s.run++
	s.cancel = cancel
	run, c := s.run, s.c
	if e.OnStart != nil {
		e.OnStart(key, e.Clock.Now())
	}
	e.runs.Go(func() {
		defer cancel()
		result := e.execute(runCtx, key, c)
		e.count(key, result)

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
func (e *Engine) execute(ctx context.Context, key types.NamespacedName, c *Compiled) string {
	log := e.Log.WithValues("rule", key)
	failed := false
	for _, a := range c.actions {
		if a.delay > 0 {
			select {
			case <-ctx.Done():
				return ResultCancelled
			case <-e.Clock.After(a.delay):
			}
			continue
		}
		if ctx.Err() != nil {
			return ResultCancelled
		}
		if a.room != "" {
			if !e.setRoom(ctx, key, a) {
				failed = true
			}
			if ctx.Err() != nil {
				return ResultCancelled
			}
			continue
		}
		dev := types.NamespacedName{Namespace: key.Namespace, Name: a.device}
		if err := e.Setter.Set(ctx, dev, a.capability, a.value); err != nil {
			log.Error(err, "Failed to run Rule action", "device", a.device, "capability", a.capability)
			return ResultFailed
		}
	}
	if failed {
		return ResultFailed
	}
	return ResultSucceeded
}

// setRoom sets a capability on every discovered device in a room that reports it, in name order.
// Membership is read now, not when the run started. It tries every device and reports whether
// all of them succeeded.
func (e *Engine) setRoom(ctx context.Context, key types.NamespacedName, a action) bool {
	log := e.Log.WithValues("rule", key)
	ok, matched := true, false
	for _, dev := range e.Store.Room(key.Namespace, a.room) {
		snap, found := e.Store.Get(dev)
		if !found || !snap.Discovered || !slices.Contains(snap.Info.Capabilities, a.capability) {
			continue
		}
		if ctx.Err() != nil {
			return ok
		}
		matched = true
		if err := e.Setter.Set(ctx, dev, a.capability, a.value); err != nil {
			log.Error(err, "Failed to run Rule action", "device", dev.Name, "room", a.room, "capability", a.capability)
			ok = false
		}
	}
	if !matched {
		log.V(1).Info("Matched no Devices in Room", "room", a.room, "capability", a.capability)
	}
	return ok
}

func (e *Engine) count(key types.NamespacedName, result string) {
	if e.Runs != nil {
		e.Runs.WithLabelValues(key.Namespace, key.Name, result).Inc()
	}
}
