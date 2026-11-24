package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"oikade.dev/internal/ident"
)

// SignalStatesNamespace keeps a SignalState per Signal, at its name.
const SignalStatesNamespace = "signals"

// VarSignals is the Vars key of what a condition reads a signal by: a func(name string) (bool,
// error), whose error is a mistake, such as a signal the engine doesn't have.
const VarSignals = "signals"

// Signal is a Bool the engine works out from When, under a name of its own. It's worked out before
// any hold or flow looks, so they read its value now, and its change triggers flows. OnDelay and
// OffDelay are a hold's. A When that can't be evaluated counts as false.
type Signal struct {
	Name              string
	When              Condition
	OnDelay, OffDelay time.Duration
}

// SignalState is what a signal needs across a restart.
type SignalState struct {
	Value bool `json:"value"`
	// Since is when Value last changed, or when the signal started.
	Since time.Time `json:"since,omitzero"`
	// OnAt is when an OnDelay ends, and OffAt when an OffDelay ends. Zero means none is running.
	OnAt  time.Time `json:"onAt,omitzero"`
	OffAt time.Time `json:"offAt,omitzero"`
}

// SignalStates keeps each Signal's SignalState, at its name.
type SignalStates = States[SignalState]

// signalState is a signal as the engine runs it. Use it with e.mu held.
type signalState struct {
	Signal
	// timing is the signal's delays, as a hold's step takes them.
	timing *hold
	// saved is the state as last worked out, and fresh says there was none to load.
	saved SignalState
	fresh bool
	// stopped is why the signal's When couldn't be evaluated at its last look, so each stretch is
	// logged once. Empty means it could.
	stopped string
}

// signals are the engine's signals, in the order they're worked out. Use them with e.mu held.
type signals struct {
	list []*signalState
	// values are each signal's value now, by name.
	values map[string]bool
	// next is when they're next looked at with no device change. Zero means at once.
	next time.Time
	// ctx is Run's, which saves outlive, and saving the states waiting to be saved, latest only.
	ctx    context.Context
	saving map[string]*SignalState
}

// initSignals loads the signals' saved states, so a flow resumed before the first look reads them.
func (e *Engine) initSignals() {
	e.signals.values = map[string]bool{}
	for _, sig := range e.Signals {
		s := &signalState{Signal: sig, timing: &hold{onDelay: sig.OnDelay, offDelay: sig.OffDelay}}
		s.When = guarded(sig.When)
		s.saved, s.fresh = e.SignalStates.Get(ident.ID{Name: sig.Name})
		s.fresh = !s.fresh
		e.signals.list = append(e.signals.list, s)
		e.signals.values[sig.Name] = s.saved.Value
	}
}

// signalReader is what conditions read signals by, from values.
func signalReader(values map[string]bool) func(string) (bool, error) {
	return func(name string) (bool, error) {
		v, ok := values[name]
		if !ok {
			return false, fmt.Errorf("no signal %s", name)
		}
		return v, nil
	}
}

// lookAtSignals works out every signal at now, in order, into devices, and returns the names of
// those whose value changed. A signal reads only those before it, which this look has worked out
// already. devices are the home's, or nil to read them. Call with e.mu held.
func (e *Engine) lookAtSignals(now time.Time, devices *home) (changed []string) {
	if len(e.signals.list) == 0 {
		return nil
	}
	if devices == nil {
		devices = e.home()
	}
	done := make(map[string]bool, len(e.signals.list))
	read := func(name string) (bool, error) {
		if v, ok := done[name]; ok {
			return v, nil
		}
		if _, ok := e.signals.values[name]; ok {
			return false, fmt.Errorf("%s is read before it's worked out: a signal reads only the signals made before it",
				signalRef(name))
		}
		return false, fmt.Errorf("no signal %s", name)
	}
	// On the minute, so a signal of a time of day changes on its edge.
	next := now.Truncate(whenEvery).Add(whenEvery)
	for _, s := range e.signals.list {
		vars := e.conditionVars(now, devices)
		vars[VarSignals] = read
		on, err := s.When(vars)
		e.signalStopped(s, err)
		on = on && err == nil
		st := e.stepSignal(s, on, now)
		if st.Value != s.saved.Value && !s.fresh {
			changed = append(changed, s.Name)
			e.Log.Info("Changed Signal", "signal", signalRef(s.Name), "value", st.Value)
			if e.OnSignal != nil {
				e.OnSignal(s.Name, st)
			}
		}
		if st != s.saved || s.fresh {
			s.saved, s.fresh = st, false
			e.saveSignal(s.Name, st)
		}
		done[s.Name] = st.Value
		next = soonest(next, futureOf(now, st.OnAt), futureOf(now, st.OffAt))
	}
	e.signals.values = done
	e.signals.next = next
	devices.signals = maps.Clone(done)
	return changed
}

// stepSignal moves a signal's state on to now, given whether its When is true. A signal with no
// saved state starts at what its When says, which is no change. Call with e.mu held.
func (e *Engine) stepSignal(s *signalState, on bool, now time.Time) SignalState {
	if s.fresh {
		return SignalState{Value: on, Since: now}
	}
	st, value := s.timing.step(HoldState{Active: s.saved.Value, OnAt: s.saved.OnAt, OffAt: s.saved.OffAt}, on, true, now)
	out := SignalState{Value: value, Since: s.saved.Since, OnAt: st.OnAt, OffAt: st.OffAt}
	if value != s.saved.Value {
		out.Since = now
	}
	return out
}

// futureOf is t if it's after now, and zero otherwise, so a delay that ended isn't looked at again.
func futureOf(now, t time.Time) time.Time {
	if t.After(now) {
		return t
	}
	return time.Time{}
}

// signalStopped logs the start and end of a stretch in which a signal's When can't be evaluated,
// once each. Call with e.mu held.
func (e *Engine) signalStopped(s *signalState, err error) {
	why := ""
	if err != nil {
		why = err.Error()
	}
	if why == s.stopped {
		return
	}
	log := e.Log.With("signal", signalRef(s.Name))
	_, broken := errors.AsType[brokenCondition](err)
	switch {
	case broken:
		log.Error("Failed to evaluate Signal; counting its conditions as false", "err", err)
	case err != nil:
		log.Info("Could not evaluate Signal; counting its conditions as false until it can", "err", err)
	case s.stopped != "":
		log.Info("Evaluated Signal again")
	}
	s.stopped = why
}

// signalsDue says whether the signals are due a look now. Call with e.mu held.
func (e *Engine) signalsDue(now time.Time) bool {
	return len(e.signals.list) > 0 && !now.Before(e.signals.next)
}

// signalsChanged sets off what a change of signals does with no device change, as at a delay's
// end: the flows they trigger, the holds that may read them, and the flows waiting on a condition.
// Call with e.mu held.
func (e *Engine) signalsChanged(changed []string, now time.Time) {
	for _, s := range e.rules {
		if s.hold != nil && s.hold.loaded && s.c.hold.when != nil {
			s.next = fire{At: now}
		}
	}
	e.triggerSignals(changed)
	close(e.changes)
	e.changes = make(chan struct{})
}

// triggerSignals queues the flows that the signals' changes trigger, to start after the look's hold
// writes. Call with e.mu held.
func (e *Engine) triggerSignals(changed []string) {
	for _, name := range changed {
		value := e.signals.values[name]
		var vars Vars
		for key, s := range e.rules {
			if s.hold != nil || !s.c.matchesSignal(name, value) {
				continue
			}
			if vars == nil {
				vars = e.vars(map[string]any{triggerType: TypeChange, "signal": name, "value": value, "previous": !value})
			}
			e.triggerAfterWrites(key, s, vars)
		}
	}
}

// saveSignal saves a signal's state apart from the engine, which can't wait for the host. Only the
// latest waiting state is saved, so an older one never lands after a newer. Call with e.mu held.
func (e *Engine) saveSignal(name string, st SignalState) {
	sg := &e.signals
	if sg.saving == nil {
		sg.saving = map[string]*SignalState{}
	}
	_, busy := sg.saving[name]
	sg.saving[name] = &st
	if busy {
		return
	}
	ctx := sg.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	e.goBusy(func() {
		for {
			e.mu.Lock()
			next := sg.saving[name]
			if next == nil {
				delete(sg.saving, name)
				e.mu.Unlock()
				return
			}
			sg.saving[name] = nil
			e.mu.Unlock()
			if err := e.SignalStates.Put(ctx, ident.ID{Name: name}, *next); err != nil && ctx.Err() == nil {
				e.Log.Error("Failed to save Signal", "err", err, "signal", signalRef(name))
			}
		}
	})
}

// signalRef names a signal in a log line.
func signalRef(name string) string { return ident.NewRef(ident.Signal, name).String() }
