package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/harrymitchinson/oikade/internal/engine"
)

// Kinds of journal entry.
const (
	kindStart = "start"
	kindNow   = "now"
	kindApply = "apply"
	kindDo    = "do"
	kindSleep = "sleep"
	kindWait  = "wait"
	kindWoke  = "woke"
)

// entry is one step in a run's journal. Kind and ID say which step it was, so a replay notices code
// that no longer takes the same steps.
type entry struct {
	Kind string `json:"k"`
	ID   string `json:"id,omitempty"`
	// At is when the step was recorded, which is what a Now step returns.
	At time.Time `json:"at"`
	// Wake is when a wait ends. Keeping it is what wakes a run at its original time.
	Wake time.Time `json:"wake,omitzero"`
	// Result is what a Do set, and Err the error of a step that returned one.
	Result json.RawMessage `json:"r,omitempty"`
	Err    string          `json:"e,omitempty"`
	// OK is what a wait for a condition returned.
	OK bool `json:"ok,omitempty"`
	// size is its length in the journal.
	size int
}

// Run is one run of a flow, as its code sees it. A flow uses it from one goroutine.
type Run struct {
	f   *Flow
	ctx context.Context
	id  string
	// recorded is the journal found on resuming, and pos how far the replay has reached.
	recorded []entry
	pos      int
	// live is set once the replay has caught up.
	live    bool
	steps   int
	bytes   int
	stopped *stop
}

// halt ends the run from inside a step.
func (r *Run) halt(s stop) {
	r.stopped = &s
	panic(s)
}

// check stops a run that has been stopped, even if the flow recovered from that, or whose context
// has ended.
func (r *Run) check() {
	if r.stopped != nil {
		panic(*r.stopped)
	}
	if r.ctx.Err() != nil {
		r.halt(stop{reason: stopCancelled})
	}
}

// replay returns the next step's journal entry, if it has one. The first step without one is where
// the run went down, so how late it is shows there.
func (r *Run) replay(kind, id string) (entry, bool) {
	r.check()
	if r.pos < len(r.recorded) {
		e := r.recorded[r.pos]
		if e.Kind != kind || e.ID != id {
			r.halt(stop{
				reason: stopEdited,
				detail: fmt.Sprintf("step %d was %s %q and is now %s %q", r.pos, e.Kind, e.ID, kind, id),
			})
		}
		r.pos++
		return e, true
	}
	if !r.live {
		r.live = true
		r.checkLate(r.recorded[len(r.recorded)-1].At)
	}
	return entry{}, false
}

// inTail reports whether e, just replayed, is the last thing the run did, so a wait it recorded
// hasn't ended.
func (r *Run) inTail() bool { return r.pos == len(r.recorded) }

// checkLate gives up on a run that's more than its limit past when it was due. Safety flows run
// however late they are.
func (r *Run) checkLate(due time.Time) {
	if r.f.Safety {
		return
	}
	limit := r.f.Late
	if limit <= 0 {
		limit = DefaultLate
	}
	if late := r.f.Clock.Now().Sub(due); late > limit {
		r.halt(stop{reason: stopLate, late: late})
	}
}

// record writes a step to the journal, and returns once it's on disk.
func (r *Run) record(e entry) entry {
	if e.At.IsZero() {
		e.At = r.f.Clock.Now()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		r.halt(stop{reason: stopLost, detail: err.Error()})
	}
	r.steps++
	r.bytes += len(raw)
	if r.steps > MaxSteps || r.bytes > MaxRunBytes {
		r.halt(stop{reason: stopTooLong})
	}
	if _, err := r.f.Journal.Journal(context.WithoutCancel(r.ctx), r.id, raw); err != nil {
		r.halt(stop{reason: stopLost, detail: err.Error()})
	}
	return e
}

// waiting logs a wait, whose journal entry is on disk by now.
func (r *Run) waiting(until time.Time) {
	r.f.Log.Info("Waiting in flow run", "rule", r.f.Key.Name, "run", r.id, "until", until)
}

// Now is the time, as the run first saw it.
func (r *Run) Now() time.Time {
	if e, ok := r.replay(kindNow, ""); ok {
		return e.At
	}
	return r.record(entry{Kind: kindNow}).At
}

// Sleep waits. A restart doesn't move the time it ends.
func (r *Run) Sleep(d time.Duration) {
	id := d.String()
	e, ok := r.replay(kindSleep, id)
	if ok && !r.inTail() {
		return
	}
	if !ok {
		e = r.record(entry{Kind: kindSleep, ID: id, Wake: r.f.Clock.Now().Add(max(d, 0))})
	}
	r.live = true
	r.waiting(e.Wake)
	if d := e.Wake.Sub(r.f.Clock.Now()); d > 0 {
		select {
		case <-r.ctx.Done():
			r.check()
		case <-r.f.Clock.After(d):
		}
	}
	r.checkLate(e.Wake)
}

// WaitFor waits until cond holds and returns true, or returns false once timeout has passed. Zero
// means no limit, and one already past looks once. A restart doesn't move the time it ends.
func (r *Run) WaitFor(cond engine.Condition, timeout time.Duration) bool {
	id := timeout.String()
	e, ok := r.replay(kindWait, id)
	if !ok {
		wait := entry{Kind: kindWait, ID: id}
		if timeout != 0 {
			wait.Wake = r.f.Clock.Now().Add(max(timeout, 0))
		}
		e = r.record(wait)
	}
	// Waiting is the point here, so time since the wait began isn't lateness.
	if r.inTail() {
		r.live = true
	}
	if woke, ok := r.replay(kindWoke, id); ok {
		return woke.OK
	}
	if !e.Wake.IsZero() {
		r.checkLate(e.Wake)
	}
	r.waiting(e.Wake)
	met, err := r.f.Actions.WaitFor(r.ctx, r.f.Key, cond, e.Wake)
	if err != nil {
		r.check()
		r.halt(stop{reason: stopCancelled, detail: err.Error()})
	}
	if !met {
		r.checkLate(e.Wake)
	}
	r.record(entry{Kind: kindWoke, ID: id, OK: met})
	return met
}

// Apply runs an action: a set, or a release. It runs at least once. A failed action's error is
// recorded too, so a replay takes the same path.
func (r *Run) Apply(a engine.Action) error {
	id := fmt.Sprintf("%T%+v", a, a)
	if e, ok := r.replay(kindApply, id); ok {
		return recordedErr(e)
	}
	err := r.f.Actions.Apply(r.ctx, r.f.Key, a)
	r.stopIfCancelled(err)
	e := entry{Kind: kindApply, ID: id}
	if err != nil {
		e.Err = err.Error()
	}
	r.record(e)
	return err
}

// Do makes an outside call and records what it set in result, a pointer, or nil for no result. Once
// that's recorded the call never happens again: a replay fills result from the journal, and
// returns an error with the recorded message. It isn't the same error, so errors.Is won't match it.
// A crash before that repeats the call.
func (r *Run) Do(name string, result any, call func(context.Context) error) error {
	if e, ok := r.replay(kindDo, name); ok {
		if result != nil && e.Err == "" {
			if len(e.Result) == 0 {
				return errNoResult
			}
			if err := json.Unmarshal(e.Result, result); err != nil {
				return fmt.Errorf("reading the recorded result of %s: %w", name, err)
			}
		}
		return recordedErr(e)
	}
	err := call(r.ctx)
	r.stopIfCancelled(err)
	e := entry{Kind: kindDo, ID: name}
	if err == nil && result != nil {
		raw, merr := json.Marshal(result)
		switch {
		case merr != nil:
			err = fmt.Errorf("recording the result of %s: %w", name, merr)
		case len(raw) > MaxResult:
			err = fmt.Errorf("the result of %s is %d bytes, over the limit of %d", name, len(raw), MaxResult)
		default:
			e.Result = raw
		}
	}
	if err != nil {
		e.Err = err.Error()
	}
	r.record(e)
	return err
}

// stopIfCancelled stops the run if a step failed with its context ended. The failure is probably
// the context's, whatever error it came as, and at least once means the step can run again.
func (r *Run) stopIfCancelled(err error) {
	if err != nil && r.ctx.Err() != nil {
		r.check()
	}
}

func recordedErr(e entry) error {
	if e.Err == "" {
		return nil
	}
	return errors.New(e.Err)
}
