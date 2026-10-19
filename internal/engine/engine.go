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
)

// Run results, as reported in oikade_automation_runs_total.
const (
	ResultSucceeded      = "succeeded"
	ResultFailed         = "failed"
	ResultCancelled      = "cancelled"
	ResultBusy           = "busy"
	ResultConditionFalse = "condition_false"
	ResultConditionError = "condition_error"
)

// Engine is a driver.Sink that runs Automations on capability changes.
type Engine struct {
	hub.NopSink
	Store    *hub.Store
	Setter   driver.Setter
	Clock    clock.Clock
	Location *time.Location
	Log      logr.Logger
	// Runs counts trigger outcomes by namespace, automation and result.
	Runs *prometheus.CounterVec
	// Dropped counts changes dropped because the engine fell behind.
	Dropped prometheus.Counter
	// OnStart, if set, is called when an Automation's actions start. It must not block.
	OnStart func(key types.NamespacedName, t time.Time)

	events chan driver.Update

	mu          sync.Mutex
	automations map[types.NamespacedName]*state
	runs        sync.WaitGroup
}

type state struct {
	c *Compiled
	// cancel stops the run in progress; nil when idle.
	cancel context.CancelFunc
	// run identifies the latest run, so a cancelled run doesn't mark a newer one idle.
	run uint64
}

// NewRunsCounter returns the counter Engine.Runs expects.
func NewRunsCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "oikade_automation_runs_total",
		Help: "Automation trigger outcomes.",
	}, []string{"namespace", "automation", "result"})
}

// NewDroppedCounter returns the counter Engine.Dropped expects.
func NewDroppedCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oikade_automation_events_dropped_total",
		Help: "Capability changes the automation engine dropped because it fell behind.",
	})
}

// Init prepares the engine. Call it before Load, Changed or Run.
func (e *Engine) Init() {
	e.events = make(chan driver.Update, 1024)
	e.automations = map[types.NamespacedName]*state{}
	if e.Clock == nil {
		e.Clock = clock.RealClock{}
	}
	if e.Location == nil {
		e.Location = time.Local
	}
}

// Load adds or replaces an Automation. A run in progress for an older generation is cancelled;
// loading the same generation again is a no-op.
func (e *Engine) Load(c *Compiled) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.automations[c.Key]; s != nil {
		if s.c.Generation == c.Generation {
			return
		}
		if s.cancel != nil {
			s.cancel()
		}
	}
	e.automations[c.Key] = &state{c: c}
}

// Remove drops an Automation and cancels its run.
func (e *Engine) Remove(key types.NamespacedName) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.automations[key]; s != nil && s.cancel != nil {
		s.cancel()
	}
	delete(e.automations, key)
	if e.Runs != nil {
		e.Runs.DeletePartialMatch(prometheus.Labels{"namespace": key.Namespace, "automation": key.Name})
	}
}

// Changed implements driver.Sink. It never blocks the driver.
func (e *Engine) Changed(u driver.Update) {
	select {
	case e.events <- u:
	default:
		if e.Dropped != nil {
			e.Dropped.Inc()
		}
		e.Log.Info("Dropped capability change; the automation engine is behind", "device", u.Device, "capability", u.Capability)
	}
}

// Run processes changes until ctx is cancelled, then waits for runs in progress to stop.
func (e *Engine) Run(ctx context.Context) {
	defer e.runs.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-e.events:
			e.handle(ctx, u)
		}
	}
}

func (e *Engine) handle(ctx context.Context, u driver.Update) {
	vars := map[string]any{
		"devices": e.Store.Values(u.Device.Namespace),
		"trigger": map[string]any{
			"device": u.Device.Name, "capability": u.Capability, "value": u.Value, "previous": u.Previous,
		},
		"now": e.Clock.Now(),
		"tz":  e.Location.String(),
	}
	var room string
	if snap, ok := e.Store.Get(u.Device); ok {
		room = snap.Room
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for key, s := range e.automations {
		if key.Namespace != u.Device.Namespace || !s.c.matches(u.Device.Name, room, u.Capability) {
			continue
		}
		ok, err := s.c.eval(vars)
		switch {
		case err != nil:
			e.Log.Error(err, "Failed to evaluate Automation condition", "automation", key)
			e.count(key, ResultConditionError)
			continue
		case !ok:
			e.count(key, ResultConditionFalse)
			continue
		}
		if s.cancel != nil {
			if s.c.Mode == homev1alpha1.AutomationModeSingle {
				e.count(key, ResultBusy)
				continue
			}
			s.cancel()
		}
		e.start(ctx, key, s)
	}
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
		if cur := e.automations[key]; cur == s && s.run == run {
			s.cancel = nil
		}
	})
}

// execute runs the actions in order. A failing device action stops the run; a room action that
// fails on some devices lets the run continue, so a dead bulb can't leave the rest of the room on,
// and the run counts as failed at the end.
func (e *Engine) execute(ctx context.Context, key types.NamespacedName, c *Compiled) string {
	log := e.Log.WithValues("automation", key)
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
			log.Error(err, "Failed to run Automation action", "device", a.device, "capability", a.capability)
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
	log := e.Log.WithValues("automation", key)
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
			log.Error(err, "Failed to run Automation action", "device", dev.Name, "room", a.room, "capability", a.capability)
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
