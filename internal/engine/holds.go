package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/ident"
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
	// reads are the rooms the hold's when has read since it loaded. A look that stops early, or
	// takes another branch, adds nothing, so a room it read once stays.
	reads []RoomRead
	// emptyRoom is the room with no members that the hold's when last stopped on, so it's
	// warned about once each time. Empty means none.
	emptyRoom string
	// stopped is why the hold's last look couldn't tell whether it should be active, as OnStopped
	// last heard it. Empty means it could.
	stopped string
	// held are the members an active room hold has written or claimed, so one that joins while it's
	// active is written then, and one that leaves lets go of its claim. heldKnown is false until the
	// hold starts or first looks in this run: after a restart, the room's members by the registry
	// count as held, discovered yet or not, as the hold wrote them before. heldGen is the engine's
	// membersGen when held was last compared with the room, so it's compared only after a change.
	held      []ident.ID
	heldKnown bool
	heldGen   uint64
	// heldStale asks for a comparison at the next look, as after a member's write failed.
	heldStale bool
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
	}, []string{labelRule})
}

func (c *Compiled) target() HoldTarget {
	h := c.hold
	return HoldTarget{Device: h.device, Room: h.room, DeviceTypes: h.types, Capability: h.capability, Value: h.value}
}

// same reports whether two states are the same, comparing targets as sameTarget does.
func (s HoldState) same(o HoldState) bool {
	return s.Active == o.Active && s.Latched == o.Latched && s.OnAt.Equal(o.OnAt) && s.OffAt.Equal(o.OffAt) &&
		s.Budget == o.Budget && sameTarget(s.Target, o.Target)
}

// sameTarget compares targets as JSON, since a saved value decodes as float64.
func sameTarget(a, b HoldTarget) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// condition reports whether the hold's while is true at now, and when the window next changes.
// known is false when when can't be evaluated, as before a device it reads has reported. devices
// are the home's, or nil to read them.
func (e *Engine) condition(key ident.ID, hs *holdState, h *hold, now time.Time, devices *home) (on, known bool, next time.Time) {
	on = true
	if h.window != nil {
		on, next = h.window.at(now, e.Location, e.Site)
	}
	if !on || h.when == nil {
		e.stopped(key, hs, "")
		return on, true, next
	}
	vars := e.conditionVars(now, devices)
	var reads []RoomRead
	vars[VarRoomReads] = &reads
	b, err := h.when(vars)
	e.addReads(key, hs, reads)
	empty, isEmpty := errors.AsType[EmptyRoom](err)
	switch {
	case !isEmpty:
		hs.emptyRoom = ""
	case hs.emptyRoom != empty.Room:
		hs.emptyRoom = empty.Room
		e.Log.Warn("Rule read a Room with no members", "rule", key, "room", empty.Room)
	}
	if err != nil {
		e.stopped(key, hs, err.Error())
	} else {
		e.stopped(key, hs, "")
	}
	if broken, ok := errors.AsType[brokenCondition](err); ok {
		e.Log.Info("Hold's when failed; keeping its state", "rule", key, "err", broken)
		return false, false, next
	}
	if err != nil {
		e.Log.Debug("Could not evaluate hold; keeping its state", "rule", key, "err", err)
		return false, false, next
	}
	return b, true, next
}

// stopped records why a hold's look couldn't tell whether it should be active, or "" when it could,
// and tells OnStopped when that changed. Call with e.mu held.
func (e *Engine) stopped(key ident.ID, hs *holdState, why string) {
	if hs.stopped == why {
		return
	}
	hs.stopped = why
	if e.OnStopped != nil {
		e.OnStopped(key, why)
	}
}

// addReads adds the rooms a look read to the hold's, and tells OnRoomReads when that grew.
func (e *Engine) addReads(key ident.ID, hs *holdState, reads []RoomRead) {
	grew := false
	for _, r := range reads {
		if !slices.ContainsFunc(hs.reads, r.Equal) {
			hs.reads, grew = append(hs.reads, r), true
		}
	}
	if grew && e.OnRoomReads != nil {
		e.OnRoomReads(key, slices.Clone(hs.reads))
	}
}

// evaluate works out whether a hold is active now, starts the write and save that follow, and
// sets when to look again. devices are the home's, or nil to read them. Call with
// e.mu held.
func (e *Engine) evaluate(ctx context.Context, key ident.ID, s *state, now time.Time, devices *home) {
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
		why := ""
		if r := hs.budget.reported; r != nil && r.Unplanned {
			why = r.Message
		}
		e.stopped(key, hs, why)
		if saved := hs.budget.saved; saved != "" {
			st.Budget = saved
		}
	} else {
		on, known, next = e.condition(key, hs, h, now, devices)
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
		e.Active.WithLabelValues(key.Name).Set(float64(btoi(st.Active)))
	}
	// An active hold claims again when its priority or latch changed, or it has just loaded.
	claim := control.Claim{Source: ruleSource(key.Name), Level: claimLevel(h.priority), Latched: h.latch}
	reclaim := active && st.Active && (hs.claimed == nil || *hs.claimed != claim)
	joined, left := e.joined(hs, st, active, end, now)
	if active != st.Active || !st.same(hs.saved) || hs.dirty || end != nil || reclaim || len(joined) > 0 || len(left) > 0 {
		hs.writing, hs.dirty, hs.claimed = true, false, &claim
		e.holdWrites = append(e.holdWrites, holdWrite{level: claim.Level, key: key, target: st.Target, run: func() {
			e.write(ctx, key, hs, st, active, end, claim, reclaim, joined, left)
		}})
		s.next = fire{}
	} else {
		s.next = fire{At: lookAgain(h, st, next, now)}
		if !hs.retry.IsZero() && hs.retry.Before(s.next.At) {
			s.next.At = hs.retry
		}
	}
	e.reportHold(key, s, st, next, now)
}

// holdWrite is a hold's write, waiting for every hold that looks at the same moment to have looked.
type holdWrite struct {
	level  control.Level
	key    ident.ID
	target HoldTarget
	run    func()
}

// pendingTrigger is a moment a look set off, to start once that look's hold writes are done.
type pendingTrigger struct {
	key  ident.ID
	s    *state
	vars Vars
}

// triggerAfterWrites runs trigger once the hold writes of the current look are in, so a moment
// can't write before a Safety hold set off by the same report has claimed. Call with e.mu held,
// then startWrites.
func (e *Engine) triggerAfterWrites(key ident.ID, s *state, vars Vars) {
	e.triggers = append(e.triggers, pendingTrigger{key: key, s: s, vars: vars})
}

// startWrites runs the hold writes evaluate queued, and then the moments the same look set off.
// Writes that can reach the same device, those in one room or to one device with no room, run one
// after another. Claims go first, highest level first: a Safety claim is in before a Normal one, so
// the arbiter never writes the Normal value. Plain writes, with nothing to arbitrate, go after,
// lowest level first, so the highest level's value is the one that stays. Writes elsewhere run
// alongside, so a slow device holds up only its own room. Call with e.mu held, after every
// evaluate of the look.
func (e *Engine) startWrites(ctx context.Context) {
	writes, triggers := e.holdWrites, e.triggers
	e.holdWrites, e.triggers = nil, nil
	if len(writes) == 0 {
		for _, t := range triggers {
			e.trigger(ctx, t.key, t.s, t.vars)
		}
		return
	}
	type ordered struct {
		holdWrite
		claims bool
	}
	groups := map[string][]ordered{}
	for _, w := range writes {
		group, claims := e.writeGroup(w.target)
		groups[group] = append(groups[group], ordered{w, claims})
	}
	var done sync.WaitGroup
	for _, group := range groups {
		slices.SortStableFunc(group, func(a, b ordered) int {
			switch {
			case a.claims != b.claims:
				if a.claims {
					return -1
				}
				return 1
			case a.level != b.level && a.claims:
				return cmp.Compare(b.level, a.level)
			case a.level != b.level:
				return cmp.Compare(a.level, b.level)
			}
			return cmp.Compare(a.key.Name, b.key.Name)
		})
		done.Add(1)
		e.goBusy(func() {
			defer done.Done()
			for _, w := range group {
				w.run()
			}
		})
	}
	if len(triggers) == 0 {
		return
	}
	e.goBusy(func() {
		done.Wait()
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, t := range triggers {
			// The rule may have been edited or removed meanwhile.
			if e.rules[t.key] == t.s {
				e.trigger(ctx, t.key, t.s, t.vars)
			}
		}
	})
}

// writeGroup returns which writes a hold's may reach the same device as, and whether it claims
// rather than writes: its room, or its device when that has none.
func (e *Engine) writeGroup(t HoldTarget) (group string, claims bool) {
	if t.Room != "" {
		for _, dev := range e.Store.Room(t.Room) {
			claims = claims || e.arbitrated(dev, t.Capability)
		}
		return "room/" + t.Room, claims
	}
	dev := ident.ID{Name: t.Device}
	snap, _ := e.Store.Get(dev)
	if snap.Room != "" {
		return "room/" + snap.Room, snap.Controls(t.Capability)
	}
	return "device/" + t.Device, snap.Controls(t.Capability)
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

// lookAgain returns when a hold next needs evaluating without a device change. It's always after
// now: a delay that ended while when was unknown is still in st, and looking again at once would
// keep Run firing it and never taking a change (OIK-216).
func lookAgain(h *hold, st HoldState, next, now time.Time) time.Time {
	at := now.Add(recheck)
	for _, t := range []time.Time{next, st.OnAt, st.OffAt} {
		if t.After(now) && t.Before(at) {
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
func (e *Engine) write(ctx context.Context, key ident.ID, hs *holdState, st HoldState, active bool, end *HoldTarget,
	claim control.Claim, reclaim bool, joined, left []ident.ID,
) {
	var retry time.Duration
	// started is what a start wrote. added are the members that joined and were written.
	var started, added []ident.ID
	startedOrEnded, joinFailed := false, false
	ended := true
	if end != nil {
		if _, d, err := e.applyHold(ctx, key, *end, false, claim); err != nil {
			retry, ended = d, false
		}
	}
	if ended && active != st.Active {
		written, d, err := e.applyHold(ctx, key, st.Target, active, claim)
		if err != nil {
			retry = d
		} else {
			st.Active, started, startedOrEnded = active, written, true
			st.Since = time.Time{}
			if active {
				st.Since = e.Clock.Now()
			}
		}
	} else if ended {
		if reclaim {
			e.claimHold(ctx, key, st.Target, claim)
		}
		for _, dev := range left {
			// It's no longer the hold's to keep, so a claim on it mustn't outlast the hold.
			if e.arbitrated(dev, st.Target.Capability) {
				e.releaseClaim(ctx, dev, st.Target.Capability, claim.Source)
			}
		}
		var d time.Duration
		added, d = e.holdJoined(ctx, key, st.Target, claim, joined)
		retry, joinFailed = max(retry, d), len(added) < len(joined)
	}
	// When the old target couldn't be ended, nothing is saved, and the next look loads the saved
	// state again and retries.
	if ended {
		if err := e.States.Put(ctx, key, st); err != nil && ctx.Err() == nil {
			e.Log.Error("Failed to save hold state", "err", err, "rule", key)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	hs.writing = false
	if hs.deleted {
		// The Rule was deleted while this ran: its state stays deleted.
		e.goBusy(func() { e.deleteState(key) })
		return
	}
	if ended {
		hs.saved = st
	} else {
		hs.loaded = false
	}
	switch {
	case startedOrEnded:
		hs.held, hs.heldKnown, hs.heldGen = started, st.Active, e.membersGen
	case joinFailed:
		// Compare again once the retry is due.
		hs.held, hs.heldStale = append(hs.held, added...), true
	default:
		hs.held = append(hs.held, added...)
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

func (e *Engine) deleteState(key ident.ID) {
	if err := e.States.Delete(context.Background(), key); err != nil {
		e.Log.Error("Failed to delete hold state", "err", err, "rule", key)
	}
}

// joined are the members of an active room hold that it doesn't hold yet and can write, as a device
// that moved into the room, or was retyped into its types, while it was active. left are those it
// held that aren't members now. A failed write waits for its retry. Call with e.mu held.
func (e *Engine) joined(hs *holdState, st HoldState, active bool, end *HoldTarget, now time.Time) (joined, left []ident.ID) {
	t := st.Target
	if t.Room == "" || !active || !st.Active || end != nil {
		hs.held, hs.heldKnown = nil, false
		return nil, nil
	}
	if hs.heldKnown && !hs.heldStale && hs.heldGen == e.membersGen || !hs.retry.IsZero() && now.Before(hs.retry) {
		return nil, nil
	}
	var members []ident.ID
	for _, dev := range e.Store.Room(t.Room) {
		if snap, ok := e.Store.Get(dev); ok && snap.OfTypes(t.DeviceTypes) {
			members = append(members, dev)
		}
	}
	gen := e.membersGen
	if !hs.heldKnown {
		hs.held, hs.heldKnown, hs.heldGen = members, true, gen
		return nil, nil
	}
	hs.heldGen, hs.heldStale = gen, false
	for _, dev := range hs.held {
		if !slices.Contains(members, dev) {
			left = append(left, dev)
		}
	}
	// One that left counts as joining again if it comes back.
	hs.held = slices.DeleteFunc(hs.held, func(dev ident.ID) bool { return slices.Contains(left, dev) })
	for _, dev := range members {
		// One not discovered yet, or without the capability, joins once it's discovered with it.
		if snap, _ := e.Store.Get(dev); !slices.Contains(hs.held, dev) && snap.Discovered &&
			slices.Contains(snap.Info.Capabilities, t.Capability) {
			joined = append(joined, dev)
		}
	}
	slices.SortFunc(joined, func(a, b ident.ID) int { return cmp.Compare(a.Name, b.Name) })
	return joined, left
}

// holdJoined writes an active room hold's start to the members that joined it, and returns those it
// wrote. One that fails is left out, so it's tried again at the retry.
func (e *Engine) holdJoined(ctx context.Context, key ident.ID, t HoldTarget, claim control.Claim,
	joined []ident.ID,
) ([]ident.ID, time.Duration) {
	var added []ident.ID
	var retry time.Duration
	for _, dev := range joined {
		one := t
		one.Room, one.DeviceTypes, one.Device = "", nil, dev.Name
		if _, d, err := e.applyHold(ctx, key, one, true, claim); err != nil {
			retry = max(retry, d)
			continue
		}
		e.Log.Info("Held Room member that joined", "rule", key, "device", dev.Name, "room", t.Room)
		added = append(added, dev)
	}
	return added, retry
}

// applyHold writes a hold's start (its value) or end (the opposite of a bool, and nothing for a
// number). On an arbitrated capability, the start is claim with the hold's value, and the end
// releases it. It returns the devices it wrote or claimed, and how soon to retry when it fails.
func (e *Engine) applyHold(ctx context.Context, key ident.ID, t HoldTarget, start bool, claim control.Claim) (written []ident.ID, retry time.Duration, failed error) {
	claim.Value = capability.Float(t.Value)
	value := t.Value
	// An unarbitrated number hold leaves the device as it is when it ends (OIK-64, answer 19).
	plainEnd := !start
	if b, ok := value.(bool); ok && !start {
		value = !b
	}
	log := e.Log.With("rule", key, "capability", t.Capability)
	var devices []ident.ID
	if t.Device != "" {
		devices = []ident.ID{{Name: t.Device}}
	}
	if t.Room != "" {
		waiting := false
		var skipped []string
		for _, dev := range e.Store.Room(t.Room) {
			snap, ok := e.Store.Get(dev)
			waiting = waiting || !ok || !snap.Discovered
			if !ok || !snap.Discovered || !slices.Contains(snap.Info.Capabilities, t.Capability) {
				continue
			}
			if !snap.OfTypes(t.DeviceTypes) {
				// A member retyped while the hold was active may still hold its claim, which its end
				// lets go of. It writes nothing to one: the hold never wrote it.
				if !start && snap.Controls(t.Capability) {
					e.releaseClaim(ctx, dev, t.Capability, claim.Source)
				}
				skipped = append(skipped, otherType(dev, snap))
				continue
			}
			devices = append(devices, dev)
		}
		if start {
			e.reportSkipped(key, skipped)
		}
		// With no member to write yet, as on startup, wait for the undiscovered ones. Once some
		// are written, one that never reports doesn't keep rewriting the rest.
		if len(devices) == 0 && waiting {
			log.Debug("Waiting for Room members before writing hold", "room", t.Room)
			return nil, retryUndiscovered, fmt.Errorf("no member of room %s is discovered", t.Room)
		}
	}
	for _, dev := range devices {
		snap, ok := e.Store.Get(dev)
		if ok && snap.Controls(t.Capability) {
			if start && e.claim(ctx, key, dev, t.Capability, claim) || !start && e.releaseClaim(ctx, dev, t.Capability, claim.Source) {
				written = append(written, dev)
				continue
			}
		}
		if _, isBool := t.Value.(bool); plainEnd && !isBool {
			continue
		}
		if !ok || !snap.Discovered {
			log.Debug("Waiting for Device before writing hold", "device", dev.Name)
			retry, failed = max(retry, retryUndiscovered), fmt.Errorf("device %s isn't discovered", dev.Name)
			continue
		}
		if err := e.Setter.Set(ctx, dev, t.Capability, value); err != nil {
			if ctx.Err() == nil {
				log.Error("Failed to write hold", "err", err, "device", dev.Name)
			}
			retry, failed = retryFailed, err
			continue
		}
		written = append(written, dev)
	}
	return written, retry, failed
}

// claimHold makes an active hold's claim again on its arbitrated targets, and writes nothing.
func (e *Engine) claimHold(ctx context.Context, key ident.ID, t HoldTarget, claim control.Claim) {
	claim.Value = capability.Float(t.Value)
	devices := e.Store.Room(t.Room)
	if t.Device != "" {
		devices = []ident.ID{{Name: t.Device}}
	}
	for _, dev := range devices {
		if snap, ok := e.Store.Get(dev); ok && !snap.OfTypes(t.DeviceTypes) {
			// It was retyped out of the hold, so its claim goes rather than staying at the old level.
			if e.arbitrated(dev, t.Capability) {
				e.releaseClaim(ctx, dev, t.Capability, claim.Source)
			}
			continue
		}
		if e.arbitrated(dev, t.Capability) {
			e.claim(ctx, key, dev, t.Capability, claim)
		}
	}
}

// reportHold reports a hold's state and next change: the window's next change, or a delay's
// end, whichever comes first. A latched hold has none. Call with e.mu held.
func (e *Engine) reportHold(key ident.ID, s *state, st HoldState, next, now time.Time) {
	sched := Schedule{Generation: s.c.Generation, Hold: true, Active: st.Active}
	if !st.Latched {
		sched.Next = next
		// A delay that ran out while when was unknown is no change to come.
		for _, t := range []time.Time{st.OnAt, st.OffAt} {
			if t.After(now) && (sched.Next.IsZero() || t.Before(sched.Next)) {
				sched.Next = t
			}
		}
	}
	if r := s.hold.reported; r != nil && *r == sched {
		return
	}
	s.hold.reported = &sched
	e.report(key, sched)
}

// release clears a latched hold's latch. Call without e.mu held.
func (e *Engine) release(key ident.ID) {
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
