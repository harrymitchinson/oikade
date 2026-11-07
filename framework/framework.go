// Package framework is how a home is written in Go. A home is a main package that declares its
// holds and moments and hands them to Run:
//
//	func main() {
//		framework.Run(framework.Home{
//			Holds: []framework.Hold{{
//				Name: "bathroom-fan",
//				Set:  BathroomFan.On.To(true),
//				When: func(s framework.State) bool {
//					return s.Get(BathroomSensor.Humidity) > 70
//				},
//			}},
//		})
//	}
//
// BathroomFan and BathroomSensor come from devices.go, which oikade types writes from the home's
// devices, with a Handle for each capability each one has.
//
// A flow is ordinary Go that can wait for hours and survive a crash: docs/writing-flows.md.
//
// The host runs that binary and restarts it if it dies. State that must survive lives in the host,
// so a crash here loses nothing. docs/architecture.md says how.
package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/schedule"
	"github.com/harrymitchinson/oikade/sdk/plugin"
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

// Get reads a device's capability. A condition reading a value nobody has reported yet stops there:
// a hold keeps its state, and a moment doesn't run. So does one with a mistake in it, such as a
// device that doesn't exist, which is logged.
func (s State) Get[T Value](h Handle[T]) T {
	v, ok := s.value(h.Device, string(h.Capability)).(T)
	if !ok {
		panic(fmt.Sprintf("%s %s isn't a %T", h.Device, h.Capability, v))
	}
	return v
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

// Time is a time of day, in At and a Window. Clock makes one.
type Time struct {
	clock string
}

// Clock is a wall-clock time, "HH:MM" from 00:00 to 23:59, in the host's time zone. A time that
// isn't fails oikade check.
func Clock(hhmm string) Time { return Time{clock: hhmm} }

// timer is the engine's time. A zero Time is none, which the engine rejects.
func (t Time) timer() engine.Timer {
	if t == (Time{}) {
		return nil
	}
	return engine.At{Time: t.clock}
}

// Window is when a hold may be active, from Start until the next End, starting on Days or on any
// day. An End at or before its Start is the next day.
type Window struct {
	Start, End Time
	Days       []time.Weekday
}

// Hold keeps a capability at a value while its Window is open and When is true. It needs at least
// one of them, or a Budget on its own. Name it once: its state is kept by name.
type Hold struct {
	Name string
	// Set is what the hold keeps, such as PorchLight.On.To(true), or InRoom's. The hold's
	// Priority is its own, so the Set takes no For, Until or Priority.
	Set      Set
	Priority Priority
	Window   *Window
	When     Condition
	// OnDelay and OffDelay are how long When must stay true or false first.
	OnDelay, OffDelay time.Duration
	// Latch keeps the hold active once it starts, until a Release.
	Latch bool
	// Budget plans when the hold is active each day. It holds one device's Bool at true, which needs
	// a Control, whose limits it plans around.
	Budget *Budget
}

// Budget places a number of minutes a day in the best slots.
type Budget struct {
	// PlanningTime starts each planning day, as "HH:MM". Empty means midnight.
	PlanningTime string
	// SlotMinutes is the length of the slots the day is planned in: 15, 30 or 60. Zero means 30.
	SlotMinutes int
	// Minutes works out the day's minutes at PlanningTime. A device that hasn't reported, or a panic,
	// makes it zero for the day, and that's logged. Set this or Bands.
	Minutes func(State) int
	// Bands picks the day's minutes from a reading. Set this or Minutes.
	Bands *Bands
	// Prefer weights slots. Forbid rules them out, and beats a Prefer.
	Prefer []Prefer
	Forbid []Window
}

// Prefer weights the slots in a window.
type Prefer struct {
	Window Window
	Score  int
}

// Bands picks a day's minutes from the band a reading falls in.
type Bands struct {
	// Reading is what picks the band.
	Reading Handle[float64]
	// MaxAge is how old the reading may be. Zero means 30 minutes.
	MaxAge time.Duration
	// Missing is the minutes when there's no reading that recent.
	Missing int
	// Ranges ascend by Below, and the first the reading is below wins.
	Ranges []Band
	// Otherwise is the minutes for a reading above them all.
	Otherwise int
}

// Band is the minutes for readings below Below.
type Band struct {
	Below   float64
	Minutes int
}

// Resolve picks between claims at the same level.
type Resolve string

const (
	// PreferOn makes on win, for a bool capability.
	PreferOn Resolve = "PreferOn"
	// PreferOff makes off win, for a bool capability.
	PreferOff Resolve = "PreferOff"
	// Highest makes the highest win, for a number capability.
	Highest Resolve = "Highest"
	// Lowest makes the lowest win, for a number capability.
	Lowest Resolve = "Lowest"
)

// External says what a change from outside your automations does, such as a tap in the Home app.
type External string

const (
	// ExternalOverride holds the change as an Override claim, for OverrideHold.
	ExternalOverride External = "Override"
	// ExternalRevert puts the capability back.
	ExternalRevert External = "Revert"
)

// Control arbitrates a capability of a device: rules claim it rather than write it, and the host
// picks the winner, including when your automations aren't running.
type Control struct {
	// Default is the capability, at its value with no claims: Heater.On.To(false). It names a
	// device, not a room, and takes no For, Until or Priority.
	Default  Set
	Resolve  Resolve
	External External
	// OverrideHold is how long an ExternalOverride lasts, unless OverrideUntilNext.
	OverrideHold time.Duration
	// OverrideUntilNext ends an ExternalOverride when the value without it next changes.
	OverrideUntilNext bool
	// MinimumOn and MinimumOff are the shortest run and rest. MaximumOn stops a longer run, and then
	// rests for MinimumOff, or until nothing wants it on.
	MinimumOn, MaximumOn, MinimumOff time.Duration
	// MaximumStartsPerDay limits off-to-on transitions. Zero means unlimited.
	MaximumStartsPerDay int
}

func (c Control) entry() (control.Entry, error) {
	if err := c.Default.plain("a Control's Default", true); err != nil {
		return control.Entry{}, err
	}
	raw, err := json.Marshal(c.Default.value)
	if err != nil {
		return control.Entry{}, fmt.Errorf("default: %w", err)
	}
	e := control.Entry{
		Capability: c.Default.capability, Default: raw, Resolve: string(c.Resolve), External: string(c.External),
		MinimumOn: control.Duration(c.MinimumOn), MaximumOn: control.Duration(c.MaximumOn),
		MinimumOff: control.Duration(c.MinimumOff), MaximumStartsPerDay: c.MaximumStartsPerDay,
	}
	switch {
	case c.OverrideUntilNext:
		e.OverrideHold = control.NameNext
	case c.OverrideHold != 0:
		e.OverrideHold = c.OverrideHold.String()
	}
	return e, nil
}

// Moment runs Do when a trigger fires and If, if set, is true. It needs a trigger: a Change, or a
// time of day in At, such as Clock("23:30").
type Moment struct {
	Name string
	On   []Change
	At   []Time
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
	At   []Time
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

// Set writes a capability, such as Hallway.On.To(true), or claims it if it's arbitrated, which
// needs For, UntilReleased or UntilNext, and is an error without one. It may run more than once,
// after a crash, so set a value and never toggle one.
func (r *FlowRun) Set(s Set) error {
	if s.priority == Normal {
		s.priority = r.priority
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

// Set writes a capability, from a Handle's To or InRoom's. For, UntilReleased and UntilNext make it a
// claim on an arbitrated capability, which needs one of them:
//
//	Heater.On.To(true).For(time.Hour)
type Set struct {
	device, room, capability string
	// types limits a room's write to devices of these types.
	types    []devicetype.Type
	value    any
	claimFor time.Duration
	until    engine.Until
	priority Priority
}

// For ends the claim after d.
func (s Set) For(d time.Duration) Set {
	s.claimFor = d
	return s
}

// UntilReleased ends the claim at a Release. It can't be combined with UntilNext.
func (s Set) UntilReleased() Set { return s.endUntil(engine.UntilReleased) }

// UntilNext ends the claim when the value without it next changes.
func (s Set) UntilNext() Set { return s.endUntil(engine.UntilNext) }

// bothUntils is the engine's error for a Set given both: an until it doesn't know.
const bothUntils engine.Until = "UntilReleased or UntilNext, not both"

func (s Set) endUntil(u engine.Until) Set {
	if s.until != "" && s.until != u {
		u = bothUntils
	}
	s.until = u
	return s
}

// Priority is the level of the claim. Without it, a moment's Set is Normal and a flow's is the
// flow's Priority.
func (s Set) Priority(p Priority) Set {
	s.priority = p
	return s
}

// plain checks a Set that's only a capability and a value, as a hold's and a Control's are, and
// one that names a device if it must.
func (s Set) plain(what string, device bool) error {
	switch {
	case s.capability == "":
		return fmt.Errorf("%s needs a capability: a handle's To", what)
	case s.claimFor != 0 || s.until != "" || s.priority != Normal:
		return fmt.Errorf("%s takes no For, Until or Priority", what)
	case device && s.room != "":
		return fmt.Errorf("%s names a device, not a room", what)
	case device && s.device == "":
		return fmt.Errorf("%s needs a device", what)
	}
	return nil
}

// roomOfTypes adds the set to rooms if it's a room's limited to device types, which the host
// checks a change of device type against.
func (s Set) roomOfTypes(rooms []wire.RoomOfTypes) []wire.RoomOfTypes {
	if s.room == "" || len(s.types) == 0 {
		return rooms
	}
	return append(rooms, wire.RoomOfTypes{Room: s.room, Capability: s.capability, DeviceTypes: slices.Clone(s.types)})
}

func (s Set) toEngine() engine.Action {
	return engine.Set{
		Device: s.device, Room: s.room, DeviceTypes: s.types, Capability: s.capability, Value: s.value, For: s.claimFor,
		Until: s.until, Priority: engine.Priority(s.priority),
	}
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
	// Control says which capabilities are arbitrated, and how.
	Control []Control
	// Plugins are the plugins your home imports.
	Plugins []Plugin
}

// Plugin is a plugin your home imports. It's a Go module like any other, and it runs as you, so
// importing one means trusting its code. The host runs each in a process of its own, so one that
// panics or hangs is restarted while automations and Matter carry on.
type Plugin struct {
	// Name is what the plugin's devices are recorded under. Keep it: changing it makes them new
	// hardware.
	Name string
	// New builds the plugin. It runs in the plugin's process, never in your automations.
	New plugin.Factory
}

// compile checks the home and turns it into the engine's rules, and returns its flows by name.
func (h Home) compile() ([]*engine.Compiled, map[string]*flow.Flow, error) {
	var rules []*engine.Compiled
	flows := map[string]*flow.Flow{}
	var errs []error
	seen := map[string]bool{}
	name := func(n string) ident.ID {
		switch {
		case n == "":
			errs = append(errs, errors.New("a rule needs a name"))
		case seen[n]:
			errs = append(errs, fmt.Errorf("rule %q is declared twice", n))
		}
		seen[n] = true
		return ident.ID{Namespace: wire.Namespace, Name: n}
	}
	add := func(rule string, c *engine.Compiled, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", rule, err))
			return
		}
		rules = append(rules, c)
	}
	settings, err := h.settings()
	errs = append(errs, err)
	for _, r := range h.Holds {
		key := name(r.Name)
		err := r.Set.plain("a hold's Set", false)
		var c *engine.Compiled
		if err == nil {
			c, err = engine.NewHold(key, r.toEngine())
		}
		if err == nil {
			err = r.checkBudget(settings)
		}
		add(r.Name, c, err)
	}
	for _, r := range h.Moments {
		c, err := engine.NewMoment(name(r.Name), r.toEngine())
		add(r.Name, c, errors.Join(err, nilChange(r.On)))
	}
	for _, r := range h.Flows {
		f := r.toFlow(name(r.Name))
		flows[r.Name] = f
		c, err := engine.NewFlow(f.Key, r.toEngine(f))
		add(r.Name, c, errors.Join(err, nilChange(r.On)))
	}
	plugins := map[string]bool{}
	for _, p := range h.Plugins {
		switch {
		case !contractv1.ValidID(p.Name):
			errs = append(errs, fmt.Errorf("plugin name %q isn't valid", p.Name))
		case plugins[p.Name]:
			errs = append(errs, fmt.Errorf("plugin %q is declared twice", p.Name))
		case p.New == nil:
			errs = append(errs, fmt.Errorf("plugin %s has no New", p.Name))
		}
		plugins[p.Name] = true
	}
	return rules, flows, errors.Join(errs...)
}

// settings checks the home's Control and turns it into what the host takes.
func (h Home) settings() ([]wire.DeviceControl, error) {
	var out []wire.DeviceControl
	var errs []error
	for _, c := range h.Control {
		device, capability := c.Default.device, c.Default.capability
		bad := func(err error) { errs = append(errs, fmt.Errorf("control %s on %s: %w", capability, device, err)) }
		if slices.ContainsFunc(out, func(o wire.DeviceControl) bool {
			return o.Device == device && o.Entry.Capability == capability
		}) {
			bad(errors.New("declared twice"))
		}
		if c.OverrideUntilNext && c.OverrideHold != 0 {
			bad(errors.New("OverrideHold and OverrideUntilNext are alternatives"))
		}
		e, err := c.entry()
		if err == nil {
			_, err = engine.ControlSettings(e, nil)
		}
		if err != nil {
			bad(err)
			continue
		}
		out = append(out, wire.DeviceControl{Device: device, Entry: e})
	}
	return out, errors.Join(errs...)
}

// manifest describes the home to the host, which judges a new version by it.
func (h Home) manifest() wire.Manifest {
	m := wire.Manifest{Protocol: wire.Version}
	for _, r := range h.Holds {
		info := wire.RuleInfo{Name: r.Name, Kind: "hold", Safety: r.Priority == Safety, Latch: r.Latch}
		if info.Safety {
			info.Rooms = r.Set.roomOfTypes(nil)
		}
		m.Rules = append(m.Rules, info)
	}
	for _, r := range h.Moments {
		info := wire.RuleInfo{Name: r.Name, Kind: "moment", Safety: r.safety()}
		for _, a := range r.Do {
			if s, ok := a.(Set); ok && s.priority == Safety {
				info.Rooms = s.roomOfTypes(info.Rooms)
			}
		}
		m.Rules = append(m.Rules, info)
	}
	for _, r := range h.Flows {
		m.Rules = append(m.Rules, wire.RuleInfo{Name: r.Name, Kind: wire.KindFlow, Safety: r.Priority == Safety})
	}
	for _, p := range h.Plugins {
		m.Plugins = append(m.Plugins, p.Name)
	}
	// A home with a mistake in its settings doesn't compile, and never gets this far.
	m.Control, _ = h.settings()
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

func trigger(on []Change, at []Time, cond Condition, late time.Duration, safety bool) engine.Trigger {
	t := engine.Trigger{Condition: cond.toEngine(), StartingDeadline: lateness(late, safety)}
	for _, c := range on {
		if c != nil { // nilChange reports it.
			t.Changes = append(t.Changes, c.change())
		}
	}
	for _, a := range at {
		t.Timers = append(t.Timers, a.timer())
	}
	return t
}

// nilChange reports a nil in a rule's On, which trigger leaves out.
func nilChange(on []Change) error {
	if i := slices.Index(on, nil); i >= 0 {
		return fmt.Errorf("a nil change, at On[%d]: give a handle, or InRoom", i)
	}
	return nil
}

func (r Flow) toFlow(key ident.ID) *flow.Flow {
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
		Device: r.Set.device, Room: r.Set.room, DeviceTypes: r.Set.types, Capability: r.Set.capability, Value: r.Set.value,
		Priority: engine.Priority(r.Priority), When: r.When.toEngine(),
		OnDelay: r.OnDelay, OffDelay: r.OffDelay, Latch: r.Latch, Budget: r.Budget.toSchedule(),
	}
	if r.Window != nil {
		h.Window = &engine.Window{
			Start: r.Window.Start.timer(), End: r.Window.End.timer(), Days: r.Window.Days,
		}
	}
	return h
}

// checkBudget checks a hold's budget against its Control, which the engine can't see until the
// device has reported, so that a mistake here fails oikade check rather than today's plan.
func (r Hold) checkBudget(settings []wire.DeviceControl) error {
	if r.Budget == nil {
		return nil
	}
	i := slices.IndexFunc(settings, func(c wire.DeviceControl) bool {
		return c.Device == r.Set.device && c.Entry.Capability == r.Set.capability
	})
	if i < 0 {
		return fmt.Errorf("a budget needs a Control for %s on %s", r.Set.capability, r.Set.device)
	}
	spec := *r.Budget.toSchedule()
	spec.Device, spec.Capability, spec.Value = r.Set.device, r.Set.capability, r.Set.value
	_, err := schedule.CompileSpec(spec, &settings[i].Entry)
	return err
}

func (b *Budget) toSchedule() *schedule.Spec {
	if b == nil {
		return nil
	}
	s := &schedule.Spec{PlanningTime: b.PlanningTime, SlotMinutes: b.SlotMinutes}
	if m := b.Minutes; m != nil {
		s.Minutes = func(devices map[string]map[string]any, now time.Time, _ *time.Location) (minutes int, err error) {
			// The engine plans holding its lock, so a mistake here must end as a failed day.
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			_, err = Condition(func(st State) bool {
				minutes = m(st)
				return true
			}).toEngine()(engine.Vars{"devices": devices, "now": now})
			return minutes, err
		}
	}
	if in := b.Bands; in != nil {
		bands := &schedule.BandSpec{
			Device: in.Reading.Device, Capability: string(in.Reading.Capability), MaxAge: in.MaxAge, MissingMinutes: in.Missing,
		}
		for _, r := range in.Ranges {
			below := r.Below
			bands.Ranges = append(bands.Ranges, schedule.Band{Below: &below, Minutes: r.Minutes})
		}
		bands.Ranges = append(bands.Ranges, schedule.Band{Minutes: in.Otherwise})
		s.Bands = bands
	}
	for _, p := range b.Prefer {
		s.Prefer = append(s.Prefer, schedule.PreferSpec{Window: p.Window.toSchedule(), Score: p.Score})
	}
	for _, f := range b.Forbid {
		s.Forbid = append(s.Forbid, f.toSchedule())
	}
	return s
}

func (w Window) toSchedule() schedule.WindowSpec {
	return schedule.WindowSpec{Start: w.Start.clock, End: w.End.clock, Days: w.Days}
}

// safety reports whether the moment has a Safety set.
func (r Moment) safety() bool {
	return slices.ContainsFunc(r.Do, func(a Action) bool {
		s, ok := a.(Set)
		return ok && s.priority == Safety
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
	log := slog.New(slog.NewJSONHandler(logOutput(), nil))
	if err := run(h, log); err != nil {
		log.Error("Stopped process", "err", err)
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
