// Package framework is how a home is written in Go. A home is a main package that declares its
// holds and moments and hands them to Run:
//
//	func main() {
//		framework.Run(framework.Home{
//			Holds: []framework.Hold{{
//				Name:       "bathroom-fan",
//				Device:     "bathroom-fan",
//				Capability: "switch.on",
//				Value:      true,
//				When: func(s framework.State) bool {
//					return s.Number("bathroom-sensor", "sensor.relative-humidity") > 70
//				},
//			}},
//		})
//	}
//
// The host runs that binary and restarts it if it dies. State that must survive lives in the host,
// so a crash here loses nothing. docs/architecture.md says how.
package framework

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
)

// Priority is the level of a rule's claims on an arbitrated capability.
type Priority string

const (
	// Normal is the default.
	Normal Priority = ""
	// Override beats Normal.
	Override Priority = "Override"
	// Safety beats everything.
	Safety Priority = "Safety"
)

// State is what a condition reads: the home now.
type State struct {
	devices map[string]map[string]any
	now     time.Time
}

// notReported ends a condition that read a value nobody has reported yet.
type notReported struct{ device, capability string }

func (s State) value(device, capability string) any {
	v, ok := s.devices[device][capability]
	if !ok {
		panic(notReported{device, capability})
	}
	return v
}

// Bool reads a bool capability. A condition reading a value nobody has reported yet stops there:
// a hold keeps its state, and a moment doesn't run.
func (s State) Bool(device, capability string) bool {
	b, ok := s.value(device, capability).(bool)
	if !ok {
		panic(fmt.Sprintf("%s %s isn't a bool", device, capability))
	}
	return b
}

// Number reads a number capability, as Bool does.
func (s State) Number(device, capability string) float64 {
	f, ok := s.value(device, capability).(float64)
	if !ok {
		panic(fmt.Sprintf("%s %s isn't a number", device, capability))
	}
	return f
}

// Now is the time of this look at the home.
func (s State) Now() time.Time { return s.now }

// Condition decides from the home now. It must not block: it holds up every other rule, and one
// that runs for too long gets the process replaced.
type Condition func(State) bool

func (c Condition) toEngine() engine.Condition {
	if c == nil {
		return nil
	}
	return func(v engine.Vars) (ok bool, err error) {
		defer func() {
			switch r := recover().(type) {
			case nil:
			case notReported:
				ok, err = false, fmt.Errorf("%s %s hasn't reported", r.device, r.capability)
			default:
				panic(r)
			}
		}()
		devices, _ := v["devices"].(map[string]map[string]any)
		now, _ := v["now"].(time.Time)
		return c(State{devices: devices, now: now}), nil
	}
}

// Window is when a hold may be active, from Start until the next End, each "HH:MM".
type Window struct {
	Start, End string
}

// Hold keeps a capability at Value while its Window is open and When is true. It needs at least
// one of them. Name it once: its state is kept by name.
type Hold struct {
	Name       string
	Device     string
	Room       string
	Capability string
	// Value is a bool or a number.
	Value    any
	Priority Priority
	Window   *Window
	When     Condition
	// OnDelay and OffDelay are how long When must stay true or false first.
	OnDelay, OffDelay time.Duration
	// Latch keeps the hold active once it starts, until a Release.
	Latch bool
}

// Change is a capability changing on a device or on any device in a room.
type Change struct {
	Device     string
	Room       string
	Capability string
}

// Moment runs Do when a trigger fires and If, if set, is true. It needs a trigger: a Change, or a
// time of day in At as "HH:MM".
type Moment struct {
	Name string
	On   []Change
	At   []string
	If   Condition
	// Restart cancels a run in progress on a new trigger. Without it, the trigger is ignored.
	Restart bool
	Do      []Action
}

// Action is a step of a moment: Set, Wait or Release.
type Action interface {
	toEngine() engine.Action
}

// Set writes a capability. For and Until make it a claim on an arbitrated capability, which needs
// one of them.
type Set struct {
	Device     string
	Room       string
	Capability string
	// Value is a bool or a number.
	Value any
	// For ends the claim after this long.
	For time.Duration
	// UntilReleased ends the claim at a Release.
	UntilReleased bool
	// UntilNext ends the claim when the value without it next changes.
	UntilNext bool
	Priority  Priority
}

func (s Set) toEngine() engine.Action {
	set := engine.Set{
		Device: s.Device, Room: s.Room, Capability: s.Capability, Value: s.Value, For: s.For,
		Priority: engine.Priority(s.Priority),
	}
	switch {
	case s.UntilReleased:
		set.Until = engine.UntilReleased
	case s.UntilNext:
		set.Until = engine.UntilNext
	}
	return set
}

// Wait waits before the next action.
type Wait time.Duration

func (w Wait) toEngine() engine.Action { return engine.Delay(w) }

// Release ends the named rule's claims, and its latch if it's a hold.
type Release string

func (r Release) toEngine() engine.Action { return engine.Release(r) }

// Home is a home's automations.
type Home struct {
	Holds   []Hold
	Moments []Moment
}

// compile checks the home and turns it into the engine's rules.
func (h Home) compile() ([]*engine.Compiled, error) {
	var rules []*engine.Compiled
	var errs []error
	seen := map[string]bool{}
	name := func(n string) types.NamespacedName {
		switch {
		case n == "":
			errs = append(errs, errors.New("a rule needs a name"))
		case seen[n]:
			errs = append(errs, fmt.Errorf("rule %q is declared twice", n))
		}
		seen[n] = true
		return types.NamespacedName{Namespace: wire.Namespace, Name: n}
	}
	add := func(rule string, c *engine.Compiled, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", rule, err))
			return
		}
		rules = append(rules, c)
	}
	for _, r := range h.Holds {
		c, err := engine.NewHold(name(r.Name), r.toEngine())
		add(r.Name, c, err)
	}
	for _, r := range h.Moments {
		c, err := engine.NewMoment(name(r.Name), r.toEngine())
		add(r.Name, c, err)
	}
	return rules, errors.Join(errs...)
}

func (r Hold) toEngine() engine.Hold {
	h := engine.Hold{
		Device: r.Device, Room: r.Room, Capability: r.Capability, Value: r.Value,
		Priority: engine.Priority(r.Priority), When: r.When.toEngine(),
		OnDelay: r.OnDelay, OffDelay: r.OffDelay, Latch: r.Latch,
	}
	if r.Window != nil {
		h.Window = &engine.Window{Start: engine.At{Time: r.Window.Start}, End: engine.At{Time: r.Window.End}}
	}
	return h
}

func (r Moment) toEngine() engine.Moment {
	m := engine.Moment{Trigger: engine.Trigger{Condition: r.If.toEngine()}}
	for _, c := range r.On {
		m.Changes = append(m.Changes, engine.Change(c))
	}
	for _, at := range r.At {
		m.Timers = append(m.Timers, engine.At{Time: at})
	}
	if r.Restart {
		m.Mode = engine.ModeRestart
	}
	for _, a := range r.Do {
		if a == nil {
			m.Do = append(m.Do, nil)
			continue
		}
		m.Do = append(m.Do, a.toEngine())
	}
	return m
}

// Run runs the home and doesn't return. The host starts this binary as "<binary> automations".
// Anything else is an error, because the home has nothing to do without the host.
func Run(h Home) {
	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(h, log); err != nil {
		log.Error(err, "Stopped automations process")
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
