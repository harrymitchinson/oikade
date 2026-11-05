package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
)

// Hold timings.
const (
	// whenEvery is how often a hold with when looks again with no device change, since when may
	// read now.
	whenEvery = time.Minute
	// retryUndiscovered is how soon a hold retries a write to a device its driver hasn't reported
	// on yet, as on startup.
	retryUndiscovered = 10 * time.Second
	// retryFailed is how soon a hold retries a write that failed.
	retryFailed = 30 * time.Second
)

// holdState is a loaded hold's state. Use it with e.mu held.
type holdState struct {
	// loaded is false until the saved state has been read.
	loaded bool
	// saved is the state as last saved, apart from changes dirty marks.
	saved HoldState
	// dirty is set when saved has changed without being saved, as after an edit.
	dirty bool
	// released is set by a release, for the next look to clear the latch.
	released bool
	// writing is true while a write or a save is in progress.
	writing bool
	// retry is when to try a failed write again. Zero means none is due.
	retry time.Time
	// reported is the last Schedule reported, so an unchanged one isn't reported again.
	reported *Schedule
	// deleted is set when the Rule is deleted, so a write still running doesn't save its state.
	deleted bool
	// claimed is the claim an active hold last made on arbitrated targets, without its value.
	// Nil means none since the hold loaded.
	claimed *control.Claim
	// budget is a budget hold's plan and progress. Nil until a budget hold is first evaluated.
	budget *budgetState
}

// NewActiveGauge returns the gauge Engine.Active expects.
func NewActiveGauge() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "oikade_rule_active",
		Help: "Whether a Rule's hold is active: 1 or 0.",
	}, []string{labelNamespace, labelRule})
}

func (c *Compiled) target() HoldTarget {
	h := c.hold
	return HoldTarget{Device: h.device, Room: h.room, Capability: h.capability, Value: h.value}
}

// sameTarget compares targets as JSON, since a saved value decodes as float64.
func sameTarget(a, b HoldTarget) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// condition reports whether the hold's while is true at now, and when the window next changes.
// known is false when when can't be evaluated, as before a device it reads has reported. devices
// are the namespace's values, or nil to read them.
func (e *Engine) condition(key types.NamespacedName, h *hold, now time.Time, devices map[string]map[string]any) (on, known bool, next time.Time) {
	on = true
	if h.window != nil {
		on, next = h.window.at(now, e.Location, e.Site)
	}
	if !on || h.when == nil {
		return on, true, next
	}
	if devices == nil {
		devices = e.Store.Values(key.Namespace)
	}
	b, err := h.when(Vars{"devices": devices, "now": now, "tz": e.Location.String()})
	if nb, ok := errors.AsType[notBool](err); ok {
		e.Log.Info("Hold's when didn't evaluate to a bool; keeping its state", "rule", key, "value", nb.value)
		return false, false, next
	}
	if err != nil {
		e.Log.V(1).Info("Could not evaluate hold; keeping its state", "rule", key, "error", err.Error())
		return false, false, next
	}
	return b, true, next
}

// evaluate works out whether a hold is active now, starts the write and save that follow, and
// sets when to look again. devices are the namespace's values, or nil to read them. Call with
// e.mu held.
func (e *Engine) evaluate(ctx context.Context, key types.NamespacedName, s *state, now time.Time, devices map[string]map[string]any) {
	hs, h := s.hold, s.c.hold
	if hs.writing {
		// The write's end signals Run, which looks again.
		s.next = fire{}
		return
	}
	if !hs.loaded {
		hs.loaded = true
		if saved, ok := e.States.Get(key); ok {
			hs.saved = saved
		}
	}
	// An edit that changed what the hold writes ends the old one, then starts afresh.
	var end *HoldTarget
	if t := s.c.target(); !sameTarget(hs.saved.Target, t) {
		if hs.saved.Active {
			old := hs.saved.Target
			end = &old
		}
		hs.saved, hs.dirty = HoldState{Target: t}, true
	}
	st := hs.saved
	if hs.released {
		st.Latched, hs.released = false, false
	}
	var on, known bool
	var next time.Time
	if h.budget != nil {
		on, next = e.budgetCondition(key, s, now, devices)
		known = true
	} else {
		on, known, next = e.condition(key, h, now, devices)
	}
	st, active := h.step(st, on, known, now)
	switch {
	case hs.retry.IsZero():
	case now.Before(hs.retry):
		// A failed write waits for its retry.
		active = st.Active
	default:
		hs.retry = time.Time{}
	}

	if e.Active != nil {
		e.Active.WithLabelValues(key.Namespace, key.Name).Set(float64(btoi(st.Active)))
	}
	// An active hold claims again when its priority or latch changed, or it has just loaded.
	claim := control.Claim{Source: ruleSource(key.Name), Level: claimLevel(h.priority), Latched: h.latch}
	reclaim := active && st.Active && (hs.claimed == nil || *hs.claimed != claim)
	if active != st.Active || st != hs.saved || hs.dirty || end != nil || reclaim {
		hs.writing, hs.dirty, hs.claimed = true, false, &claim
		e.runs.Go(func() { e.write(ctx, key, hs, st, active, end, claim, reclaim) })
		s.next = fire{}
	} else {
		s.next = fire{At: lookAgain(h, st, next, now)}
		if !hs.retry.IsZero() && hs.retry.Before(s.next.At) {
			s.next.At = hs.retry
		}
	}
	e.reportHold(key, s, st, next)
}

// step moves a hold's state on to now, given its condition, and returns whether it should be
// active.
func (h *hold) step(st HoldState, on, known bool, now time.Time) (HoldState, bool) {
	active := st.Active
	switch {
	case st.Latched:
		active = true
	case !known:
	case on:
		st.OffAt = time.Time{}
		if !st.Active {
			if h.onDelay > 0 && st.OnAt.IsZero() {
				st.OnAt = now.Add(h.onDelay)
			}
			if !now.Before(st.OnAt) {
				active, st.OnAt = true, time.Time{}
			}
		}
	default:
		st.OnAt = time.Time{}
		if st.Active {
			if h.offDelay > 0 && st.OffAt.IsZero() {
				st.OffAt = now.Add(h.offDelay)
			}
			if !now.Before(st.OffAt) {
				active, st.OffAt = false, time.Time{}
			}
		}
	}
	// Only a condition known to be true latches, so a release while when can't be evaluated
	// isn't undone: the hold ends once when is known to be false.
	if active && h.latch && known && on {
		st.Latched = true
	}
	return st, active
}

// lookAgain returns when a hold next needs evaluating without a device change.
func lookAgain(h *hold, st HoldState, next, now time.Time) time.Time {
	at := now.Add(recheck)
	for _, t := range []time.Time{next, st.OnAt, st.OffAt} {
		if !t.IsZero() && t.Before(at) {
			at = t
		}
	}
	// A budget looks every minute too, to credit its target's run.
	if every := now.Add(whenEvery); (h.when != nil || h.budget != nil) && !st.Latched && every.Before(at) {
		at = every
	}
	return at
}

// write ends an edited hold's old target, writes the hold's start or end, and saves its state.
// A failed write isn't saved as done, so it's tried again.
func (e *Engine) write(ctx context.Context, key types.NamespacedName, hs *holdState, st HoldState, active bool, end *HoldTarget, claim control.Claim, reclaim bool) {
	var retry time.Duration
	ended := true
	if end != nil {
		if d, err := e.applyHold(ctx, key, *end, false, claim); err != nil {
			retry, ended = d, false
		}
	}
	if ended && active != st.Active {
		if d, err := e.applyHold(ctx, key, st.Target, active, claim); err != nil {
			retry = d
		} else {
			st.Active = active
		}
	} else if ended && reclaim {
		e.claimHold(ctx, key, st.Target, claim)
	}
	// When the old target couldn't be ended, nothing is saved, and the next look loads the saved
	// state again and retries.
	if ended {
		if err := e.States.Put(ctx, key, st); err != nil && ctx.Err() == nil {
			e.Log.Error(err, "Failed to save hold state", "rule", key)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	hs.writing = false
	if hs.deleted {
		// The Rule was deleted while this ran: its state stays deleted.
		e.runs.Go(func() { e.deleteState(key) })
		return
	}
	if ended {
		hs.saved = st
	} else {
		hs.loaded = false
	}
	if retry != 0 {
		hs.retry = e.Clock.Now().Add(retry)
	}
	// The hold may have been reloaded or unloaded meanwhile; a reload keeps hs.
	if s := e.rules[key]; s != nil && s.hold == hs {
		s.next = fire{At: e.Clock.Now()}
		if retry != 0 {
			s.next = fire{At: hs.retry}
		}
		e.signal()
	}
}

func (e *Engine) deleteState(key types.NamespacedName) {
	if err := e.States.Delete(context.Background(), key); err != nil {
		e.Log.Error(err, "Failed to delete hold state", "rule", key)
	}
}

// applyHold writes a hold's start (its value) or end (the opposite of a bool, and nothing for a
// number). On an arbitrated capability, the start is claim with the hold's value, and the end
// releases it. It returns how soon to retry when it fails.
func (e *Engine) applyHold(ctx context.Context, key types.NamespacedName, t HoldTarget, start bool, claim control.Claim) (time.Duration, error) {
	claim.Value = capability.Float(t.Value)
	value := t.Value
	// An unarbitrated number hold leaves the device as it is when it ends (OIK-64, answer 19).
	plainEnd := !start
	if b, ok := value.(bool); ok && !start {
		value = !b
	}
	log := e.Log.WithValues("rule", key, "capability", t.Capability)
	var devices []types.NamespacedName
	if t.Device != "" {
		devices = []types.NamespacedName{{Namespace: key.Namespace, Name: t.Device}}
	}
	var retry time.Duration
	var failed error
	if t.Room != "" {
		waiting := false
		for _, dev := range e.Store.Room(key.Namespace, t.Room) {
			snap, ok := e.Store.Get(dev)
			waiting = waiting || !ok || !snap.Discovered
			if ok && snap.Discovered && slices.Contains(snap.Info.Capabilities, t.Capability) {
				devices = append(devices, dev)
			}
		}
		// With no member to write yet, as on startup, wait for the undiscovered ones. Once some
		// are written, one that never reports doesn't keep rewriting the rest.
		if len(devices) == 0 && waiting {
			log.V(1).Info("Waiting for Room members before writing hold", "room", t.Room)
			return retryUndiscovered, fmt.Errorf("no member of room %s is discovered", t.Room)
		}
	}
	for _, dev := range devices {
		snap, ok := e.Store.Get(dev)
		if ok && snap.Controls(t.Capability) {
			if start && e.claim(ctx, key, dev, t.Capability, claim) || !start && e.releaseClaim(ctx, dev, t.Capability, claim.Source) {
				continue
			}
		}
		if _, isBool := t.Value.(bool); plainEnd && !isBool {
			continue
		}
		if !ok || !snap.Discovered {
			log.V(1).Info("Waiting for Device before writing hold", "device", dev.Name)
			retry, failed = max(retry, retryUndiscovered), fmt.Errorf("device %s isn't discovered", dev.Name)
			continue
		}
		if err := e.Setter.Set(ctx, dev, t.Capability, value); err != nil {
			if ctx.Err() == nil {
				log.Error(err, "Failed to write hold", "device", dev.Name)
			}
			retry, failed = retryFailed, err
		}
	}
	return retry, failed
}

// claimHold makes an active hold's claim again on its arbitrated targets, and writes nothing.
func (e *Engine) claimHold(ctx context.Context, key types.NamespacedName, t HoldTarget, claim control.Claim) {
	claim.Value = capability.Float(t.Value)
	devices := e.Store.Room(key.Namespace, t.Room)
	if t.Device != "" {
		devices = []types.NamespacedName{{Namespace: key.Namespace, Name: t.Device}}
	}
	for _, dev := range devices {
		if e.arbitrated(dev, t.Capability) {
			e.claim(ctx, key, dev, t.Capability, claim)
		}
	}
}

// reportHold reports a hold's state and next change: the window's next change, or a delay's
// end, whichever comes first. A latched hold has none. Call with e.mu held.
func (e *Engine) reportHold(key types.NamespacedName, s *state, st HoldState, next time.Time) {
	sched := Schedule{Generation: s.c.Generation, Hold: true, Active: st.Active}
	if !st.Latched {
		sched.Next = next
		for _, t := range []time.Time{st.OnAt, st.OffAt} {
			if !t.IsZero() && (sched.Next.IsZero() || t.Before(sched.Next)) {
				sched.Next = t
			}
		}
	}
	if s.c.needsSite() && e.Site == nil {
		sched.Reason = ReasonNoLocation
	}
	if r := s.hold.reported; r != nil && *r == sched {
		return
	}
	s.hold.reported = &sched
	e.report(key, sched)
}

// release clears a latched hold's latch. Call without e.mu held.
func (e *Engine) release(key types.NamespacedName) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.rules[key]
	if s == nil || s.hold == nil {
		e.Log.Info("Released no hold; the Rule isn't loaded or has none", "rule", key)
		return
	}
	s.hold.released = true
	s.next = fire{At: e.Clock.Now()}
	e.signal()
}
