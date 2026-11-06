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
// A flow is ordinary Go that can wait for hours and survive a crash: docs/writing-flows.md.
//
// The host runs that binary and restarts it if it dies. State that must survive lives in the host,
// so a crash here loses nothing. docs/architecture.md says how.
package framework

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/flow"
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
// a hold keeps its state, and a moment doesn't run. So does one with a mistake in it, such as a
// device that doesn't exist, which is logged.
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
		devices, _ := v["devices"].(map[string]map[string]any)
		now, _ := v["now"].(time.Time)
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// Waiting for a device to report is normal. Anything else is a bug: the engine
			// recovers it, and logs it where a person will see it.
			if n, waiting := r.(notReported); waiting {
				if _, declared := devices[n.device]; declared {
					ok, err = false, fmt.Errorf("%s %s hasn't reported", n.device, n.capability)
					return
				}
				panic(fmt.Sprintf("no device %s", n.device))
			}
			panic(r)
		}()
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
	// Late is how late a time in At may still fire, as when no process was running. Zero means
	// DefaultLate. A moment with a Safety Set fires however late it is.
	Late time.Duration
	Do   []Action
}

// DefaultLate is how late a flow may wake, or a time of day fire, and still run.
const DefaultLate = 10 * time.Minute

// Flow runs Run when a trigger fires and If, if set, is true, like a moment. Run is ordinary Go, and
// survives a crash at any point. It must stick to the steps on its FlowRun for anything that
// happens outside it or varies between runs, because after a crash it runs again from the top and
// recorded steps return what they returned the first time. docs/writing-flows.md says how.
type Flow struct {
	Name string
	On   []Change
	At   []string
	If   Condition
	// Restart cancels a run in progress on a new trigger. Without it, the trigger is ignored.
	Restart bool
	// Priority is the level of its Sets that don't give one. A Safety flow never expires.
	Priority Priority
	// Late is how late a run may wake and still carry on. Zero means DefaultLate. A run later than
	// that gives up and logs it, rather than turn the lights off at 09:00.
	Late time.Duration
	Run  func(*FlowRun) error
}

// FlowRun is one run of a flow. Its steps are written to the host's disk before the flow continues.
type FlowRun struct {
	run      *flow.Run
	priority Priority
}

// Now is the time. It returns the same time when a flow replays, which time.Now wouldn't.
func (r *FlowRun) Now() time.Time { return r.run.Now() }

// Sleep waits. A restart doesn't move the time it ends.
func (r *FlowRun) Sleep(d time.Duration) { r.run.Sleep(d) }

// WaitFor waits until c is true and returns true, or returns false once timeout has passed. Zero
// means no limit. A restart doesn't move the time it ends.
func (r *FlowRun) WaitFor(c Condition, timeout time.Duration) bool {
	return r.run.WaitFor(c.toEngine(), timeout)
}

// Set writes a capability, or claims it if it's arbitrated, which needs For, UntilReleased or
// UntilNext, and is an error without one. It may run more than once, after a crash, so set a value
// and never toggle one.
func (r *FlowRun) Set(s Set) error {
	if s.Priority == Normal {
		s.Priority = r.priority
	}
	return r.run.Apply(s.toEngine())
}

// Release ends the named rule's claims, and its latch if it's a hold.
func (r *FlowRun) Release(rule string) error { return r.run.Apply(engine.Release(rule)) }

// Do makes an outside call, which sets result, a pointer, or nil for none. Once it's recorded the
// call never happens again: a replay fills result from the journal and returns an error with the
// same message, which errors.Is won't match. A crash before that repeats it, so make the call safe
// to repeat. Names tell a flow's calls apart.
func (r *FlowRun) Do(name string, result any, call func(context.Context) error) error {
	return r.run.Do(name, result, call)
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
	Flows   []Flow
}

// compile checks the home and turns it into the engine's rules, and returns its flows by name.
func (h Home) compile() ([]*engine.Compiled, map[string]*flow.Flow, error) {
	var rules []*engine.Compiled
	flows := map[string]*flow.Flow{}
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
	for _, r := range h.Flows {
		f := r.toFlow(name(r.Name))
		flows[r.Name] = f
		c, err := engine.NewFlow(f.Key, r.toEngine(f))
		add(r.Name, c, err)
	}
	return rules, flows, errors.Join(errs...)
}

// manifest describes the home to the host, which judges a new version by it.
func (h Home) manifest() wire.Manifest {
	m := wire.Manifest{Protocol: wire.Version}
	for _, r := range h.Holds {
		m.Rules = append(m.Rules, wire.RuleInfo{Name: r.Name, Kind: "hold", Safety: r.Priority == Safety, Latch: r.Latch})
	}
	for _, r := range h.Moments {
		m.Rules = append(m.Rules, wire.RuleInfo{Name: r.Name, Kind: "moment", Safety: r.safety()})
	}
	for _, r := range h.Flows {
		m.Rules = append(m.Rules, wire.RuleInfo{Name: r.Name, Kind: "flow", Safety: r.Priority == Safety})
	}
	return m
}

// lateness is how late a time of day may fire. A negative Late isn't valid, which the engine
// rejects.
func lateness(late time.Duration, safety bool) time.Duration {
	switch {
	case safety:
		return neverLate
	case late == 0:
		return DefaultLate
	}
	return late
}

// neverLate is as long as a Safety rule's missed time of day counts for.
const neverLate = 100 * 365 * 24 * time.Hour

func trigger(on []Change, at []string, cond Condition, late time.Duration, safety bool) engine.Trigger {
	t := engine.Trigger{Condition: cond.toEngine(), StartingDeadline: lateness(late, safety)}
	for _, c := range on {
		t.Changes = append(t.Changes, engine.Change(c))
	}
	for _, a := range at {
		t.Timers = append(t.Timers, engine.At{Time: a})
	}
	return t
}

func (r Flow) toFlow(key types.NamespacedName) *flow.Flow {
	f := &flow.Flow{Key: key, Safety: r.Priority == Safety, Late: r.Late}
	if r.Run != nil {
		f.Body = func(run *flow.Run) error { return r.Run(&FlowRun{run: run, priority: r.Priority}) }
	}
	return f
}

func (r Flow) toEngine(f *flow.Flow) engine.Flow {
	e := engine.Flow{Trigger: trigger(r.On, r.At, r.If, r.Late, f.Safety)}
	if r.Restart {
		e.Mode = engine.ModeRestart
	}
	if f.Body != nil {
		e.Body = f.Start
	}
	return e
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

// safety reports whether the moment has a Safety set.
func (r Moment) safety() bool {
	return slices.ContainsFunc(r.Do, func(a Action) bool {
		s, ok := a.(Set)
		return ok && s.Priority == Safety
	})
}

func (r Moment) toEngine() engine.Moment {
	m := engine.Moment{Trigger: trigger(r.On, r.At, r.If, r.Late, r.safety())}
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

// Run runs the home and doesn't return. The host starts this binary as "<binary> automations", and
// as "<binary> check" to judge a new version: that loads the home and prints what it finds, with no
// writes and no session. Anything else is an error, because the home has nothing to do without the
// host.
func Run(h Home) {
	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(h, log); err != nil {
		log.Error(err, "Stopped automations process")
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
