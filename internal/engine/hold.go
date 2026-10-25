package engine

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/cel-go/cel"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/sun"
)

// whenEnv is the CEL environment of while.when: a condition with no trigger.
var whenEnv = mustWhenEnv()

func mustWhenEnv() *cel.Env {
	e, err := cel.NewEnv(
		cel.Variable("devices", cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.DynType))),
		cel.Variable("now", cel.TimestampType),
		cel.Variable("tz", cel.StringType),
	)
	if err != nil {
		panic(err)
	}
	return e
}

// hold is a compiled while and hold.
type hold struct {
	device     string
	room       string
	capability string
	value      any
	priority   homev1alpha1.RulePriority
	// window is nil when the hold has none.
	window *window
	// when is nil when the hold has no when.
	when              cel.Program
	onDelay, offDelay time.Duration
	latch             bool
	// budget is set for a budget hold, which the runtime doesn't run yet (OIK-8).
	budget bool
}

// window is a compiled while.window. Its start and end are at or sun times with no days.
type window struct {
	start, end timeTrigger
	days       []time.Weekday
}

// at reports whether t is inside the window, and when that next changes. The window is open from
// a start on one of its days until the first end after it, so it may cross midnight. An end at
// the same time as the start makes it a whole day. Next is zero when nothing changes within a
// week, as with sun times and no site.
func (w *window) at(t time.Time, loc *time.Location, site *sun.Site) (in bool, next time.Time) {
	local := t.In(loc)
	first := local.AddDate(0, 0, -2)
	for day := time.Date(first.Year(), first.Month(), first.Day(), 12, 0, 0, 0, loc); day.Before(local.AddDate(0, 0, lookback)); day = day.AddDate(0, 0, 1) {
		start, ok := w.start.on(day.Year(), day.Month(), day.Day(), loc, site)
		if !ok || len(w.days) > 0 && !slices.Contains(w.days, start.In(loc).Weekday()) {
			continue
		}
		end, ok := w.endAfter(start, loc, site)
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
func (w *window) endAfter(start time.Time, loc *time.Location, site *sun.Site) (time.Time, bool) {
	d := start.In(loc)
	for i := range 3 {
		day := d.AddDate(0, 0, i)
		if end, ok := w.end.on(day.Year(), day.Month(), day.Day(), loc, site); ok && end.After(start) {
			return end, true
		}
	}
	return time.Time{}, false
}

func compileHold(w *homev1alpha1.While, h *homev1alpha1.Hold, priority homev1alpha1.RulePriority) (*hold, error) {
	if h == nil {
		return nil, errors.New("hold is required with while")
	}
	c := &hold{device: h.Device, room: h.Room, capability: h.Capability, priority: priority, latch: w.Latch, budget: w.Budget != nil}
	var errs []error
	if (h.Device == "") == (h.Room == "") {
		errs = append(errs, errors.New("hold: exactly one of device or room is required"))
	}
	if v, err := writableValue(h.Capability, h.Value.Raw); err != nil {
		errs = append(errs, fmt.Errorf("hold: %w", err))
	} else {
		c.value = v
	}
	if err := checkPriority(priority); err != nil {
		errs = append(errs, err)
	}
	switch {
	case w.Budget != nil:
		// The budget itself is checked against the target's control entry (schedule.Compile).
		if w.Window != nil || w.When != "" || w.OnDelay != nil || w.OffDelay != nil || w.Latch {
			errs = append(errs, errors.New("while: budget must be the only field"))
		}
	case w.Window == nil && w.When == "":
		errs = append(errs, errors.New("while: a window, when or budget is required"))
	}
	if w.Latch && w.When == "" {
		errs = append(errs, errors.New("while: latch needs when"))
	}
	if w.Window != nil {
		win, err := compileWindow(w.Window)
		if err != nil {
			errs = append(errs, fmt.Errorf("while.window: %w", err))
		}
		c.window = win
	}
	if w.When != "" {
		prg, err := compileBool(whenEnv, w.When)
		if err != nil {
			errs = append(errs, fmt.Errorf("while.when: %w", err))
		}
		c.when = prg
	}
	if w.OnDelay != nil {
		if c.onDelay = w.OnDelay.Duration; c.onDelay < 0 {
			errs = append(errs, errors.New("while.onDelay must not be negative"))
		}
	}
	if w.OffDelay != nil {
		if c.offDelay = w.OffDelay.Duration; c.offDelay < 0 {
			errs = append(errs, errors.New("while.offDelay must not be negative"))
		}
	}
	return c, errors.Join(errs...)
}

func compileWindow(w *homev1alpha1.Window) (*window, error) {
	var errs []error
	start, err := compileWindowTime(w.Start)
	if err != nil {
		errs = append(errs, fmt.Errorf("start: %w", err))
	}
	end, err := compileWindowTime(w.End)
	if err != nil {
		errs = append(errs, fmt.Errorf("end: %w", err))
	}
	days, err := compileDays(w.Days)
	errs = append(errs, err)
	return &window{start: start, end: end, days: days}, errors.Join(errs...)
}

func compileWindowTime(t homev1alpha1.WindowTime) (timeTrigger, error) {
	switch {
	case t.Time != "" && t.Sun == "" && t.Offset == nil && hhmm.MatchString(t.Time):
		return compileAt(&homev1alpha1.AtTrigger{Time: t.Time})
	case t.Time == "" && t.Sun != "":
		return compileSun(&homev1alpha1.SunTrigger{Event: t.Sun, Offset: t.Offset})
	case t.Time != "":
		return timeTrigger{}, fmt.Errorf(`%s is neither "HH:MM" nor {sun, offset}`, t.Time)
	default:
		return timeTrigger{}, errors.New(`"HH:MM" or {sun, offset} is required`)
	}
}

// compileBool compiles a CEL expression that must evaluate to a bool.
func compileBool(e *cel.Env, expr string) (cel.Program, error) {
	ast, iss := e.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
		return nil, fmt.Errorf("must evaluate to a bool, not %s", ast.OutputType())
	}
	return e.Program(ast)
}

// writableValue validates a JSON value for a writable capability.
func writableValue(name string, raw []byte) (any, error) {
	spec, ok := capability.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown capability %q", name)
	}
	if !spec.Writable {
		return nil, fmt.Errorf("capability %s is read-only", name)
	}
	return capability.ParseJSON(name, raw)
}

func checkPriority(p homev1alpha1.RulePriority) error {
	switch p {
	case "", homev1alpha1.RulePrioritySafety, homev1alpha1.RulePriorityOverride, homev1alpha1.RulePriorityNormal:
		return nil
	}
	return fmt.Errorf("unknown priority %q", p)
}
