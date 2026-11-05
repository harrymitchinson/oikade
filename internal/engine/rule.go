package engine

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/sun"
)

// Mode decides what happens when a trigger fires while a run is in progress.
type Mode string

const (
	// ModeSingle ignores triggers while a run is in progress.
	ModeSingle Mode = "Single"
	// ModeRestart cancels the run in progress and starts again.
	ModeRestart Mode = "Restart"
)

// Priority is the level of a rule's claims. Empty means Normal.
type Priority string

const (
	PrioritySafety   Priority = "Safety"
	PriorityOverride Priority = "Override"
	PriorityNormal   Priority = "Normal"
)

// Until says what ends a claim. Empty means its for, or nothing.
type Until string

const (
	// UntilNext ends the claim when the result without it next changes.
	UntilNext Until = "Next"
	// UntilReleased ends the claim at a release action.
	UntilReleased Until = "Released"
)

// Vars are what a condition sees: "devices", each device's capability values by name; "now", a
// time.Time; "tz", the household's IANA timezone; and for a moment, "trigger".
type Vars = map[string]any

// Condition is a rule's condition. An error means it can't be evaluated yet, as before a device it
// reads has reported: a moment doesn't run, and a hold keeps its state. It runs with the engine's
// lock held, so it must not block, and rules share its Vars, so it must not change them. A panic
// counts as an error.
type Condition func(Vars) (bool, error)

// guarded turns a panic in a Go condition into an error. CEL can't panic, but one Go condition
// reading a device that hasn't reported mustn't stop every other rule.
func guarded(c Condition) Condition {
	if c == nil {
		return nil
	}
	return func(v Vars) (ok bool, err error) {
		defer func() {
			if r := recover(); r != nil {
				ok, err = false, fmt.Errorf("condition panicked: %v", r)
			}
		}()
		return c(v)
	}
}

// checkDays rejects a value that isn't a weekday, which would never match.
func checkDays(days []time.Weekday) error {
	for _, d := range days {
		if d < time.Sunday || d > time.Saturday {
			return fmt.Errorf("unknown day %d", int(d))
		}
	}
	return nil
}

// Change fires when a capability changes on a device, or on any device in a room.
type Change struct {
	Device     string
	Room       string
	Capability string
}

func (c Change) check() []error {
	var errs []error
	if (c.Device == "") == (c.Room == "") {
		errs = append(errs, errors.New("exactly one of device or room is required"))
	}
	if _, ok := capability.Lookup(c.Capability); !ok {
		errs = append(errs, fmt.Errorf("unknown capability %q", c.Capability))
	}
	return errs
}

// Timer is a time of day: At or SunTime.
type Timer interface {
	timer() (timeTrigger, error)
}

// At is a wall-clock time, "HH:MM". No days means every day.
type At struct {
	Time string
	Days []time.Weekday
}

func (a At) timer() (timeTrigger, error) {
	t, err := atTime(a.Time)
	if err != nil {
		return t, err
	}
	t.days = slices.Clone(a.Days)
	return t, checkDays(a.Days)
}

// SunTime is a sunrise or sunset, moved by at most 6 hours. No days means every day.
type SunTime struct {
	Event  sun.Event
	Offset time.Duration
	Days   []time.Weekday
}

func (s SunTime) timer() (timeTrigger, error) {
	if s.Event != sun.Sunrise && s.Event != sun.Sunset {
		return timeTrigger{}, fmt.Errorf("unknown event %v", s.Event)
	}
	t := timeTrigger{typ: TypeSun, event: s.Event, offset: s.Offset, days: slices.Clone(s.Days)}
	return t, errors.Join(checkSunOffset(s.Offset), checkDays(s.Days))
}

// Action is a step of a moment: Set, Delay or Release.
type Action interface {
	action() (action, error)
}

// Set writes a capability on a device or a room. For, Until and Priority make it a claim on an
// arbitrated capability.
type Set struct {
	Device     string
	Room       string
	Capability string
	// Value is a bool or a number.
	Value    any
	For      time.Duration
	Until    Until
	Priority Priority
}

func (s Set) action() (action, error) {
	if (s.Device == "") == (s.Room == "") {
		return action{}, errors.New("exactly one of device or room is required")
	}
	v, err := writableValue(s.Capability, s.Value)
	if err != nil {
		return action{}, err
	}
	if s.For < 0 {
		return action{}, errors.New("for must be positive")
	}
	switch s.Until {
	case "", UntilNext:
	case UntilReleased:
		if s.For != 0 {
			return action{}, errors.New("for can't be combined with until: Released")
		}
	default:
		return action{}, fmt.Errorf("unknown until %q", s.Until)
	}
	if err := checkPriority(s.Priority); err != nil {
		return action{}, err
	}
	return action{device: s.Device, room: s.Room, capability: s.Capability, value: v, claimFor: s.For, until: s.Until, priority: s.Priority}, nil
}

// Delay waits before the next action.
type Delay time.Duration

func (d Delay) action() (action, error) {
	if d <= 0 {
		return action{}, errors.New("delay must be positive")
	}
	return action{delay: time.Duration(d)}, nil
}

// Release ends the named rule's claims.
type Release string

func (r Release) action() (action, error) {
	if r == "" {
		return action{}, errors.New("release needs a rule")
	}
	return action{release: string(r)}, nil
}

// Trigger is when a moment, or a flow, starts.
type Trigger struct {
	Changes []Change
	Timers  []Timer
	// Condition, if set, must hold for the trigger to fire.
	Condition Condition
	// StartingDeadline is how late a missed timer may still fire on startup.
	StartingDeadline time.Duration
	// Since is when a missed timer was last accounted for. Zero means when the rule loads, so
	// nothing before that counts as missed. Keep it across restarts to catch up on what was
	// missed while down.
	Since time.Time
	// Generation is the rule's version. Load ignores a rule with the same key and generation as
	// the loaded one, so a changed rule needs a higher one. Zero means 1.
	Generation int64
}

func (t Trigger) compile(key types.NamespacedName) (*Compiled, []error) {
	c := &Compiled{
		Key: key, Generation: max(t.Generation, 1), Mode: ModeSingle, condition: guarded(t.Condition),
		StartingDeadline: t.StartingDeadline, Since: t.Since, sinceLoad: t.Since.IsZero(), changes: slices.Clone(t.Changes),
	}
	var errs []error
	if len(t.Changes) == 0 && len(t.Timers) == 0 {
		errs = append(errs, errors.New("a trigger is required"))
	}
	if t.StartingDeadline < 0 {
		errs = append(errs, errors.New("startingDeadline must not be negative"))
	}
	for i, change := range t.Changes {
		for _, err := range change.check() {
			errs = append(errs, fmt.Errorf("changes[%d]: %w", i, err))
		}
	}
	for i, timer := range t.Timers {
		tt, err := compileTimer(timer)
		if err != nil {
			errs = append(errs, fmt.Errorf("timers[%d]: %w", i, err))
			continue
		}
		c.timers = append(c.timers, tt)
	}
	return c, errs
}

func compileTimer(t Timer) (timeTrigger, error) {
	if t == nil {
		return timeTrigger{}, errors.New("no time")
	}
	return t.timer()
}

// Moment runs its actions when its trigger fires.
type Moment struct {
	Trigger
	// Mode defaults to ModeSingle.
	Mode Mode
	Do   []Action
}

// NewMoment compiles a moment written in Go. It's checked as a Rule is.
func NewMoment(key types.NamespacedName, m Moment) (*Compiled, error) {
	c, errs := m.compile(key)
	switch m.Mode {
	case "":
	case ModeSingle, ModeRestart:
		c.Mode = m.Mode
	default:
		errs = append(errs, fmt.Errorf("unknown mode %q", m.Mode))
	}
	if len(m.Do) == 0 {
		errs = append(errs, errors.New("do is required"))
	}
	for i, do := range m.Do {
		if do == nil {
			errs = append(errs, fmt.Errorf("do[%d]: no action", i))
			continue
		}
		a, err := do.action()
		if err != nil {
			errs = append(errs, fmt.Errorf("do[%d]: %w", i, err))
			continue
		}
		c.actions = append(c.actions, a)
	}
	return c, errors.Join(errs...)
}

// NewTrigger compiles a trigger with nothing to run yet, for a flow to attach to.
func NewTrigger(key types.NamespacedName, t Trigger) (*Compiled, error) {
	c, errs := t.compile(key)
	return c, errors.Join(errs...)
}

// Window is when a hold may be active. It's open from Start until the first End after it, so it
// may cross midnight. Start and End take no days of their own: Days limits which starts count.
type Window struct {
	Start, End Timer
	Days       []time.Weekday
}

func (w *Window) compile() (*window, error) {
	start, startErr := windowTime(w.Start)
	if startErr != nil {
		startErr = fmt.Errorf("start: %w", startErr)
	}
	end, endErr := windowTime(w.End)
	if endErr != nil {
		endErr = fmt.Errorf("end: %w", endErr)
	}
	return &window{start: start, end: end, days: slices.Clone(w.Days)}, errors.Join(startErr, endErr, checkDays(w.Days))
}

func windowTime(t Timer) (timeTrigger, error) {
	tt, err := compileTimer(t)
	if err == nil && len(tt.days) > 0 {
		return tt, errors.New("days belong on the window")
	}
	return tt, err
}

// Hold keeps a capability at Value while its Window is open and When is true. It needs at least
// one of them. Budgets are only available from a Rule so far.
type Hold struct {
	Device     string
	Room       string
	Capability string
	// Value is a bool or a number.
	Value    any
	Priority Priority
	// Window, if set, must be open.
	Window *Window
	// When, if set, must be true. It's looked at on every device change and every minute.
	When              Condition
	OnDelay, OffDelay time.Duration
	// Latch keeps the hold active once it starts, until a Release. It needs When.
	Latch bool
	// Generation is the rule's version, as on a Trigger. Zero means 1.
	Generation int64
}

// NewHold compiles a hold written in Go. It's checked as a Rule is.
func NewHold(key types.NamespacedName, h Hold) (*Compiled, error) {
	c := &Compiled{Key: key, Generation: max(h.Generation, 1), Mode: ModeSingle}
	c.hold = &hold{
		device: h.Device, room: h.Room, capability: h.Capability, priority: h.Priority, when: guarded(h.When),
		onDelay: h.OnDelay, offDelay: h.OffDelay, latch: h.Latch,
	}
	var errs []error
	if (h.Device == "") == (h.Room == "") {
		errs = append(errs, errors.New("hold: exactly one of device or room is required"))
	}
	v, err := writableValue(h.Capability, h.Value)
	if err != nil {
		errs = append(errs, fmt.Errorf("hold: %w", err))
	}
	c.hold.value = v
	if err := checkPriority(h.Priority); err != nil {
		errs = append(errs, err)
	}
	if h.Window == nil && h.When == nil {
		errs = append(errs, errors.New("hold: a window or when is required"))
	}
	if h.Latch && h.When == nil {
		errs = append(errs, errors.New("hold: latch needs when"))
	}
	if h.Window != nil {
		win, err := h.Window.compile()
		if err != nil {
			errs = append(errs, fmt.Errorf("hold.window: %w", err))
		}
		c.hold.window = win
	}
	if h.OnDelay < 0 {
		errs = append(errs, errors.New("hold: onDelay must not be negative"))
	}
	if h.OffDelay < 0 {
		errs = append(errs, errors.New("hold: offDelay must not be negative"))
	}
	return c, errors.Join(errs...)
}
