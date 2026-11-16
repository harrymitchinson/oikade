// Package oikade is how a home is written in Go. A home is a main package that decides what sets
// each capability, and hands the home to Run:
//
//	func main() {
//		home := oikade.NewHome(oikade.Options{})
//		home.Decide(BathroomFan.On).
//			Case("humid", true, BathroomSensor.Humidity.Above(70)).Linger(5 * time.Minute).
//			Default(false)
//		oikade.Run(home)
//	}
//
// BathroomFan and BathroomSensor come from devices.go, which oikade types writes from the home's
// devices, with a Bool or a Number for each capability each one has.
//
// A flow is ordinary Go that can wait for hours and survive a crash: docs/writing-flows.md.
//
// The host runs that binary and restarts it if it dies. State that must survive lives in the host,
// so a crash here loses nothing. docs/architecture.md says how.
package oikade

import (
	"context"
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
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/matterbridge"
	"github.com/harrymitchinson/oikade/internal/schedule"
	"github.com/harrymitchinson/oikade/internal/secretuse"
	"github.com/harrymitchinson/oikade/internal/sun"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// Snapshot is the home at one moment, which a ConditionFunc and NewBudget's function read.
type Snapshot struct {
	devices  map[string]map[string]any
	members  map[string]hub.Member
	now      time.Time
	location *time.Location
	site     *sun.Site
	// reads collects the rooms a condition reads, for the engine. Nil means nobody asked.
	reads *[]engine.RoomRead
}

// notReported ends a condition that read a value nobody has reported yet.
type notReported struct{ device, capability string }

// roomNotReported ends a condition that read a room where no device has reported the capability.
type roomNotReported struct{ room, capability string }

// roomEmpty ends a condition that read a room with no members in the read's group.
type roomEmpty struct{ room string }

func (s Snapshot) value(device, capability string) any {
	v, ok := s.devices[device][capability]
	if !ok {
		panic(notReported{device, capability})
	}
	return v
}

// Get reads a device's capability. A condition reading a value nobody has reported yet stops there:
// a hold keeps its state, and a moment doesn't run. So does one with a mistake in it, such as a
// device that doesn't exist, which is logged. Lookup carries on instead.
func (s Snapshot) Get[T Value](r Ref[T]) T {
	h := r.handle()
	return as[T](h.device, h.capability, s.value(h.device, h.capability))
}

// Lookup reads a device's capability, and reports false if nobody has reported it yet, so that a
// condition can carry on without it:
//
//	if v, ok := s.Lookup(CellarSensor.Humidity); ok && v > 65 {
func (s Snapshot) Lookup[T Value](r Ref[T]) (T, bool) {
	h := r.handle()
	values, declared := s.devices[h.device]
	if !declared {
		panic(fmt.Sprintf("no device %s", h.device))
	}
	v, ok := values[h.capability]
	if !ok {
		var zero T
		return zero, false
	}
	return as[T](h.device, h.capability, v), true
}

// Is reports whether c is true now, as a condition of a hold or a moment would be. A condition that
// reads a value nobody has reported yet stops the ConditionFunc there too.
func (s Snapshot) Is(c Condition) bool {
	if c == nil {
		panic("Is needs a condition")
	}
	return c.holds(s)
}

func as[T Value](device, capability string, v any) T {
	t, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("%s %s isn't a %T", device, capability, t))
	}
	return t
}

// Any reports whether the capability is true on any of the room's devices that have reported it.
//
//	s.Any(oikade.InRoom("hallway", oikade.Everything, oikade.Occupied))
//
// A room read, like Any, All, Count and Readings, covers the devices in the room now, in its group,
// and ignores those that haven't reported the capability. If none has, the condition
// stops there, as at a Get. A room with no members stops it too, and a hold keeps its state, so it's
// logged at Warn.
func (s Snapshot) Any(r Room[bool]) bool { return s.Count(r) > 0 }

// All reports whether the capability is true on all of the room's devices that have reported it.
func (s Snapshot) All(r Room[bool]) bool { return !slices.Contains(roomValues(s, r), false) }

// Count is how many of the room's devices that have reported the capability have it true.
func (s Snapshot) Count(r Room[bool]) int {
	n := 0
	for _, v := range roomValues(s, r) {
		if v {
			n++
		}
	}
	return n
}

// Readings are the capability's values on the room's devices that have reported it, in device name
// order, for slices.Max or an average.
//
//	slices.Max(s.Readings(oikade.InRoom("cellar", oikade.Everything, oikade.Humidity)))
func (s Snapshot) Readings(r Room[float64]) []float64 { return roomValues(s, r) }

// roomValues are the reported values of a room's capability, in device name order.
func roomValues[T Value](s Snapshot, r Room[T]) []T {
	room, c, types := r.room, r.capability, r.engineTypes()
	var names []string
	for name, m := range s.members {
		// A device with no room is in none, as for a room's writes and triggers.
		if room != "" && m.Room == room && m.OfTypes(types) {
			names = append(names, name)
		}
	}
	// A device with no room is in none, so "" is a mistake, not an empty room.
	if room != "" {
		if s.reads != nil {
			*s.reads = append(*s.reads, engine.RoomRead{Room: room, Types: types})
		}
		if len(names) == 0 {
			panic(roomEmpty{room})
		}
	}
	slices.Sort(names)
	var out []T
	for _, name := range names {
		if v, ok := s.devices[name][string(c)]; ok {
			out = append(out, as[T](name, string(c), v))
		}
	}
	if len(out) == 0 {
		panic(roomNotReported{room, string(c)})
	}
	return out
}

// Now is the time of this look at the home.
func (s Snapshot) Now() time.Time { return s.now }

// evaluate runs f on a snapshot of vars, and turns the panic of a read that can't be made yet into
// an error. A mistake panics on, for the engine to recover and log.
func evaluate(v engine.Vars, f func(Snapshot)) (err error) {
	devices, _ := v["devices"].(map[string]map[string]any)
	members, _ := v["members"].(map[string]hub.Member)
	now, _ := v["now"].(time.Time)
	location, _ := v["location"].(*time.Location)
	if location == nil {
		location = time.Local // as the engine's
	}
	site, _ := v["site"].(*sun.Site)
	reads, _ := v[engine.VarRoomReads].(*[]engine.RoomRead)
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		// Waiting for a device to report is normal. Anything else is a bug: the engine recovers it,
		// and logs it where a person will see it.
		switch n := r.(type) {
		case notReported:
			if _, declared := devices[n.device]; declared {
				err = fmt.Errorf("%s %s hasn't reported", n.device, n.capability)
				return
			}
			panic(fmt.Sprintf("no device %s", n.device))
		case roomNotReported:
			err = fmt.Errorf("no device in room %s has reported %s", n.room, n.capability)
			return
		case roomEmpty:
			err = engine.EmptyRoom{Room: n.room}
			return
		}
		panic(r)
	}()
	f(Snapshot{devices: devices, members: members, now: now, location: location, site: site, reads: reads})
	return nil
}

// Time is a time of day, in At and Daily. Clock, Sunrise and Sunset make one.
type Time struct {
	clock string
	// sun is the event of a sunrise or sunset, and zero for a clock time.
	sun    sun.Event
	offset time.Duration
}

// Clock is a wall-clock time, "HH:MM" from 00:00 to 23:59, in the host's time zone. A time that
// isn't fails oikade check.
func Clock(hhmm string) Time { return Time{clock: hhmm} }

// Sunrise is the day's sunrise at the home's location, moved by offset: -20*time.Minute is 20 minutes
// before. The offset is at most 6 hours either way. A day the sun doesn't rise, it doesn't happen.
func Sunrise(offset time.Duration) Time { return Time{sun: sun.Sunrise, offset: offset} }

// Sunset is the day's sunset at the home's location, moved by offset, as Sunrise is.
func Sunset(offset time.Duration) Time { return Time{sun: sun.Sunset, offset: offset} }

// isSun reports whether it's a sunrise or sunset.
func (t Time) isSun() bool { return t.sun != 0 }

// timer is the engine's time. A zero Time is none, which the engine rejects.
func (t Time) timer() engine.Timer {
	switch {
	case t.isSun():
		return engine.SunTime{Event: t.sun, Offset: t.offset}
	case t == (Time{}):
		return nil
	}
	return engine.At{Time: t.clock}
}

// needsLocation says, for a rule with these times, why it can't run in a home without a location.
func needsLocation(o Options, times ...Time) error {
	if o.site() == nil && slices.ContainsFunc(times, Time.isSun) {
		return errors.New("a sunrise or sunset needs the home's location: set Location in Options with oikade.NewLocation")
	}
	return nil
}

// once records which of a rule's single-valued options have been given, so that compile can report
// one given twice. A method on a rule returns a copy, so what's here is clipped before it's added
// to, and two chains from one rule stay separate.
type once struct {
	given, twice []string
}

func (o once) note(option string) once {
	if !slices.Contains(o.given, option) {
		o.given = append(slices.Clip(o.given), option)
	} else if !slices.Contains(o.twice, option) {
		o.twice = append(slices.Clip(o.twice), option)
	}
	return o
}

func (o once) err() error {
	errs := make([]error, 0, len(o.twice))
	for _, option := range o.twice {
		errs = append(errs, fmt.Errorf("%s is given twice, and takes one", option))
	}
	return errors.Join(errs...)
}

// Budget places a number of minutes a day in the best slots. NewBudget and NewBandedBudget make one.
type Budget struct {
	planningTime string
	slotMinutes  int
	minutes      func(Snapshot) int
	bands        *Bands
	prefer       []prefer
	forbid       []Hours
	once         once
}

// prefer weights the slots in some hours.
type prefer struct {
	hours Hours
	score int
}

// NewBudget works out the day's minutes at the planning time. A device that hasn't reported, or a
// panic, makes it zero for the day, and that's logged.
func NewBudget(minutes func(Snapshot) int) Budget { return Budget{minutes: minutes} }

// NewBandedBudget picks the day's minutes from a reading.
func NewBandedBudget(bands Bands) Budget { return Budget{bands: &bands} }

// PlanningTime starts each planning day, as "HH:MM". Without it, or if empty, that's midnight.
func (b Budget) PlanningTime(hhmm string) Budget {
	b.planningTime = hhmm
	b.once = b.once.note("a budget's PlanningTime")
	return b
}

// SlotMinutes is the length of the slots the day is planned in: 15, 30 or 60. Without it, or at
// zero, 30.
func (b Budget) SlotMinutes(minutes int) Budget {
	b.slotMinutes = minutes
	b.once = b.once.note("a budget's SlotMinutes")
	return b
}

// Prefer weights the slots in some hours, from Daily, adding to any others.
func (b Budget) Prefer(h Hours, score int) Budget {
	b.prefer = append(slices.Clip(b.prefer), prefer{hours: h, score: score})
	return b
}

// Forbid rules out the slots in these hours, from Daily, adding to any others. It beats a Prefer.
func (b Budget) Forbid(hours ...Hours) Budget {
	b.forbid = append(slices.Clip(b.forbid), hours...)
	return b
}

// Bands picks a day's minutes from the band a reading falls in.
type Bands struct {
	// Reading is what picks the band.
	Reading Number
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

// Moment runs its actions when a trigger fires and If, if given, is true. It needs a trigger: On, or
// a time of day in At, such as Clock("23:30"). NewMoment makes one.
type Moment struct {
	name    string
	on      []Change
	at      []Time
	cond    Condition
	restart bool
	late    time.Duration
	do      []Action
	once    once
}

// NewMoment runs the actions in order. Name it once.
func NewMoment(name string, do ...Action) Moment {
	return Moment{name: name, do: slices.Clone(do)}
}

// On adds changes that trigger the moment.
func (r Moment) On(changes ...Change) Moment {
	r.on = append(slices.Clip(r.on), changes...)
	return r
}

// At adds times of day that trigger the moment.
func (r Moment) At(times ...Time) Moment {
	r.at = append(slices.Clip(r.at), times...)
	return r
}

// If is checked when a trigger fires, and the moment runs only if it's true.
func (r Moment) If(c Condition) Moment {
	r.cond = c
	r.once = r.once.note("If")
	return r
}

// Restart cancels a run in progress on a new trigger. Without it, the trigger is ignored.
func (r Moment) Restart() Moment {
	r.restart = true
	r.once = r.once.note("Restart")
	return r
}

// Late is how late a time in At may still fire, as when no process was running. Without it, or at
// zero, that's DefaultLate. A moment with a Safety Set fires however late it is.
func (r Moment) Late(d time.Duration) Moment {
	r.late = d
	r.once = r.once.note("Late")
	return r
}

// DefaultLate is how late a flow may wake, or a time of day fire, and still run.
const DefaultLate = 10 * time.Minute

// Flow runs its function when a trigger fires and If, if given, is true, like a moment. The function
// is ordinary Go, and survives a crash at any point. It must stick to the steps on its FlowRun for
// anything that happens outside it or varies between runs, because after a crash it runs again from
// the top and recorded steps return what they returned the first time. docs/writing-flows.md says
// how. NewFlow makes one.
type Flow struct {
	name    string
	on      []Change
	at      []Time
	cond    Condition
	restart bool
	late    time.Duration
	run     func(*FlowRun) error
	once    once
}

// NewFlow runs run on each trigger. Name it once.
func NewFlow(name string, run func(*FlowRun) error) Flow { return Flow{name: name, run: run} }

// On adds changes that trigger the flow.
func (r Flow) On(changes ...Change) Flow {
	r.on = append(slices.Clip(r.on), changes...)
	return r
}

// At adds times of day that trigger the flow.
func (r Flow) At(times ...Time) Flow {
	r.at = append(slices.Clip(r.at), times...)
	return r
}

// If is checked when a trigger fires, and the flow runs only if it's true.
func (r Flow) If(c Condition) Flow {
	r.cond = c
	r.once = r.once.note("If")
	return r
}

// Restart cancels a run in progress on a new trigger. Without it, the trigger is ignored.
func (r Flow) Restart() Flow {
	r.restart = true
	r.once = r.once.note("Restart")
	return r
}

// Late is how late a run may wake and still carry on. Without it, or at zero, that's DefaultLate. A
// run later than that gives up and logs it, rather than turn the lights off at 09:00.
func (r Flow) Late(d time.Duration) Flow {
	r.late = d
	r.once = r.once.note("Late")
	return r
}

// FlowRun is one run of a flow. Its steps are written to the host's disk before the flow continues.
type FlowRun struct {
	run *flow.Run
	// decided are the home's decisions, which say where a Set ranks.
	decided []compiledDecision
	// options are the home's, which a condition is checked against when the flow uses it.
	options Options
}

// checked checks a condition a flow uses, which oikade check can't see, and ends the run as failed
// if it has a mistake in it.
func (r *FlowRun) checked(step string, c Condition) {
	if c == nil {
		panic(step + " needs a condition")
	}
	if err := c.check(r.options); err != nil {
		panic(fmt.Sprintf("%s: %v", step, err))
	}
}

// Now is the time. It returns the same time when a flow replays, which time.Now wouldn't.
func (r *FlowRun) Now() time.Time { return r.run.Now() }

// Sleep waits. A restart doesn't move the time it ends.
func (r *FlowRun) Sleep(d time.Duration) { r.run.Sleep(d) }

// Get reads a capability now, such as Hallway.Occupied. A replay returns what it read the first
// time. A value nobody has reported yet ends the run as failed, and that's logged.
func (r *FlowRun) Get[T Value](c Ref[T]) T {
	h := c.handle()
	var v T
	err := r.run.Look(h.device+" "+h.capability, &v, func(vars engine.Vars) (bool, error) {
		return true, evaluate(vars, func(s Snapshot) { v = s.Get(c) })
	})
	if err != nil {
		panic(fmt.Sprintf("Get: %v", err))
	}
	return v
}

// Is reports whether c is true now, such as oikade.Daily(oikade.Sunset(0), oikade.Sunrise(0)) for
// whether it's dark. A replay returns what it found the first time. A condition that reads a value
// nobody has reported yet is false. One with a mistake in it ends the run as failed.
func (r *FlowRun) Is(c Condition) bool {
	r.checked("Is", c)
	var ok bool
	err := r.run.Look("is "+label(c), &ok, func(vars engine.Vars) (bool, error) {
		if evaluate(vars, func(s Snapshot) { ok = c.holds(s) }) != nil {
			ok = false // Not reported yet.
		}
		return true, nil
	})
	if err != nil {
		panic(fmt.Sprintf("Is: %v", err))
	}
	return ok
}

// WaitFor waits until c is true and returns true, or returns false once timeout has passed. Zero
// means no limit. A restart doesn't move the time it ends.
func (r *FlowRun) WaitFor(c Condition, timeout time.Duration) bool {
	r.checked("WaitFor", c)
	return r.run.WaitFor(toEngine(c), timeout)
}

// Set writes a capability, such as Hallway.On.To(true), or claims it if it's arbitrated, which
// needs For, UntilReleased or UntilNext, and is an error without one. It may run more than once,
// after a crash, so set a value and never toggle one.
func (r *FlowRun) Set(s Set) error {
	if err := needsFlowsLine(r.decided, s.device, s.capability); err != nil {
		return err
	}
	return r.run.Apply(s.toEngine(r.decided))
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
	toEngine(decided []compiledDecision) engine.Action
}

// Set writes a capability, from a Bool's, a Number's or InRoom's To. For, UntilReleased and UntilNext make it a
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

// toEngine is the engine's Set, ranked where the capability's decision puts flows.
func (s Set) toEngine(decided []compiledDecision) engine.Action {
	return engine.Set{
		Device: s.device, Room: s.room, DeviceTypes: s.types, Capability: s.capability, Value: s.value, For: s.claimFor,
		Until: s.until, Priority: claimPriority(decided, s.device, s.capability),
	}
}

// Wait waits before the next action.
type Wait time.Duration

func (w Wait) toEngine([]compiledDecision) engine.Action { return engine.Delay(w) }

// Release ends the named rule's claims, and its latch if it's a hold.
type Release string

func (r Release) toEngine([]compiledDecision) engine.Action { return engine.Release(r) }

// Options is what a home has exactly one of. A zero Options is valid.
type Options struct {
	// Location is where the home is, for Sunrise and Sunset. A home without them needs none.
	Location Location
	// MatterName is the Matter bridge's name, as a controller such as the Home app first shows it:
	// at most 32 bytes. Empty is "Oikade". A deploy renames the bridge in place, with no pairing
	// again.
	MatterName string
}

func (o Options) check() error {
	var name error
	if err := matterbridge.CheckBridgeName(o.MatterName); err != nil {
		name = fmt.Errorf("MatterName: %w", err)
	}
	return errors.Join(o.Location.check(), name)
}

// site is the home's location for the engine, and nil if it has none.
func (o Options) site() *sun.Site { return o.Location.site() }

// Location is where a home is. NewLocation makes one, and a zero Location is none.
type Location struct {
	latitude, longitude float64
	set                 bool
}

// NewLocation is a place, in degrees. North and east are positive, and 0 is a coordinate like any
// other. Two decimals, about a kilometre, move sun times by seconds, so round to that.
func NewLocation(latitude, longitude float64) Location {
	return Location{latitude: latitude, longitude: longitude, set: true}
}

func (l Location) check() error {
	switch {
	case !l.set:
		return nil
	case !(l.latitude >= -90 && l.latitude <= 90):
		return fmt.Errorf("the home's latitude %v isn't between -90 and 90", l.latitude)
	case !(l.longitude >= -180 && l.longitude <= 180):
		return fmt.Errorf("the home's longitude %v isn't between -180 and 180", l.longitude)
	}
	return nil
}

func (l Location) site() *sun.Site {
	if !l.set {
		return nil
	}
	return &sun.Site{Latitude: l.latitude, Longitude: l.longitude}
}

// Part is a piece of a home: a Moment, Flow, Plugin, or a Bundle of them. A function
// can return one, so that what belongs together stays together. Only this package adds kinds.
//
// A part keeps the names written in it, whichever Bundle it's in. A name is a rule's state key, so a
// helper used twice builds a different name each time.
type Part interface {
	part()
}

func (Moment) part() {}
func (Flow) part()   {}
func (Plugin) part() {}
func (Bundle) part() {}

// Bundle is parts that belong together, such as a flow and the moment that starts it. NewBundle
// makes one. It's a Part itself, so bundles nest, and an empty one is fine.
type Bundle struct {
	parts []Part
}

// NewBundle groups parts.
func NewBundle(parts ...Part) Bundle {
	return Bundle{parts: slices.Clone(parts)}
}

// Home is a home's automations. NewHome makes one.
type Home struct {
	options   Options
	decisions []decider
	moments   []Moment
	flows     []Flow
	plugins   []Plugin
	// mistakes are the parts NewHome couldn't take, which compile reports.
	mistakes []error
}

// NewHome makes a home from its parts, which Decide adds to. A nil part, or a pointer to one, fails
// oikade check.
func NewHome(o Options, parts ...Part) *Home {
	h := &Home{options: o}
	h.add("parts", parts)
	return h
}

// add sorts parts by kind, in the order they're written, with path saying where they came from.
func (h *Home) add(path string, parts []Part) {
	for i, p := range parts {
		at := fmt.Sprintf("%s[%d]", path, i)
		switch p := p.(type) {
		case Moment:
			h.moments = append(h.moments, p)
		case Flow:
			h.flows = append(h.flows, p)
		case Plugin:
			h.plugins = append(h.plugins, p)
		case Bundle:
			h.add(at, p.parts)
		case nil:
			h.mistakes = append(h.mistakes, fmt.Errorf("a nil part, at %s: give a rule or a Bundle", at))
		default:
			h.mistakes = append(h.mistakes,
				fmt.Errorf("a %T, at %s: a part is a Moment, Flow, Plugin or Bundle, written as a value", p, at))
		}
	}
}

// Plugin is a plugin your home imports. It's a Go module like any other, and it runs as you, so
// importing one means trusting its code. The host runs each in a process of its own, so one that
// panics or hangs is restarted while automations and Matter carry on. NewPlugin makes one.
type Plugin struct {
	name    string
	factory plugin.Factory
}

// NewPlugin imports a plugin. name is what its devices are recorded under: keep it, because changing
// it makes them new hardware. factory builds the plugin, in its own process, never in your automations.
func NewPlugin(name string, factory plugin.Factory) Plugin {
	return Plugin{name: name, factory: factory}
}

// compile checks the home and turns it into the engine's rules, and returns its flows by name.
func (h *Home) compile() ([]*engine.Compiled, map[string]*flow.Flow, error) {
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
		return ident.ID{Name: n}
	}
	add := func(rule string, c *engine.Compiled, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", rule, err))
			return
		}
		rules = append(rules, c)
	}
	decided, err := h.compileDecisions()
	errs = append(errs, h.mistakes...)
	errs = append(errs, err, h.options.check())
	for _, d := range decided {
		for _, c := range d.cases {
			key := name(c.key)
			r, err := c.rule()
			if err == nil {
				r.Key = key
			}
			add(c.key, r, err)
		}
	}
	for _, r := range h.moments {
		c, err := engine.NewMoment(name(r.name), r.toEngine(decided))
		add(r.name, c, errors.Join(err, r.once.err(), nilChange(r.on), needsLocation(h.options, r.at...),
			checkCondition(r.cond, h.options), r.checkSets(decided)))
	}
	for _, r := range h.flows {
		f := r.toFlow(name(r.name), h.options, decided)
		flows[r.name] = f
		c, err := engine.NewFlow(f.Key, r.toEngine(f))
		add(r.name, c, errors.Join(err, r.once.err(), nilChange(r.on), needsLocation(h.options, r.at...),
			checkCondition(r.cond, h.options)))
	}
	plugins := map[string]bool{}
	for _, p := range h.plugins {
		switch {
		case !contractv1.ValidID(p.name):
			errs = append(errs, fmt.Errorf("plugin name %q isn't valid", p.name))
		case plugins[p.name]:
			errs = append(errs, fmt.Errorf("plugin %q is declared twice", p.name))
		case p.factory == nil:
			errs = append(errs, fmt.Errorf("plugin %s has no factory", p.name))
		}
		plugins[p.name] = true
	}
	return rules, flows, errors.Join(errs...)
}

// settings checks the home's decisions and turns them into what the host takes.
func (h *Home) settings() ([]wire.DeviceControl, error) {
	decided, err := h.compileDecisions()
	return settingsOf(decided), err
}

// manifest describes the home to the host, which judges a new version by it.
func (h *Home) manifest() wire.Manifest {
	m := wire.Manifest{Protocol: wire.Version}
	// A home with a mistake in its decisions doesn't compile, and never gets this far.
	decided, _ := h.compileDecisions()
	for _, d := range decided {
		for _, c := range d.cases {
			m.Rules = append(m.Rules, wire.RuleInfo{
				Name: c.key, Kind: wire.KindCase, Safety: c.safety, Latch: c.latch,
				Device: d.device, Capability: d.capability, Case: c.name,
			})
		}
		m.Warnings = append(m.Warnings, d.warnings...)
	}
	for _, r := range h.moments {
		m.Rules = append(m.Rules, wire.RuleInfo{Name: r.name, Kind: "moment", Safety: r.safety(decided)})
	}
	for _, r := range h.flows {
		m.Rules = append(m.Rules, wire.RuleInfo{Name: r.name, Kind: wire.KindFlow})
	}
	for _, p := range h.plugins {
		m.Plugins = append(m.Plugins, p.name)
	}
	m.Control = settingsOf(decided)
	m.Secrets = secretuse.Names()
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
	t := engine.Trigger{Condition: toEngine(cond), StartingDeadline: lateness(late, safety)}
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

func (r Flow) toFlow(key ident.ID, o Options, decided []compiledDecision) *flow.Flow {
	// Until a decision can name the flows it ranks, any flow may make a Safety claim where a Flows
	// line is above the override line, so every flow is Safety then: it acts however late it is.
	f := &flow.Flow{Key: key, Late: r.late, Safety: slices.ContainsFunc(decided, func(d compiledDecision) bool {
		return d.flows && d.flowsSafety
	})}
	if r.run != nil {
		f.Body = func(run *flow.Run) error { return r.run(&FlowRun{run: run, decided: decided, options: o}) }
	}
	return f
}

func (r Flow) toEngine(f *flow.Flow) engine.Flow {
	e := engine.Flow{Trigger: trigger(r.on, r.at, r.cond, r.late, f.Safety)}
	if r.restart {
		e.Mode = engine.ModeRestart
	}
	if f.Body != nil {
		e.Body = f.Start
	}
	return e
}

// checkBudget checks a budget line against its decision, which the engine can't see until the
// device has reported, so that a mistake here fails oikade check rather than today's plan.
func checkBudget(b *Budget, h handle, value any, entry control.Entry) error {
	if err := b.clockOnly(); err != nil {
		return err
	}
	spec := *b.toSchedule()
	spec.Device, spec.Capability, spec.Value = h.device, h.capability, value
	_, err := schedule.CompileSpec(spec, &entry)
	return err
}

func (b *Budget) toSchedule() *schedule.Spec {
	if b == nil {
		return nil
	}
	s := &schedule.Spec{PlanningTime: b.planningTime, SlotMinutes: b.slotMinutes}
	if m := b.minutes; m != nil {
		s.Minutes = func(vars map[string]any, _ time.Time, _ *time.Location) (minutes int, err error) {
			// The engine plans holding its lock, so a mistake here must end as a failed day.
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			err = evaluate(vars, func(st Snapshot) { minutes = m(st) })
			return minutes, err
		}
	}
	if in := b.bands; in != nil {
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
	for _, p := range b.prefer {
		s.Prefer = append(s.Prefer, schedule.PreferSpec{Window: p.hours.toSchedule(), Score: p.score})
	}
	for _, f := range b.forbid {
		s.Forbid = append(s.Forbid, f.toSchedule())
	}
	return s
}

// clockOnly rejects a sunrise or sunset in a budget's windows, which are planned in slots.
func (b *Budget) clockOnly() error {
	sunEdged := func(h Hours) bool { return h.start.isSun() || h.end.isSun() }
	preferred := func(p prefer) bool { return sunEdged(p.hours) }
	if slices.ContainsFunc(b.forbid, sunEdged) || slices.ContainsFunc(b.prefer, preferred) {
		return errors.New("a budget's windows take clock times only, not a sunrise or sunset")
	}
	return nil
}

func (h Hours) toSchedule() schedule.WindowSpec {
	return schedule.WindowSpec{Start: h.start.clock, End: h.end.clock, Days: h.days}
}

// checkSets refuses a Set on a decided capability whose decision has no Flows line.
func (r Moment) checkSets(decided []compiledDecision) error {
	var errs []error
	for _, a := range r.do {
		if s, ok := a.(Set); ok && s.device != "" {
			errs = append(errs, needsFlowsLine(decided, s.device, s.capability))
		}
	}
	return errors.Join(errs...)
}

// safety reports whether the moment sets a capability whose Flows line is a safety line.
func (r Moment) safety(decided []compiledDecision) bool {
	return slices.ContainsFunc(r.do, func(a Action) bool {
		s, ok := a.(Set)
		return ok && s.device != "" && claimPriority(decided, s.device, s.capability) == engine.PrioritySafety
	})
}

func (r Moment) toEngine(decided []compiledDecision) engine.Moment {
	m := engine.Moment{Trigger: trigger(r.on, r.at, r.cond, r.late, r.safety(decided))}
	if r.restart {
		m.Mode = engine.ModeRestart
	}
	for _, a := range r.do {
		if a == nil {
			m.Do = append(m.Do, nil)
			continue
		}
		m.Do = append(m.Do, a.toEngine(decided))
	}
	return m
}

// Run runs the home and doesn't return. The host starts this binary as "<binary> automations", and
// as "<binary> check" to judge a new version: that loads the home and prints what it finds, with no
// writes and no session. Anything else is an error, because the home has nothing to do without the
// host.
func Run(h *Home) {
	log := slog.New(slog.NewJSONHandler(logOutput(), nil))
	if err := run(h, log); err != nil {
		log.Error("Stopped process", "err", err)
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
