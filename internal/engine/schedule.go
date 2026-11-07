package engine

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/harrymitchinson/oikade/internal/sun"
)

// Trigger types, as a condition sees them in Vars["trigger"].
const (
	TypeChange = "change"
	TypeAt     = "at"
	TypeSun    = "sun"
)

// lookback bounds how far back a missed fire is looked for, and how far ahead the next one:
// a week of days, plus a day for offsets that cross midnight.
const lookback = 8

// timeTrigger is a compiled at or sun trigger.
type timeTrigger struct {
	typ string
	// minutes is an at trigger's local time of day.
	minutes int
	event   sun.Event
	offset  time.Duration
	// days is empty for every day.
	days []time.Weekday
}

// hhmm is a time of day, as HH:MM.
var hhmm = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// atTime parses an HH:MM time of day.
func atTime(s string) (timeTrigger, error) {
	parts := hhmm.FindStringSubmatch(s)
	if parts == nil {
		return timeTrigger{}, fmt.Errorf("time %q is not HH:MM", s)
	}
	h, _ := strconv.Atoi(parts[1])
	m, _ := strconv.Atoi(parts[2])
	return timeTrigger{typ: TypeAt, minutes: h*60 + m}, nil
}

func checkSunOffset(d time.Duration) error {
	if d < -6*time.Hour || d > 6*time.Hour {
		return fmt.Errorf("offset %v is not between -6h and 6h", d)
	}
	return nil
}

// on returns when the trigger fires on the local calendar day y-m-d, if it does. A sun trigger
// without a site never fires.
func (t timeTrigger) on(y int, m time.Month, d int, loc *time.Location, site *sun.Site) (time.Time, bool) {
	var at time.Time
	switch t.typ {
	case TypeAt:
		at = wallClock(y, m, d, t.minutes, loc)
	case TypeSun:
		if site == nil {
			return time.Time{}, false
		}
		ev, ok := site.Time(t.event, y, m, d, loc)
		if !ok {
			return time.Time{}, false
		}
		at = ev.Add(t.offset)
	}
	if len(t.days) > 0 && !slices.Contains(t.days, at.In(loc).Weekday()) {
		return time.Time{}, false
	}
	return at, true
}

// wallClock returns minutes past midnight on y-m-d in loc. A time a daylight saving change skips
// is when the clocks go forward, and a time that happens twice is the first.
func wallClock(y int, m time.Month, d, minutes int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, minutes/60, minutes%60, 0, 0, loc)
	if t.Hour()*60+t.Minute() != minutes {
		// Skipped: time.Date moved it past the change, which starts t's zone period.
		start, _ := t.ZoneBounds()
		return start
	}
	start, _ := t.ZoneBounds()
	if !start.IsZero() {
		_, before := start.Add(-time.Second).Zone()
		_, after := t.Zone()
		if earlier := t.Add(-time.Duration(before-after) * time.Second); before > after &&
			earlier.Before(start) && earlier.Hour()*60+earlier.Minute() == minutes {
			return earlier
		}
	}
	return t
}

// fire is when a timer fires, and its trigger type.
type fire struct {
	At  time.Time
	Typ string
}

// fires returns the timers' fire times on the local days from the day before from to the day
// after to, in order.
func (c *Compiled) fires(from, to time.Time, loc *time.Location, site *sun.Site) []fire {
	var out []fire
	first := from.In(loc).AddDate(0, 0, -1)
	last := to.In(loc).AddDate(0, 0, 1)
	for day := time.Date(first.Year(), first.Month(), first.Day(), 12, 0, 0, 0, loc); !day.After(last); day = day.AddDate(0, 0, 1) {
		for _, t := range c.timers {
			if at, ok := t.on(day.Year(), day.Month(), day.Day(), loc, site); ok {
				out = append(out, fire{At: at, Typ: t.typ})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b fire) int { return a.At.Compare(b.At) })
	return out
}

// nextFire returns the first fire after t. It reports false when none is due within a week.
func (c *Compiled) nextFire(t time.Time, loc *time.Location, site *sun.Site) (fire, bool) {
	for _, f := range c.fires(t, t.AddDate(0, 0, lookback), loc, site) {
		if f.At.After(t) {
			return f, true
		}
	}
	return fire{}, false
}

// lastFire returns the latest fire after since and at or before t, looking back at most a week.
func (c *Compiled) lastFire(since, t time.Time, loc *time.Location, site *sun.Site) (fire, bool) {
	if limit := t.AddDate(0, 0, -lookback); since.Before(limit) {
		since = limit
	}
	fires := c.fires(since, t, loc, site)
	for _, fire := range slices.Backward(fires) {
		if f := fire; !f.At.After(t) {
			return f, f.At.After(since)
		}
	}
	return fire{}, false
}
