package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/fault"
	"oikade.dev/internal/ident"
)

const (
	// whenEvery is how often a hold with when looks again with no device change, since when may
	// read now.
	whenEvery = time.Minute
	// retryUndiscovered is how soon a hold retries a write to a device its driver hasn't reported
	// on yet, as on startup.
	retryUndiscovered = 10 * time.Second
	// retryFailed is how soon a failed write or save is tried again.
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
	// followWhy is why a followed hold's value isn't known, or "".
	followWhy string
	// released is set by a release, for the next look to clear the latch.
	released bool
	// writing is true while a write or a save is in progress.
	writing bool
	// retry is when to try a failed write again. Zero means none is due.
	retry time.Time
	// joinRetry is when to try again to write the members of an active group or room hold that its
	// start or a join couldn't. Unlike retry, it doesn't hold up the hold's end. Zero means none.
	joinRetry time.Time
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
	// whyNot is why the hold is inactive, as OnWhyNot last heard it.
	whyNot string
	// unknown is set while the hold's when can't be evaluated, so the start and end of that stretch
	// are each logged once at Info.
	unknown bool
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

func (c *Compiled) target() HoldTarget {
	h := c.hold
	return HoldTarget{
		Device: h.device, Devices: h.devices, Room: h.room, DeviceTypes: h.types, Except: h.except,
		Capability: h.capability, Value: h.value,
	}
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

// condition reports whether the hold's window is open and its when true at now, and when the window
// next changes. known is false when when can't be evaluated, as before a device it reads has
// reported. devices are the home's, or nil to read them.
func (e *Engine) condition(key ident.ID, hs *holdState, h *hold, now time.Time, devices *home) (on, known bool, next time.Time) {
	on = true
	if h.window != nil {
		on, next = h.window.at(now, e.Location, e.suns)
	}
	if !on || h.when == nil {
		e.stopped(key, hs, "")
		e.whenKnown(key, hs)
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
		e.Log.Warn("Rule read a Room with no members", "rule", e.ruleRef(key), "room", empty.Room)
	}
	if err != nil {
		e.stopped(key, hs, err.Error())
	} else {
		e.stopped(key, hs, "")
	}
	if broken, ok := errors.AsType[brokenCondition](err); ok {
		e.Log.Info("Hold's when failed; keeping its state", "rule", e.ruleRef(key), "err", broken)
		hs.unknown = true
		return false, false, next
	}
	if err != nil {
		if hs.unknown {
			e.Log.Debug("Could not evaluate hold; keeping its state", "rule", e.ruleRef(key), "err", err)
		} else {
			e.Log.Info("Could not evaluate hold; keeping its state until it can", "rule", e.ruleRef(key), "err", err)
			hs.unknown = true
		}
		return false, false, next
	}
	e.whenKnown(key, hs)
	return b, true, next
}

// untilHolds says whether a latched hold's until is true, which ends the latch. One that can't be
// told yet keeps it. Call with e.mu held.
func (e *Engine) untilHolds(h *hold, now time.Time, devices *home) bool {
	if h.until == nil {
		return false
	}
	ok, err := h.until(e.conditionVars(now, devices))
	return err == nil && ok
}

// edited starts a hold's state afresh if an edit changed what it writes, and returns the old target
// for the write to end, if it was active. A followed value is the hold's to change, not an edit.
func edited(hs *holdState, c *Compiled) *HoldTarget {
	t := c.target()
	if c.hold.follow != nil {
		t.Value = hs.saved.Target.Value
	}
	if sameTarget(hs.saved.Target, t) {
		return nil
	}
	var end *HoldTarget
	if hs.saved.Active {
		old := hs.saved.Target
		end = &old
	}
	hs.saved, hs.dirty = HoldState{Target: t}, true
	return end
}

// budgetLook says whether a budget hold's plan, and its when if it has one, want it active now, and
// when to look again. Call with e.mu held.
func (e *Engine) budgetLook(key ident.ID, s *state, st *HoldState, now time.Time, devices *home) (on, known bool, next time.Time) {
	hs, h := s.hold, s.c.hold
	// A budget's when is looked at first, so a plan it couldn't make is what's said to stop it. A
	// when that can't be told is false: the budget's slots, not its when, say when it stops.
	when := true
	var whenNext time.Time
	if h.when != nil {
		var whenKnown bool
		when, whenKnown, whenNext = e.condition(key, hs, h, now, devices)
		when = when && whenKnown
	}
	on, next = e.budgetCondition(key, s, now, devices, when)
	on, known = on && when, true
	if !whenNext.IsZero() && (next.IsZero() || whenNext.Before(next)) {
		next = whenNext
	}
	switch r := hs.budget.reported; {
	case r != nil && r.Unplanned:
		e.stopped(key, hs, r.Message)
	case h.when == nil:
		e.stopped(key, hs, "")
	}
	if h.when == nil {
		// A Rule edited into a budget hold with no When, so a stretch it was in has ended.
		e.whenKnown(key, hs)
	}
	if saved := hs.budget.saved; saved != "" {
		st.Budget = saved
	}
	return on, known, next
}

// follow works out a followed hold's value, into st's target while it's known, and says whether it
// is. Why it isn't is the hold's why-not. Call with e.mu held.
func (e *Engine) follow(hs *holdState, h *hold, st *HoldState, now time.Time, devices *home) bool {
	v, err := h.follow(e.conditionVars(now, devices))
	if err == nil && (math.IsNaN(v) || math.IsInf(v, 0)) {
		err = fmt.Errorf("follow gave %v", v)
	}
	if err != nil {
		hs.followWhy = err.Error()
		return false
	}
	hs.followWhy = ""
	st.Target.Value = h.followed(v, st.Target.Value)
	return true
}

// whyNot works out why an inactive hold isn't active, and tells OnWhyNot when that changed. Call
// with e.mu held.
func (e *Engine) whyNot(key ident.ID, hs *holdState, h *hold, inactive bool, now time.Time, devices *home) {
	why := ""
	switch {
	case !inactive:
	case h.follow != nil:
		why = hs.followWhy
	case h.whyNot != nil:
		why = h.whyNot(e.conditionVars(now, devices))
	}
	if hs.whyNot != why {
		hs.whyNot = why
		if e.OnWhyNot != nil {
			e.OnWhyNot(key, why)
		}
	}
}

// whenKnown records that the hold's look could tell whether it should be active, and logs the end
// of a stretch when it couldn't. Call with e.mu held.
func (e *Engine) whenKnown(key ident.ID, hs *holdState) {
	if hs.unknown {
		hs.unknown = false
		e.Log.Info("Evaluated hold again", "rule", e.ruleRef(key))
	}
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
// sets when to look again. devices are the home's, or nil to read them. Call with e.mu held.
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
	end := edited(hs, s.c)
	st := hs.saved
	if hs.released {
		st.Latched, hs.released = false, false
	}
	st.Latched = st.Latched && !e.untilHolds(h, now, devices)
	var on, known bool
	var next time.Time
	if h.budget != nil {
		on, known, next = e.budgetLook(key, s, &st, now, devices)
	} else if h.follow != nil {
		on, known = e.follow(hs, h, &st, now, devices), true
	} else {
		on, known, next = e.condition(key, hs, h, now, devices)
	}
	clock := offClock{loc: e.Location, suns: e.suns}
	if h.offUntil != nil && h.window != nil {
		clock.closed = h.window.closed(now, e.Location, e.suns)
	}
	st, active := h.step(st, on, known, now, clock)
	e.whyNot(key, hs, h, known && !on && !active, now, devices)
	// A followed value that changed while the hold stays active is written again.
	rewrite := h.follow != nil && active && st.Active && !sameTarget(st.Target, hs.saved.Target)
	switch {
	case hs.retry.IsZero():
	case now.Before(hs.retry):
		// A failed write waits for its retry.
		active = st.Active
	default:
		hs.retry = time.Time{}
	}

	e.metrics.active.Set(float64(btoi(st.Active)), e.ruleRef(key))
	// An active hold claims again when its priority or latch changed, or it has just loaded.
	claim := control.Claim{Source: ruleSource(key.Name), Level: claimLevel(h.priority), Latched: h.latch}
	reclaim := active && st.Active && (hs.claimed == nil || *hs.claimed != claim)
	// Before joined, which forgets them as the hold ends.
	only := endsOnly(hs, st, active)
	joined, left := e.joined(hs, st, active, end, now)
	if active != st.Active || !st.same(hs.saved) || hs.dirty || end != nil || reclaim || len(joined) > 0 || len(left) > 0 {
		hs.writing, hs.dirty, hs.claimed = true, false, &claim
		ends := st.Active && !active
		e.holdWrites = append(e.holdWrites, holdWrite{level: claim.Level, key: key, target: st.Target, ends: ends, run: func() {
			e.write(ctx, key, hs, st, active, end, claim, reclaim || rewrite, rewrite, joined, left, only)
		}})
		s.next = fire{}
	} else {
		s.next = fire{At: lookAgain(h, st, next, now)}
		s.next.At = soonest(s.next.At, hs.retry, hs.joinRetry)
	}
	e.reportHold(key, s, st, next, now)
}

// retryIn sets when a failed write, and a member still to write, are tried again, if they are.
func (hs *holdState) retryIn(now time.Time, retry, joinRetry time.Duration) {
	if retry != 0 {
		hs.retry = now.Add(retry)
	}
	if joinRetry != 0 {
		hs.joinRetry = now.Add(joinRetry)
	}
}

// endsOnly are the names of the members a group hold that's ending holds, which its end writes
// back: one its start never reached was never written. Nil means every member. Call with e.mu held.
func endsOnly(hs *holdState, st HoldState, active bool) []string {
	if !st.Active || active || len(st.Target.Devices) == 0 || !hs.heldKnown {
		return nil
	}
	only := make([]string, 0, len(hs.held))
	for _, dev := range hs.held {
		only = append(only, dev.Name)
	}
	return only
}

// onlyMembers is a group's target limited to only, or t if only is nil.
func onlyMembers(t HoldTarget, only []string) HoldTarget {
	if only != nil {
		t.Devices = slices.DeleteFunc(slices.Clone(t.Devices), func(d string) bool { return !slices.Contains(only, d) })
	}
	return t
}

// soonest is the earliest of next and the times that are set. A zero next means none.
func soonest(next time.Time, times ...time.Time) time.Time {
	for _, at := range times {
		if !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	return next
}

// holdWrite is a hold's write, waiting for every hold that looks at the same moment to have looked.
type holdWrite struct {
	level  control.Level
	key    ident.ID
	target HoldTarget
	ends   bool
	run    func()
}

// pendingTrigger is a flow a look set off, to start once that look's hold writes are done.
type pendingTrigger struct {
	key  ident.ID
	s    *state
	vars Vars
}

// triggerAfterWrites runs trigger once the hold writes of the current look are in, so a flow
// can't write before a Safety hold set off by the same report has claimed. Call with e.mu held,
// then startWrites.
func (e *Engine) triggerAfterWrites(key ident.ID, s *state, vars Vars) {
	e.triggers = append(e.triggers, pendingTrigger{key: key, s: s, vars: vars})
}

// startWrites runs the hold writes evaluate queued, and then the flows the same look set off.
// Writes that can reach the same device, those in one room or to one device with no room, run one
// after another. Claims go first: those that start before those that end, so a hold taking over
// from another at the same moment has claimed before the other lets go and the arbiter never falls
// back to the default, and then highest level first: a Safety claim is in before a Normal one, so
// the arbiter never writes the Normal value. Plain writes, with nothing to arbitrate, go after:
// ends first, since a bool's end writes the opposite of its value, then lowest level first, so a
// start's value, and the highest level's, is the one that stays. Writes elsewhere run alongside, so
// a slow device holds up only its own room. Call with e.mu held, after every evaluate of the look.
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
	each := make([]ordered, len(writes))
	reach := make([][]string, len(writes))
	for i, w := range writes {
		gs, claims := e.writeGroups(w.target)
		each[i], reach[i] = ordered{w, claims}, gs
		if len(gs) == 0 {
			reach[i] = []string{"rule/" + w.key.Name}
		}
	}
	groups := map[string][]ordered{}
	for i, g := range joinGroups(reach) {
		groups[g] = append(groups[g], each[i])
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
			case a.ends != b.ends && a.claims:
				// A claim ends after the starts, so a hold that takes over from it at the same
				// moment has claimed before it lets go.
				if a.ends {
					return 1
				}
				return -1
			case a.ends != b.ends:
				// A plain end writes the opposite of its value, so it goes before the starts.
				if a.ends {
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

// named are the devices a hold names: its device, or its group's members.
func named(t HoldTarget) []ident.ID {
	if t.Device != "" {
		return []ident.ID{{Name: t.Device}}
	}
	out := make([]ident.ID, 0, len(t.Devices))
	for _, d := range t.Devices {
		out = append(out, ident.ID{Name: d})
	}
	return out
}

// roomMembers are the devices in a room hold's room, apart from those it leaves out.
func (e *Engine) roomMembers(t HoldTarget) []ident.ID {
	return slices.DeleteFunc(e.Store.Room(t.Room), func(d ident.ID) bool { return slices.Contains(t.Except, d.Name) })
}

// joinGroups says which group each write runs in, given the write groups each reaches: a write
// that reaches several, as a group's members in several rooms do, joins them into one.
func joinGroups(reach [][]string) []string {
	parent := map[string]string{}
	var find func(string) string
	find = func(g string) string {
		p, ok := parent[g]
		if !ok || p == g {
			return g
		}
		root := find(p)
		parent[g] = root
		return root
	}
	for _, gs := range reach {
		for _, g := range gs[1:] {
			if a, b := find(gs[0]), find(g); a != b {
				parent[b] = a
			}
		}
	}
	out := make([]string, len(reach))
	for i, gs := range reach {
		out[i] = find(gs[0])
	}
	return out
}

// writeGroups returns which writes a hold's may reach the same device as, and whether it claims
// rather than writes: its room, or its device when that has none, or each of a group's members'.
func (e *Engine) writeGroups(t HoldTarget) (groups []string, claims bool) {
	if t.Room != "" {
		for _, dev := range e.roomMembers(t) {
			claims = claims || e.arbitrated(dev, t.Capability)
		}
		return []string{"room/" + t.Room}, claims
	}
	for _, dev := range named(t) {
		snap, _ := e.Store.Get(dev)
		claims = claims || snap.Controls(t.Capability)
		if snap.Room != "" {
			groups = append(groups, "room/"+snap.Room)
		} else {
			groups = append(groups, "device/"+dev.Name)
		}
	}
	return groups, claims
}

// step moves a hold's state on to now, given its condition, and returns whether it should be
// active. clock works out an offUntil.
func (h *hold) step(st HoldState, on, known bool, now time.Time, clock offClock) (HoldState, bool) {
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
			if h.lingers() && st.OffAt.IsZero() {
				st.OffAt = h.offAt(now, clock)
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
// keep Run firing it and never taking a change.
func lookAgain(h *hold, st HoldState, next, now time.Time) time.Time {
	at := now.Add(recheck)
	for _, t := range []time.Time{next, st.OnAt, st.OffAt} {
		if t.After(now) && t.Before(at) {
			at = t
		}
	}
	// A budget looks every minute too, to credit its target's run.
	if every := now.Add(whenEvery); (h.when != nil || h.budget != nil || h.follow != nil) && (!st.Latched || h.until != nil) && every.Before(at) {
		at = every
	}
	return at
}

// write ends an edited hold's old target, writes the hold's start or end, and saves its state.
// A failed write isn't saved as done, so it's tried again.
//
// rewrite writes an active hold's target again, as when the value it follows has changed.
func (e *Engine) write(ctx context.Context, key ident.ID, hs *holdState, st HoldState, active bool, end *HoldTarget,
	claim control.Claim, reclaim, rewrite bool, joined, left []ident.ID, only []string,
) {
	// retry holds up the hold's next change, and joinRetry only the members still to write.
	var retry, joinRetry time.Duration
	// started is what a start wrote. added are the members that joined and were written.
	var started, added []ident.ID
	// partial is a group's or a room's start that reached some members and failed on others.
	startedOrEnded, joinFailed, partial := false, false, false
	ended := true
	if end != nil {
		if _, d, err := e.applyHold(ctx, key, *end, false, claim); err != nil {
			retry, ended = d, false
		}
	}
	// saved is set once st is on disk as it ends up.
	saved := false
	if ended && active != st.Active {
		var since time.Time
		if active {
			since = e.Clock.Now()
			saved = e.saveStart(ctx, key, st, since)
		}
		written, d, err := e.applyHold(ctx, key, onlyMembers(st.Target, only), active, claim)
		if err != nil && active && len(written) > 0 && isGroup(st.Target) {
			// A write to one member failed. What it reached is held: inactive, nothing would end those
			// claims. The rest join it at the retry, as members that join a room hold do.
			joinRetry, saved, partial = d, false, true
			err = nil
		}
		if err != nil {
			retry, saved = d, false
		} else {
			st.Active, st.Since, started, startedOrEnded = active, since, written, true
			if active {
				fault.Hit(fault.ClientHoldStarted)
			}
		}
	} else if ended {
		switch {
		case rewrite:
			if _, d, err := e.applyHold(ctx, key, st.Target, true, claim); err != nil {
				// The value isn't saved as written, so the next look writes it again.
				retry, st.Target = d, hs.saved.Target
			}
		case reclaim:
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
		joinRetry, joinFailed = d, len(added) < len(joined)
	}
	// When the old target couldn't be ended, nothing is saved, and the next look loads the saved
	// state again and retries.
	if ended && !saved {
		if err := e.States.Put(ctx, key, st); err != nil && ctx.Err() == nil {
			e.Log.Error("Failed to save hold state", "err", err, "rule", e.ruleRef(key))
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
		hs.held, hs.heldKnown, hs.heldGen, hs.heldStale = started, st.Active, e.membersGen, partial
	default:
		// After a failed join, compare again once the retry is due.
		hs.held, hs.heldStale = append(hs.held, added...), hs.heldStale || joinFailed
	}
	hs.retryIn(e.Clock.Now(), retry, joinRetry)
	// The hold may have been reloaded or unloaded meanwhile; a reload keeps hs.
	if s := e.rules[key]; s != nil && s.hold == hs {
		s.next = fire{At: e.Clock.Now()}
		if retry != 0 {
			s.next = fire{At: hs.retry}
		}
		e.signal()
	}
}

// saveStart saves a hold as active before its start claims, and reports whether that's on disk. A
// claim outlives your process, in the host, so a process killed between the two loads the hold
// active and claims again, latch and all, rather than inactive with nothing to end the claim. A
// plain write ends with your process, so a hold that writes any device is saved after: the next
// process writes the start again if it's still due, where one loaded active would only claim again.
// A failed save is logged and the start goes ahead, so a Safety hold still acts on a failing disk,
// though the claim waits for the save's answer.
func (e *Engine) saveStart(ctx context.Context, key ident.ID, st HoldState, since time.Time) bool {
	if !e.onlyClaims(st.Target) {
		return false
	}
	st.Active, st.Since = true, since
	if err := e.States.Put(ctx, key, st); err != nil {
		if ctx.Err() == nil {
			e.Log.Error("Failed to save hold state", "err", err, "rule", e.ruleRef(key))
		}
		return false
	}
	return true
}

// onlyClaims reports whether a hold's start claims on every device it reaches, and writes none.
func (e *Engine) onlyClaims(t HoldTarget) bool {
	devices := named(t)
	if t.Room != "" {
		devices = e.roomMembers(t)
	}
	return len(devices) > 0 && !slices.ContainsFunc(devices, func(dev ident.ID) bool { return !e.arbitrated(dev, t.Capability) })
}

func (e *Engine) deleteState(key ident.ID) {
	if err := e.States.Delete(context.Background(), key); err != nil {
		e.Log.Error("Failed to delete hold state", "err", err, "rule", e.ruleRef(key))
	}
}

// isGroup reports whether a hold's target is a group's or a room's members, rather than one device.
func isGroup(t HoldTarget) bool { return t.Room != "" || len(t.Devices) > 0 }

// joined are the members of an active group or room hold that it doesn't hold yet and can write, as
// a device that moved into the room, or was retyped into its types, while it was active, or one its
// start couldn't write. left are those it held that aren't members now. A failed write waits for
// its retry. Call with e.mu held.
func (e *Engine) joined(hs *holdState, st HoldState, active bool, end *HoldTarget, now time.Time) (joined, left []ident.ID) {
	t := st.Target
	if !isGroup(t) || !active || !st.Active || end != nil {
		hs.held, hs.heldKnown, hs.joinRetry = nil, false, time.Time{}
		return nil, nil
	}
	if now.Before(hs.joinRetry) {
		return nil, nil
	}
	hs.joinRetry = time.Time{}
	if hs.heldKnown && !hs.heldStale && hs.heldGen == e.membersGen || !hs.retry.IsZero() && now.Before(hs.retry) {
		return nil, nil
	}
	var members []ident.ID
	candidates := named(t)
	if t.Room != "" {
		candidates = e.roomMembers(t)
	}
	for _, dev := range candidates {
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
		// One not discovered yet, or without the capability, joins once it's discovered with it. A
		// group's member that isn't on record yet is written once it is.
		if snap, _ := e.Store.Get(dev); !slices.Contains(hs.held, dev) && snap.Discovered &&
			slices.Contains(snap.Info.Capabilities, t.Capability) {
			joined = append(joined, dev)
		}
	}
	slices.SortFunc(joined, func(a, b ident.ID) int { return cmp.Compare(a.Name, b.Name) })
	return joined, left
}

// holdJoined writes an active group or room hold's start to the members that joined it, and
// returns those it wrote. One that fails is left out, so it's tried again at the retry.
func (e *Engine) holdJoined(ctx context.Context, key ident.ID, t HoldTarget, claim control.Claim,
	joined []ident.ID,
) ([]ident.ID, time.Duration) {
	var added []ident.ID
	var retry time.Duration
	for _, dev := range joined {
		one := t
		one.Room, one.DeviceTypes, one.Devices, one.Device = "", nil, nil, dev.Name
		if _, d, err := e.applyHold(ctx, key, one, true, claim); err != nil {
			retry = max(retry, d)
			continue
		}
		if t.Room != "" {
			e.Log.Info("Held Room member that joined", "rule", e.ruleRef(key), "device", ref(dev), "room", t.Room)
		} else {
			e.Log.Info("Held group member once it could be written", "rule", e.ruleRef(key), "device", ref(dev))
		}
		added = append(added, dev)
	}
	return added, retry
}

// applyHold writes a hold's start (its value) or end (the opposite of a bool, and nothing for a
// number or for a capability with NoEndWrite, such as locked). On an arbitrated capability, the
// start is a claim of the hold's value, and the end releases it. It returns the devices it wrote or
// claimed, and how soon to retry when it fails.
func (e *Engine) applyHold(ctx context.Context, key ident.ID, t HoldTarget, start bool, claim control.Claim) (written []ident.ID, retry time.Duration, failed error) {
	claim.Value = capability.Float(t.Value)
	value := t.Value
	// An unarbitrated number hold leaves the device as it is when it ends (OIK-64, answer 19), and so
	// does a lock's: only an explicit write unlocks a door (OIK-23).
	skipEnd := !start && !writesEnd(t)
	if b, ok := value.(bool); ok && !start {
		value = !b
	}
	log := e.Log.With("rule", e.ruleRef(key), "capability", t.Capability)
	devices := named(t)
	if t.Room != "" {
		var undiscovered bool
		devices, undiscovered = e.roomTargets(ctx, key, t, start, claim.Source)
		// With no member to write yet, as on startup, wait for the undiscovered ones. Once some
		// are written, one that never reports doesn't keep rewriting the rest.
		if len(devices) == 0 && undiscovered {
			log.Debug("Waiting for Room members before writing hold", "room", t.Room)
			return nil, retryUndiscovered, fmt.Errorf("no member of room %s is discovered", t.Room)
		}
	}
	// A group's member that isn't discovered yet joins the hold once it is, as a room's does, so it
	// holds up neither the others' start nor the end.
	waiting := false
	for _, dev := range devices {
		snap, ok := e.Store.Get(dev)
		if ok && snap.Controls(t.Capability) {
			if start && e.claim(ctx, key, dev, t.Capability, claim) || !start && e.releaseClaim(ctx, dev, t.Capability, claim.Source) {
				written = append(written, dev)
				continue
			}
		}
		if skipEnd {
			continue
		}
		if !ok || !snap.Discovered {
			log.Debug("Waiting for Device before writing hold", "device", ref(dev))
			if len(t.Devices) > 0 {
				waiting = true
				continue
			}
			retry, failed = max(retry, retryUndiscovered), fmt.Errorf("device %s isn't discovered", dev.Name)
			continue
		}
		if err := e.Setter.Set(writtenFor(ctx, ruleSource(key.Name)), dev, t.Capability, value); err != nil {
			if ctx.Err() == nil {
				log.Error("Failed to write hold", "err", err, "device", ref(dev))
			}
			retry, failed = retryFailed, err
			continue
		}
		written = append(written, dev)
	}
	if waiting && len(written) == 0 && failed == nil && !skipEnd {
		return nil, retryUndiscovered, fmt.Errorf("no member of the group is discovered")
	}
	return written, retry, failed
}

// roomTargets are the members of a room hold's room it writes: discovered with the capability, and
// of its types. waiting says some member isn't discovered yet. A member of another type is left
// out, and reported on a start, and an end lets go of a claim it may still hold.
func (e *Engine) roomTargets(ctx context.Context, key ident.ID, t HoldTarget, start bool, source string) (devices []ident.ID, waiting bool) {
	var skipped []string
	for _, dev := range e.roomMembers(t) {
		snap, ok := e.Store.Get(dev)
		waiting = waiting || !ok || !snap.Discovered
		if !ok || !snap.Discovered || !slices.Contains(snap.Info.Capabilities, t.Capability) {
			continue
		}
		if !snap.OfTypes(t.DeviceTypes) {
			// A member retyped while the hold was active may still hold its claim, which its end
			// lets go of. It writes nothing to one: the hold never wrote it.
			if !start && snap.Controls(t.Capability) {
				e.releaseClaim(ctx, dev, t.Capability, source)
			}
			skipped = append(skipped, otherType(dev, snap))
			continue
		}
		devices = append(devices, dev)
	}
	if start {
		e.reportSkipped(key, skipped)
	}
	return devices, waiting
}

// writesEnd says a hold's end writes to an unarbitrated target: the opposite of a bool, unless
// the capability has NoEndWrite.
func writesEnd(t HoldTarget) bool {
	_, isBool := t.Value.(bool)
	spec, _ := capability.Lookup(t.Capability)
	return isBool && !spec.NoEndWrite
}

// claimHold makes an active hold's claim again on its arbitrated targets, and writes nothing.
func (e *Engine) claimHold(ctx context.Context, key ident.ID, t HoldTarget, claim control.Claim) {
	claim.Value = capability.Float(t.Value)
	devices := e.roomMembers(t)
	if t.Room == "" {
		devices = named(t)
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
		e.Log.Info("Released no hold; the Rule isn't loaded or has none", "rule", e.ruleRef(key))
		return
	}
	s.hold.released = true
	s.next = fire{At: e.Clock.Now()}
	e.signal()
}
