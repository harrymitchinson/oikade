package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
)

// hold is a compiled Hold.
type hold struct {
	device     string
	room       string
	types      []devicetype.Type
	capability string
	value      any
	priority   Priority
	// window is nil when the hold has none.
	window *window
	// when is nil when the hold has no when.
	when              Condition
	onDelay, offDelay time.Duration
	latch             bool
	// budget is nil when the hold has none.
	budget *budgetHold
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
