package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/devicetype"
)

// hold is a compiled Hold.
type hold struct {
	device     string
	devices    []string
	room       string
	types      []devicetype.Type
	except     []string
	capability string
	value      any
	priority   Priority
	// window is nil when the hold has none.
	window *window
	// when is nil when the hold has no when.
	when              Condition
	onDelay, offDelay time.Duration
	latch             bool
	// until is nil when the latch ends only with a release.
	until Condition
	// whyNot is nil when the hold can't say why it's inactive.
	whyNot func(Vars) string
	// budget is nil when the hold has none.
	budget *budgetHold
	// follow is nil when the hold's value is its own, and followStep what a followed value rounds to.
	follow     func(Vars) (float64, error)
	followStep float64
}

// window is a compiled Window. Its start and end are at or sun times with no days.
type window struct {
	start, end timeTrigger
	days       []time.Weekday
}

// at reports whether t is inside the window, and when that next changes. The window is open from
// a start on one of its days until the first end after it, so it may cross midnight. An end at
// the same time as the start makes it a whole day. Next is zero when nothing changes within a
// week, as with sun times and no site.
func (w *window) at(t time.Time, loc *time.Location, suns *sunCache) (in bool, next time.Time) {
	local := t.In(loc)
	first := local.AddDate(0, 0, -2)
	for day := time.Date(first.Year(), first.Month(), first.Day(), 12, 0, 0, 0, loc); day.Before(local.AddDate(0, 0, lookback)); day = day.AddDate(0, 0, 1) {
		start, ok := w.start.on(day.Year(), day.Month(), day.Day(), loc, suns)
		if !ok || len(w.days) > 0 && !slices.Contains(w.days, start.In(loc).Weekday()) {
			continue
		}
		end, ok := w.endAfter(start, loc, suns)
		if !ok {
			continue
		}
		if !start.After(t) && t.Before(end) {
			in = true
		}
		for _, b := range []time.Time{start, end} {
			if b.After(t) && (next.IsZero() || b.Before(next)) {
				next = b
			}
		}
	}
	return in, next
}

// endAfter returns the window's first end after start, looking at most two days ahead.
func (w *window) endAfter(start time.Time, loc *time.Location, suns *sunCache) (time.Time, bool) {
	d := start.In(loc)
	for i := range 3 {
		day := d.AddDate(0, 0, i)
		if end, ok := w.end.on(day.Year(), day.Month(), day.Day(), loc, suns); ok && end.After(start) {
			return end, true
		}
	}
	return time.Time{}, false
}

// writableValue validates a value for a writable capability. It's encoded first, so a Go int and
// float64 are checked alike.
func writableValue(name string, value any) (any, error) {
	spec, ok := capability.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown capability %q", name)
	}
	if !spec.Writable {
		return nil, fmt.Errorf("capability %s is read-only", name)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("capability %s: %w", name, err)
	}
	return capability.ParseJSON(name, raw)
}

func checkPriority(p Priority) error {
	switch p {
	case "", PrioritySafety, PriorityOverride, PriorityNormal:
		return nil
	}
	return fmt.Errorf("unknown priority %q", p)
}

// followStep is what a followed value of capability rounds to: step, or the capability's own.
func followStep(name string, step float64) (float64, error) {
	spec, ok := capability.Lookup(name)
	switch {
	case !ok:
		return 0, fmt.Errorf("unknown capability %q", name)
	case !spec.Writable:
		return 0, fmt.Errorf("capability %s is read-only", name)
	case spec.Kind != capability.KindNumber:
		return 0, fmt.Errorf("capability %s isn't a number, so it can't follow a reading", name)
	case step < 0:
		return 0, fmt.Errorf("step %v must be positive", step)
	case step == 0 && spec.Step == 0:
		return 0, fmt.Errorf("capability %s has no step of its own: give one", name)
	case step == 0:
		return spec.Step, nil
	}
	return step, nil
}

// guardedFollow is f, with a panic an error, since the engine holds its lock.
func guardedFollow(f func(Vars) (float64, error)) func(Vars) (float64, error) {
	return func(v Vars) (out float64, err error) {
		defer func() {
			if r := recover(); r != nil {
				out, err = 0, fmt.Errorf("follow panicked: %v", r)
			}
		}()
		return f(v)
	}
}

// followed is v rounded to h's step, within the capability's bounds, as a hold's Value is kept. It
// stays last, the value last written, until v is three quarters of a step from it, so a reading
// that wobbles across a rounding edge isn't a write each time.
func (h *hold) followed(v float64, last any) any {
	if spec, ok := capability.Lookup(h.capability); ok && spec.Max > spec.Min {
		v = min(max(v, spec.Min), spec.Max)
	}
	if last != nil && math.Abs(v-capability.Float(last)) < 0.75*h.followStep {
		return last
	}
	v = math.Round(v/h.followStep) * h.followStep
	if out, err := writableValue(h.capability, v); err == nil {
		return out
	}
	return v
}
