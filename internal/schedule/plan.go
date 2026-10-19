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

// Package schedule plans and runs Schedules: a day of slots, shaped by rules and an optional
// daily budget, that decides when one bool capability is on.
package schedule

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
	"time"
)

// maxSlots bounds a planning day: 25 hours of 15-minute slots is 100.
const maxSlots = 128

// Effect is what a rule does to the slots it matches.
type Effect int

const (
	// Require puts the target on in the slot.
	Require Effect = iota + 1
	// Forbid keeps the budget out of the slot.
	Forbid
	// Prefer weights the slot for the budget by Rule.Prefer.
	Prefer
)

// Window is a daily time window in local wall-clock minutes since midnight. An End at or before
// Start wraps past midnight and belongs to the day it starts.
type Window struct {
	Start, End int
	// Days limits the window to these weekdays. Empty means every day.
	Days []time.Weekday
}

func (w *Window) onDay(d time.Weekday) bool {
	if len(w.Days) == 0 {
		return true
	}
	return slices.Contains(w.Days, d)
}

// covers reports whether the local time t is inside the window.
func (w *Window) covers(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.Start < w.End {
		return m >= w.Start && m < w.End && w.onDay(t.Weekday())
	}
	return (m >= w.Start && w.onDay(t.Weekday())) || (m < w.End && w.onDay((t.Weekday()+6)%7))
}

// Slot is one step of the planning day, in absolute time.
type Slot struct {
	Start, End time.Time
}

// Rule is a planning rule. Live rules never reach the planner.
type Rule struct {
	Name string
	// Window is nil for the whole day.
	Window *Window
	// When is nil for always. An error counts as false.
	When   func(Slot) (bool, error)
	Effect Effect
	// Prefer is the weight of a Prefer rule.
	Prefer int
}

// Reading is a capability value and when it was reported.
type Reading struct {
	Value float64
	At    time.Time
}

// Band maps input values below Below to Minutes. A nil Below matches every value.
type Band struct {
	Below   *float64
	Minutes int
}

// Bands picks the budget from an input reading.
type Bands struct {
	// Input is nil when the input has never reported.
	Input          *Reading
	MaxAge         time.Duration
	MissingMinutes int
	Ranges         []Band
}

// Minutes is the daily budget. Exactly one of Bands or Expression is set.
type Minutes struct {
	Bands      *Bands
	Expression func() (int, error)
}

// Constraints shape the blocks the budget places.
type Constraints struct {
	MinimumRun  time.Duration
	MinimumRest time.Duration
	// MaximumStarts limits blocks per day. Zero means unlimited.
	MaximumStarts int
}

// PlanInput is everything the planner needs for one planning day.
type PlanInput struct {
	Location *time.Location
	// Now is the planning time, which judges the age of a bands input.
	Now time.Time
	// NotBefore makes slots that start before it unavailable.
	NotBefore time.Time
	// Slots are the planning day's slots, contiguous and all of one length, as from DaySlots.
	Slots []Slot
	// Minutes is nil for no budget: the plan is then exactly the required slots.
	Minutes     *Minutes
	Rules       []Rule
	Constraints Constraints
	// LastOff is when the target last went off, carried over from the previous day. Zero means
	// it has rested long enough.
	LastOff time.Time
}

// TargetSource says where the day's budget came from.
type TargetSource string

const (
	SourceBands      TargetSource = "Bands"
	SourceMissing    TargetSource = "Missing"
	SourceExpression TargetSource = "Expression"
	SourceNone       TargetSource = "None"
)

// Block is a run of on slots.
type Block struct {
	Start, End time.Time
}

// Day is a planned day.
type Day struct {
	TargetSource TargetSource
	// TargetMinutes is the budget rounded up to whole slots. Without a budget, it is the required
	// minutes.
	TargetMinutes    int
	ScheduledMinutes int
	ShortfallMinutes int
	Blocks           []Block
	// Errors are rule conditions or the budget expression that failed to evaluate, one per
	// rule. A failed condition counts as false, and a failed expression as a budget of zero.
	Errors []error
}

// DaySlots divides the planning day that starts at planningMinute (minutes since local midnight)
// on the given date into slots of slotMinutes. DST days have fewer or more slots.
func DaySlots(year int, month time.Month, day, planningMinute, slotMinutes int, loc *time.Location) []Slot {
	start := time.Date(year, month, day, 0, planningMinute, 0, 0, loc)
	end := time.Date(year, month, day+1, 0, planningMinute, 0, 0, loc)
	step := time.Duration(slotMinutes) * time.Minute
	var slots []Slot
	for t := start; t.Before(end); t = t.Add(step) {
		e := t.Add(step)
		if e.After(end) {
			e = end
		}
		slots = append(slots, Slot{Start: t, End: e})
	}
	return slots
}

type slotKind int

const (
	allowed slotKind = iota
	unavailable
	required
	forbidden
)

type slotClass struct {
	kind  slotKind
	score int
}

// Plan places the day's budget into slots. It is pure: the same input always gives the same plan.
func Plan(in PlanInput) Day {
	if len(in.Slots) > maxSlots {
		panic(fmt.Sprintf("schedule: %d slots exceeds %d", len(in.Slots), maxSlots))
	}
	var day Day
	classes, errs := classify(in)
	day.Errors = errs

	var on [2]uint64
	reqSlots := 0
	for i, c := range classes {
		if c.kind == required {
			on[i/64] |= 1 << (i % 64)
			reqSlots++
		}
	}
	slotMin := slotMinutes(in.Slots)

	if in.Minutes == nil {
		day.TargetSource = SourceNone
		day.TargetMinutes = reqSlots * slotMin
	} else {
		var minutes int
		var err error
		day.TargetSource, minutes, err = budget(in)
		if err != nil {
			day.Errors = append(day.Errors, err)
		}
		target := 0
		if slotMin > 0 {
			target = (minutes + slotMin - 1) / slotMin
		}
		day.TargetMinutes = target * slotMin
		if target > reqSlots {
			on = search(in, classes, target-reqSlots)
		}
	}

	scheduled := onesCount(on)
	day.ScheduledMinutes = scheduled * slotMin
	day.ShortfallMinutes = max(0, day.TargetMinutes-day.ScheduledMinutes)
	for _, b := range blocks(on, len(in.Slots)) {
		day.Blocks = append(day.Blocks, Block{Start: in.Slots[b.start].Start, End: in.Slots[b.start+b.length-1].End})
	}
	return day
}

func slotMinutes(slots []Slot) int {
	if len(slots) == 0 {
		return 0
	}
	return int(slots[0].End.Sub(slots[0].Start) / time.Minute)
}

// budget returns the day's budget in minutes, clamped to a day.
func budget(in PlanInput) (TargetSource, int, error) {
	m := in.Minutes
	var source TargetSource
	var minutes int
	var err error
	switch {
	case m.Bands != nil:
		source, minutes = SourceMissing, m.Bands.MissingMinutes
		if r := m.Bands.Input; r != nil && in.Now.Sub(r.At) <= m.Bands.MaxAge {
			for _, b := range m.Bands.Ranges {
				if b.Below == nil || r.Value < *b.Below {
					source, minutes = SourceBands, b.Minutes
					break
				}
			}
		}
	case m.Expression != nil:
		source = SourceExpression
		minutes, err = m.Expression()
		if err != nil {
			minutes, err = 0, fmt.Errorf("minutes.expression: %w", err)
		}
	default:
		source = SourceNone
	}
	return source, min(max(minutes, 0), 24*60), err
}

// classify marks each slot unavailable, required, forbidden or allowed with a score. A rule
// matches a slot when its window covers any minute of the slot and its condition is true.
// Precedence is require over forbid over prefer.
func classify(in PlanInput) ([]slotClass, []error) {
	classes := make([]slotClass, len(in.Slots))
	failed := map[string]bool{}
	var errs []error
	for i, s := range in.Slots {
		if s.Start.Before(in.NotBefore) {
			classes[i].kind = unavailable
			continue
		}
		var req, forbid bool
		score := 0
		for _, r := range in.Rules {
			if !inWindow(r.Window, s, in.Location) {
				continue
			}
			if r.When != nil {
				ok, err := r.When(s)
				if err != nil {
					if !failed[r.Name] {
						failed[r.Name] = true
						errs = append(errs, fmt.Errorf("rule %q: %w", r.Name, err))
					}
					continue
				}
				if !ok {
					continue
				}
			}
			switch r.Effect {
			case Require:
				req = true
			case Forbid:
				forbid = true
			case Prefer:
				score += r.Prefer
			}
		}
		switch {
		case req:
			classes[i].kind = required
		case forbid:
			classes[i].kind = forbidden
		default:
			classes[i].score = score
		}
	}
	return classes, errs
}

func inWindow(w *Window, s Slot, loc *time.Location) bool {
	if w == nil {
		return true
	}
	for t := s.Start; t.Before(s.End); t = t.Add(time.Minute) {
		if w.covers(t.In(loc)) {
			return true
		}
	}
	return false
}

// key is a search state after a slot. While the target is on, run counts the block's slots
// (capped at the minimum run), fixed says the block is only required slots, and locked says it
// started too soon after the last block, so the budget can't extend it. While off, gap counts the
// slots since the last block (capped at the minimum rest), and prevFixed says that block was only
// required slots. Required slots are what the user asked for, so constraints only bind blocks the
// budget touches.
type key struct {
	chosen    int
	starts    int // only while maximum starts is set
	on        bool
	run       int
	fixed     bool
	locked    bool
	gap       int
	prevFixed bool
}

// plan is a candidate: the slots it puts on, its score and its starts.
type plan struct {
	on     [2]uint64
	score  int
	starts int
}

// search places up to extra non-required slots, maximising first the minutes placed, then the
// score, then fewer starts, then longer blocks, then earlier blocks. It keeps the best plan for
// each state, slot by slot. Every tie-break is decided by the slots already planned, so keeping
// one plan per state loses nothing.
func search(in PlanInput, classes []slotClass, extra int) [2]uint64 {
	n := len(classes)
	step := in.Slots[0].End.Sub(in.Slots[0].Start)
	minRun := slotsCeil(in.Constraints.MinimumRun, step)
	minRest := slotsCeil(in.Constraints.MinimumRest, step)

	limit := 0
	if in.Constraints.MaximumStarts > 0 {
		// Required runs always start, so the limit never forbids them.
		limit = in.Constraints.MaximumStarts
		runs := 0
		for i, c := range classes {
			if c.kind == required && (i == 0 || classes[i-1].kind != required) {
				runs++
			}
		}
		limit = max(limit, runs)
	}

	// Slots that start before LastOff+MinimumRest count as the rest before the first block.
	start := key{gap: minRest, prevFixed: true}
	if !in.LastOff.IsZero() {
		restUntil := in.LastOff.Add(in.Constraints.MinimumRest)
		resting := 0
		for _, s := range in.Slots {
			if !s.Start.Before(restUntil) {
				break
			}
			resting++
		}
		start.gap = minRest - resting
	}

	layer := map[key]plan{start: {}}
	for i, c := range classes {
		next := make(map[key]plan, len(layer))
		keep := func(k key, p plan) {
			if old, ok := next[k]; !ok || better(p, old, i+1) {
				next[k] = p
			}
		}
		for k, p := range layer {
			switch c.kind {
			case unavailable, forbidden:
				if k2, ok := off(k, minRun, minRest); ok {
					keep(k2, p)
				}
			case required:
				if k2, ok := onSlot(k, true, minRun, minRest, limit); ok {
					keep(k2, withSlot(p, i, 0, k2, k))
				}
			case allowed:
				if k2, ok := off(k, minRun, minRest); ok {
					keep(k2, p)
				}
				if k.chosen < extra {
					if k2, ok := onSlot(k, false, minRun, minRest, limit); ok {
						k2.chosen++
						keep(k2, withSlot(p, i, c.score, k2, k))
					}
				}
			}
		}
		layer = next
	}

	var best plan
	bestChosen := -1
	for k, p := range layer {
		if k.on && !k.fixed && k.run < minRun {
			continue
		}
		if k.chosen > bestChosen || k.chosen == bestChosen && better(p, best, n) {
			best, bestChosen = p, k.chosen
		}
	}
	return best.on
}

func slotsCeil(d, step time.Duration) int {
	return int(math.Ceil(float64(d) / float64(step)))
}

// off moves a state through an off slot.
func off(k key, minRun, minRest int) (key, bool) {
	if k.on {
		if !k.fixed && k.run < minRun {
			return k, false
		}
		return key{chosen: k.chosen, starts: k.starts, gap: min(1, minRest), prevFixed: k.fixed}, true
	}
	k.gap = min(k.gap+1, minRest)
	return k, true
}

// onSlot moves a state through an on slot, required or chosen by the budget.
func onSlot(k key, isRequired bool, minRun, minRest, limit int) (key, bool) {
	if k.on {
		if !isRequired && k.locked {
			return k, false
		}
		k.run = min(k.run+1, minRun)
		k.fixed = k.fixed && isRequired
		return k, true
	}
	short := k.gap < minRest
	if short && (!isRequired || !k.prevFixed) {
		return k, false
	}
	starts := k.starts
	if limit > 0 {
		if starts+1 > limit {
			return k, false
		}
		starts++
	}
	return key{chosen: k.chosen, starts: starts, on: true, run: min(1, minRun), fixed: isRequired, locked: short}, true
}

func withSlot(p plan, i, score int, now, before key) plan {
	p.on[i/64] |= 1 << (i % 64)
	p.score += score
	if !before.on && now.on {
		p.starts++
	}
	return p
}

// better reports whether a beats b over the first n slots: higher score, then fewer starts, then
// longer blocks, comparing the blocks in order, then earlier blocks.
func better(a, b plan, n int) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if a.starts != b.starts {
		return a.starts < b.starts
	}
	ab, bb := blocks(a.on, n), blocks(b.on, n)
	for i := range min(len(ab), len(bb)) {
		if ab[i].length != bb[i].length {
			return ab[i].length > bb[i].length
		}
	}
	// The first slot only one plan has on decides.
	for w := range a.on {
		if x := a.on[w] ^ b.on[w]; x != 0 {
			return a.on[w]&(x&-x) != 0
		}
	}
	return false
}

type run struct{ start, length int }

func blocks(on [2]uint64, n int) []run {
	var runs []run
	for i := range n {
		if on[i/64]&(1<<(i%64)) == 0 {
			continue
		}
		if len(runs) > 0 && runs[len(runs)-1].start+runs[len(runs)-1].length == i {
			runs[len(runs)-1].length++
		} else {
			runs = append(runs, run{start: i, length: 1})
		}
	}
	return runs
}

func onesCount(on [2]uint64) int {
	return bits.OnesCount64(on[0]) + bits.OnesCount64(on[1])
}
