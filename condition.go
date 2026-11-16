package oikade

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/sun"
)

// Condition is something true or false about the home now. A Bool is one, as are a Number's Above,
// Below and Within, Daily, Yearly, Any, All and Not of others, and a ConditionFunc. A condition that
// reads a value nobody has reported yet stops there: a hold keeps its state, and a moment doesn't
// run.
//
//	oikade.All(PoolWater.Temperature.Below(3), oikade.Daily("22:00", "06:00"))
type Condition interface {
	// holds reports whether the condition is true now. A read of a value nobody has reported yet
	// panics, as Snapshot's Get does.
	holds(s Snapshot) bool
	// explain says why the condition isn't want: why it's false for true, and why it's true for
	// false, as Not needs.
	explain(s Snapshot, want bool) string
	// check reports a mistake in the condition, such as a date that isn't one, for oikade check.
	check(o Options) error
}

func (b Bool) holds(s Snapshot) bool { return s.Get(b) }

func (b Bool) explain(s Snapshot, _ bool) string {
	return fmt.Sprintf("%s %s is %t", b.Device, b.Capability, s.Get(b))
}

func (b Bool) check(Options) error { return checkHandle(b.handle()) }

func checkHandle(h handle) error {
	if h.device == "" || h.capability == "" {
		return errors.New("a condition reads a device's capability: give one from devices.go")
	}
	return nil
}

// Above is true while the reading is above v.
func (n Number) Above(v float64) Condition { return compare{n: n, op: above, a: v} }

// Below is true while the reading is below v.
func (n Number) Below(v float64) Condition { return compare{n: n, op: below, a: v} }

// Within is true while the reading is from low to high, both included.
func (n Number) Within(low, high float64) Condition {
	return compare{n: n, op: within, a: low, b: high}
}

type op int

const (
	above op = iota + 1
	below
	within
)

// compare is a reading against a threshold, or two.
type compare struct {
	n    Number
	op   op
	a, b float64
}

func (c compare) holds(s Snapshot) bool {
	v := s.Get(c.n)
	switch c.op {
	case above:
		return v > c.a
	case below:
		return v < c.a
	}
	return v >= c.a && v <= c.b
}

func (c compare) explain(s Snapshot, want bool) string {
	what := fmt.Sprintf("%s %s is %s", c.n.Device, c.n.Capability, number(s.Get(c.n)))
	not := ""
	if want {
		not = "not "
	}
	switch c.op {
	case above:
		return fmt.Sprintf("%s, %sabove %s", what, not, number(c.a))
	case below:
		return fmt.Sprintf("%s, %sbelow %s", what, not, number(c.a))
	}
	return fmt.Sprintf("%s, %swithin %s to %s", what, not, number(c.a), number(c.b))
}

func (c compare) check(Options) error {
	if c.op == within && c.a > c.b {
		return fmt.Errorf("Within(%s, %s) is never true: give the lower first", number(c.a), number(c.b))
	}
	return checkHandle(c.n.handle())
}

func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Hours is a window of each day, from Daily. It's open from its start until the next end, so one
// whose end is at or before its start crosses midnight.
type Hours struct {
	start, end Time
	days       []time.Weekday
}

// Daily is open each day from start until the next end: times such as "22:00", from "00:00" to
// "23:59" in the host's time zone, or a Sunrise or Sunset. An end at or before its start is the next
// day, so Daily("22:00", "06:00") is the night.
//
//	oikade.Daily(oikade.Sunset(0), oikade.Sunrise(0)) // while it's dark
func Daily[T string | Time](start, end T) Hours {
	return Hours{start: timeOf(start), end: timeOf(end)}
}

func timeOf[T string | Time](t T) Time {
	switch t := any(t).(type) {
	case string:
		return Clock(t)
	case Time:
		return t
	}
	panic("unreachable")
}

// Days limits the window to those that start on these days, adding to any it has.
func (h Hours) Days(days ...time.Weekday) Hours {
	h.days = append(slices.Clip(h.days), days...)
	return h
}

func (h Hours) holds(s Snapshot) bool {
	in, err := h.toEngine().Contains(s.now, s.location, s.site)
	if err != nil {
		panic(fmt.Sprintf("Daily: %v", err))
	}
	return in
}

func (h Hours) explain(s Snapshot, want bool) string {
	where := "outside"
	if !want {
		where = "inside"
	}
	layout := "15:04"
	if len(h.days) > 0 {
		layout = "Mon 15:04"
	}
	return fmt.Sprintf("it's %s, %s %s", s.now.In(s.location).Format(layout), where, h)
}

func (h Hours) check(o Options) error {
	if err := needsLocation(o, h.start, h.end); err != nil {
		return err
	}
	// Any day will do: it's the times that are checked.
	if _, err := h.toEngine().Contains(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC), time.UTC, o.site()); err != nil {
		return fmt.Errorf("Daily(%s): %w", h, err)
	}
	return nil
}

// String is the window as it's written, such as "22:00 to 06:00".
func (h Hours) String() string {
	out := h.start.String() + " to " + h.end.String()
	if len(h.days) > 0 {
		days := make([]string, 0, len(h.days))
		for _, d := range h.days {
			days = append(days, d.String()[:3])
		}
		out += " starting " + strings.Join(days, ", ")
	}
	return out
}

func (h Hours) toEngine() *engine.Window {
	return &engine.Window{Start: h.start.timer(), End: h.end.timer(), Days: h.days}
}

// String is the time as it's written: "06:30", "sunrise" or "sunset-20m".
func (t Time) String() string {
	if !t.isSun() {
		return t.clock
	}
	name := "sunrise"
	if t.sun == sun.Sunset {
		name = "sunset"
	}
	switch {
	case t.offset > 0:
		return name + "+" + t.offset.String()
	case t.offset < 0:
		return name + t.offset.String()
	}
	return name
}

// Dates is a stretch of each year, from Yearly.
type Dates struct {
	start, end string
}

// Yearly is true from the start of the day start until the end of the day end, each year, in the
// host's time zone: dates such as "1 Jun" or "15 September". An end before its start is the next
// year, so Yearly("1 Nov", "28 Feb") is the winter. A date that isn't one fails oikade check.
func Yearly(start, end string) Dates { return Dates{start: start, end: end} }

// date is a day of the year, as a month and a day.
type date struct {
	month time.Month
	day   int
}

func (d date) before(o date) bool { return d.month < o.month || d.month == o.month && d.day < o.day }

func parseDate(s string) (date, error) {
	for _, layout := range []string{"2 Jan", "2 January"} {
		// Year 0 is a leap year, so 29 Feb parses.
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return date{t.Month(), t.Day()}, nil
		}
	}
	return date{}, fmt.Errorf("%q isn't a date such as \"1 Jun\"", s)
}

func (y Dates) dates() (date, date, error) {
	start, err := parseDate(y.start)
	end, endErr := parseDate(y.end)
	return start, end, errors.Join(err, endErr)
}

func (y Dates) holds(s Snapshot) bool {
	start, end, err := y.dates()
	if err != nil {
		panic(fmt.Sprintf("Yearly: %v", err))
	}
	month, day := s.now.In(s.location).Month(), s.now.In(s.location).Day()
	today := date{month, day}
	if end.before(start) {
		return !today.before(start) || !end.before(today)
	}
	return !today.before(start) && !end.before(today)
}

func (y Dates) explain(s Snapshot, want bool) string {
	where := "outside"
	if !want {
		where = "inside"
	}
	return fmt.Sprintf("it's %s, %s %s to %s", s.now.In(s.location).Format("2 Jan"), where, y.start, y.end)
}

func (y Dates) check(Options) error {
	if _, _, err := y.dates(); err != nil {
		return fmt.Errorf("Yearly: %w", err)
	}
	return nil
}

// Any is true while any of the conditions is.
func Any(c Condition, more ...Condition) Condition { return anyOf(append([]Condition{c}, more...)) }

// All is true while all of the conditions are.
func All(c Condition, more ...Condition) Condition { return allOf(append([]Condition{c}, more...)) }

// Not is true while c is false.
func Not(c Condition) Condition { return not{c} }

type anyOf []Condition

func (a anyOf) holds(s Snapshot) bool {
	return slices.ContainsFunc(a, func(c Condition) bool { return c.holds(s) })
}

func (a anyOf) explain(s Snapshot, want bool) string {
	if want {
		return joinExplained(s, a, true)
	}
	i := slices.IndexFunc(a, func(c Condition) bool { return c.holds(s) })
	return a[i].explain(s, false)
}

func (a anyOf) check(o Options) error { return checkAll("Any", a, o) }

type allOf []Condition

func (a allOf) holds(s Snapshot) bool {
	return !slices.ContainsFunc(a, func(c Condition) bool { return !c.holds(s) })
}

func (a allOf) explain(s Snapshot, want bool) string {
	if !want {
		return joinExplained(s, a, false)
	}
	i := slices.IndexFunc(a, func(c Condition) bool { return !c.holds(s) })
	return a[i].explain(s, true)
}

func (a allOf) check(o Options) error { return checkAll("All", a, o) }

func joinExplained(s Snapshot, cs []Condition, want bool) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.explain(s, want))
	}
	return strings.Join(parts, ", and ")
}

func checkAll(what string, cs []Condition, o Options) error {
	errs := make([]error, 0, len(cs))
	for i, c := range cs {
		if c == nil {
			errs = append(errs, fmt.Errorf("%s has a nil condition, at %d", what, i))
			continue
		}
		errs = append(errs, c.check(o))
	}
	return errors.Join(errs...)
}

type not struct{ c Condition }

func (n not) holds(s Snapshot) bool { return !n.c.holds(s) }

func (n not) explain(s Snapshot, want bool) string { return n.c.explain(s, !want) }

func (n not) check(o Options) error { return checkAll("Not", []Condition{n.c}, o) }

// ConditionFunc is a condition written as Go, for what the others can't say. label is what oikade
// status shows for it, since it can't say why f is false. f must not block: it holds up every other
// rule, and one that runs for too long gets the process replaced.
//
//	oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool {
//		return slices.Max(s.Readings(oikade.InRoom("cellar", oikade.Everything, oikade.Humidity))) > 65
//	})
func ConditionFunc(label string, f func(Snapshot) bool) Condition { return funcCondition{label, f} }

type funcCondition struct {
	label string
	f     func(Snapshot) bool
}

func (c funcCondition) holds(s Snapshot) bool { return c.f(s) }

func (c funcCondition) explain(_ Snapshot, want bool) string {
	return fmt.Sprintf("%s is %t", c.label, !want)
}

func (c funcCondition) check(Options) error {
	switch {
	case c.label == "":
		return errors.New("a ConditionFunc needs a label")
	case c.f == nil:
		return fmt.Errorf("ConditionFunc %q has no function", c.label)
	}
	return nil
}

// checkCondition checks c, which may be nil for none.
func checkCondition(c Condition, o Options) error {
	if c == nil {
		return nil
	}
	return c.check(o)
}

// explain says why c is false now, or what stops it being worked out, for oikade status.
func explain(c Condition, vars engine.Vars) (why string) {
	err := evaluate(vars, func(s Snapshot) {
		why = ""
		if !c.holds(s) {
			why = c.explain(s, true)
		}
	})
	if err != nil {
		return err.Error()
	}
	return why
}

// toEngine turns a condition into the engine's. Nil is none.
func toEngine(c Condition) engine.Condition {
	if c == nil {
		return nil
	}
	return func(vars engine.Vars) (ok bool, err error) {
		err = evaluate(vars, func(s Snapshot) { ok = c.holds(s) })
		return ok, err
	}
}

// splitHours takes a hold's Daily out of its condition, on its own or in an All, so the engine starts
// and ends the hold on the minute of its edge. It returns what's left, or nil for nothing.
func splitHours(c Condition) (*Hours, Condition) {
	switch c := c.(type) {
	case Hours:
		return &c, nil
	case allOf:
		i := slices.IndexFunc(c, func(m Condition) bool { _, ok := m.(Hours); return ok })
		if i < 0 {
			return nil, c
		}
		h := c[i].(Hours)
		rest := slices.Delete(slices.Clone(c), i, i+1)
		switch len(rest) {
		case 0:
			return &h, nil
		case 1:
			return &h, rest[0]
		}
		return &h, rest
	}
	return nil, c
}
