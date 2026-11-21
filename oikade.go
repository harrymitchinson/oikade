// Package oikade is how a home is written in Go. A home is a main package that decides what sets
// each capability, adds flows that do something when a trigger fires, and runs:
//
//	func main() {
//		home := oikade.NewHome(oikade.Options{})
//		home.Decide(BathroomFan.On).
//			Case("humid", true, BathroomSensor.Humidity.Above(70)).Linger(5 * time.Minute).
//			Default(false)
//		home.Run()
//	}
//
// BathroomFan and BathroomSensor come from devices.go, which oikade types writes from the home's
// devices, with a Bool or a Number for each capability each one has.
//
// A flow, from home.Flow, is ordinary Go that can wait for hours and survive a crash:
// docs/writing-flows.md.
//
// The host runs that binary and restarts it if it dies. State that must survive lives in the host,
// so a crash here loses nothing. docs/architecture.md says how.
package oikade

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"oikade.dev/internal/control"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/flow"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/matterbridge"
	"oikade.dev/internal/schedule"
	"oikade.dev/internal/sun"
	"oikade.dev/sdk/plugin"
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

// Get reads a device's capability. If nobody has reported it yet, the condition stops there: a case
// keeps its state, and a flow's If doesn't let it run. Lookup carries on instead. A mistake stops
// the condition too, and is logged: a device that doesn't exist, or a group's capability, which
// Any, All, Count and Readings read.
func (s Snapshot) Get[T Value](r Ref[T]) T {
	h := r.handle()
	notGroup(h)
	return as[T](h.device, h.capability, s.value(h.device, h.capability))
}

// Lookup reads a device's capability, and reports false if nobody has reported it yet, so that a
// condition can carry on without it. A mistake is one as at Get: a device that doesn't exist, or a
// group's capability.
//
//	if v, ok := s.Lookup(CellarSensor.Humidity); ok && v > 65 {
func (s Snapshot) Lookup[T Value](r Ref[T]) (T, bool) {
	h := r.handle()
	notGroup(h)
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

// notGroup refuses a read of one value from a group's capability.
func notGroup(h handle) {
	if h.g != nil {
		panic(fmt.Sprintf("%s is a group's: read it with Any, All, Count or Readings", h.named()))
	}
}

// Is reports whether c is true now, as a case's condition or a flow's If would be. A condition that
// reads a value nobody has reported yet stops the ConditionFunc there too.
func (s Snapshot) Is(c Condition) bool {
	if c == nil {
		panic("Is needs a condition")
	}
	return c.holds(s)
}

func as[T Value](device, capability string, v any) T {
	if name, ok := v.(string); ok {
		v = ClimateMode(name)
	}
	t, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("%s %s isn't a %T", device, capability, t))
	}
	return t
}

// Any reports whether a group's capability is true on any of its members that have reported it.
//
//	s.Any(home.Room("hallway").Occupied)
//
// Any, All, Count and Readings ignore the members that haven't reported the capability, and if none
// has, the condition stops there, as at a Get. A room's capability covers the devices in it now, of
// its kinds. A room with none stops the condition too, and since a case then keeps its state,
// that's logged at Warn.
func (s Snapshot) Any(b Bool) bool { return s.Count(b) > 0 }

// All reports whether a group's capability is true on all of its members that have reported it.
func (s Snapshot) All(b Bool) bool { return !slices.Contains(groupValues[bool](s, b.handle()), false) }

// Count is how many of a group's members that have reported the capability have it true.
func (s Snapshot) Count(b Bool) int {
	n := 0
	for _, v := range groupValues[bool](s, b.handle()) {
		if v {
			n++
		}
	}
	return n
}

// Readings are a group's capability's values on its members that have reported it, for slices.Max
// or an average: in the order given for a group, and by device name for a room.
//
//	slices.Max(s.Readings(cellar.Humidity))
func (s Snapshot) Readings(n Number) []float64 { return groupValues[float64](s, n.handle()) }

// roomValues are the reported values of a room's capability, on its devices of types, in device
// name order.
func roomValues[T Value](s Snapshot, room string, types []devicetype.Type, c string) []T {
	// A device with no room is in none, as for a room's writes and triggers, so "" is a mistake, not
	// an empty room.
	var names []string
	for name, m := range s.members {
		if room != "" && m.Room == room && m.OfTypes(types) {
			names = append(names, name)
		}
	}
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
		if v, ok := s.devices[name][c]; ok {
			out = append(out, as[T](name, c, v))
		}
	}
	if len(out) == 0 {
		panic(roomNotReported{room, c})
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
		case groupNotReported:
			err = fmt.Errorf("no member of group %s has reported %s", n.group, n.capability)
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

// Prefer weights the slots in some hours, from Daily, by score, adding to any others. The plan
// favours the slots whose scores add up highest.
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

// DefaultLate is how late a flow may wake, or a time of day fire, and still run.
const DefaultLate = 10 * time.Minute

// Flow runs its function when a trigger fires and If, if given, is true. The function is ordinary
// Go, and survives a crash at any point. It must stick to the steps on its FlowRun for anything that
// happens outside it or varies between runs, because after a crash it runs again from the top and
// recorded steps return what they returned the first time. docs/writing-flows.md says how. It needs
// a trigger: When, or a time of day in At, such as Clock("23:30"). home.Flow adds one.
type Flow struct {
	name    string
	on      []Change
	at      []Time
	cond    Condition
	restart bool
	late    time.Duration
	run     func(*FlowRun)
	once    once
}

// Flow adds a flow to the home, which runs run on each trigger. Give each flow a name of its own.
func (h *Home) Flow(name string, run func(*FlowRun)) *Flow {
	f := &Flow{name: name, run: run}
	h.flows = append(h.flows, f)
	return f
}

// When adds changes that trigger the flow, such as FrontDoor.Open.Becomes(true).
func (f *Flow) When(changes ...Change) *Flow {
	f.on = append(f.on, changes...)
	return f
}

// At adds times of day that trigger the flow.
func (f *Flow) At(times ...Time) *Flow {
	f.at = append(f.at, times...)
	return f
}

// If is checked when a trigger fires, and the flow runs only if it's true.
func (f *Flow) If(c Condition) *Flow {
	f.cond = c
	f.once = f.once.note("If")
	return f
}

// Restart cancels a run in progress on a new trigger. Without it, the trigger is ignored.
func (f *Flow) Restart() *Flow {
	f.restart = true
	f.once = f.once.note("Restart")
	return f
}

// Late is how late a run may start or wake and still carry on. Without it, or at zero, that's
// DefaultLate. A run later than that gives up and logs it, rather than turn the lights off at 09:00.
// A flow a decision ranks above its override line acts however late it is.
func (f *Flow) Late(d time.Duration) *Flow {
	f.late = d
	f.once = f.once.note("Late")
	return f
}

// FlowRun is one run of a flow. Its steps are written to the host's disk before the flow continues.
type FlowRun struct {
	run *flow.Run
	// name is the flow's, which a decision may rank on its own.
	name string
	// decided are the home's decisions, which say where a Set ranks.
	decided []compiledDecision
	// failed are the steps that failed, which make the run fail when it ends.
	failed []error
	// options are the home's, which a condition is checked against when the flow uses it.
	options Options
}

// checked checks a condition a flow uses, which oikade check can't see, and ends the run as failed
// if it has a mistake in it.
func (r *FlowRun) checked(step string, c Condition) {
	if c == nil {
		r.Fail(errors.New(step + " needs a condition"))
	}
	if err := c.check(r.options); err != nil {
		r.Fail(fmt.Errorf("%s: %w", step, err))
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
		r.Fail(fmt.Errorf("Get: %w", err))
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
		r.Fail(fmt.Errorf("Is: %w", err))
	}
	return ok
}

// WaitFor waits until c is true and returns true, or returns false once timeout has passed. Zero
// means no limit. A restart doesn't move the time it ends.
func (r *FlowRun) WaitFor(c Condition, timeout time.Duration) bool {
	r.checked("WaitFor", c)
	return r.run.WaitFor(toEngine(c), timeout)
}

// Set writes a capability, such as Hallway.On.To(true), or claims it if a decision decides it,
// which needs For, UntilReleased or UntilNext. It may run more than once, after a crash, so set a
// value and never toggle one. A Set that fails, as on a device that doesn't answer or a decision with
// no Flows line for this flow, is logged, and the run carries on and ends as failed.
//
// A group's write is one to each member with the capability, ranked by its own decision, and one
// that fails doesn't stop the others.
func (r *FlowRun) Set(s Set) {
	replayed := r.run.Replaying()
	var err error
	if s.group != nil && s.group.room == "" {
		if len(s.devices) == 0 {
			err = fmt.Errorf("no member of %s has %s", s.group.what(), s.capability)
		}
		for _, dev := range s.devices {
			one := s
			one.device, one.devices, one.group = dev, nil, nil
			err = errors.Join(err, r.set(one))
		}
	} else {
		err = r.set(s)
	}
	r.failedStep("set", err, replayed)
}

// set is a write to a device, or a room.
func (r *FlowRun) set(s Set) error {
	at := s.device
	if s.room != "" {
		at = s.room
	}
	if err := needsFlowsLine(r.decided, r.name, at, s.capability); err != nil {
		return err
	}
	return r.run.Apply(s.toEngine(r.decided, r.name))
}

// Release ends the named rule's claims, and its latch if it's a case: "<device> <capability> <case>"
// for a case, or a flow's name. One that fails is logged, and the run ends as failed.
func (r *FlowRun) Release(rule string) {
	replayed := r.run.Replaying()
	r.failedStep("release", r.run.Apply(engine.Release(rule)), replayed)
}

// failedStep records a step that failed, which the run carries on past. A step replayed from the
// journal was logged when it failed.
func (r *FlowRun) failedStep(step string, err error, replayed bool) {
	if err == nil {
		return
	}
	if !replayed {
		r.run.Log().Error("Failed step of flow run", "err", err, "step", step)
	}
	r.failed = append(r.failed, fmt.Errorf("%s: %w", step, err))
}

// Fail ends the run as failed, with err as the reason, and any steps that failed before it. It
// doesn't return. A recover in the flow can catch it, and the run still ends as failed.
func (r *FlowRun) Fail(err error) {
	if err == nil {
		err = errors.New("the flow failed")
	}
	r.failed = append(r.failed, err)
	panic(failure{err})
}

// failure is the panic Fail ends a run with.
type failure struct{ err error }

// Do makes an outside call and returns its result, or the zero value and its error. Once it's
// recorded the call never happens again: a replay returns the recorded result, or an error with the
// same message, which errors.Is won't match. A crash before that repeats it, so make the call safe
// to repeat. Names tell a flow's calls apart, and the result must survive JSON.
func (r *FlowRun) Do[T any](name string, call func(context.Context) (T, error)) (T, error) {
	var result T
	err := r.run.Do(name, &result, func(ctx context.Context) error {
		v, err := call(ctx)
		result = v
		return err
	})
	if err != nil {
		// A replay has only the error, so a live run mustn't give more.
		var zero T
		return zero, err
	}
	return result, nil
}

// Set writes a capability, from a Bool's or a Number's To, a device's or a group's. For,
// UntilReleased and UntilNext make it a claim on an arbitrated capability, which needs one of them:
//
//	Heater.On.To(true).For(time.Hour)
type Set struct {
	device, room, capability string
	// devices are a group's members that the write goes to, and group the group, if it's one's.
	devices []string
	group   *group
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

// toEngine is the engine's Set, ranked where the capability's decision puts the flow.
func (s Set) toEngine(decided []compiledDecision, flowName string) engine.Action {
	return engine.Set{
		Device: s.device, Room: s.room, DeviceTypes: s.types, Capability: s.capability, Value: s.value, For: s.claimFor,
		Until: s.until, Priority: claimPriority(decided, flowName, cmp.Or(s.device, s.room), s.capability),
	}
}

// Options are the settings of the whole home, which NewHome takes. The zero Options is a home with
// no location and a bridge called "Oikade".
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

// Home is a home's automations. NewHome makes one, and Decide, Flow, Plugin and Device add to it.
type Home struct {
	options   Options
	decisions []decider
	groups    []*group
	// mistakes are those made adding a group or room, which oikade check reports.
	mistakes []error
	flows    []*Flow
	plugins  []*Plugin
	// declared are the devices the home declares, from Device.
	declared []*Declaration
}

// NewHome makes an empty home with options o: add to it, then call Run.
func NewHome(o Options) *Home { return &Home{options: o} }

// Plugin is a plugin the home imports, which Home.Plugin makes.
type Plugin struct {
	name    string
	factory plugin.Factory
	hidden  bool
	shown   []string
}

// Plugin imports a plugin. It's a Go module like any other, and it runs as you, so importing one
// means trusting its code. The host runs each in a process of its own, so one that panics or hangs
// is restarted while automations and Matter carry on. name is what its devices are recorded under:
// keep it, because changing it makes them new hardware. factory builds the plugin, in its own
// process, never in your automations.
func (h *Home) Plugin(name string, factory plugin.Factory) *Plugin {
	p := &Plugin{name: name, factory: factory}
	h.plugins = append(h.plugins, p)
	return p
}

// HideFromMatter keeps the plugin's devices off Matter, for a platform that puts them in the Home app
// itself, so that they don't show there twice. Your code still reads and sets them, and one the
// plugin reports later is hidden too. Each deploy says which devices it takes off, and a controller
// loses its room and scenes for them. One shown again comes back as the same device to Matter.
func (p *Plugin) HideFromMatter() *Plugin {
	p.hidden = true
	return p
}

// ShowOnMatter names the devices of a plugin hidden from Matter that stay on it, such as one its
// platform doesn't put in the Home app. oikade check fails on a name that isn't one of its devices.
func (p *Plugin) ShowOnMatter(names ...string) *Plugin {
	p.shown = append(p.shown, names...)
	return p
}

// check finds the mistakes in what a plugin's import says of Matter.
func (p *Plugin) check() error {
	var errs []error
	if len(p.shown) > 0 && !p.hidden {
		errs = append(errs, fmt.Errorf("plugin %s: ShowOnMatter needs HideFromMatter, or its devices are on Matter already",
			p.name))
	}
	seen := map[string]bool{}
	for _, n := range p.shown {
		switch {
		case n == "":
			errs = append(errs, fmt.Errorf("plugin %s: ShowOnMatter needs a device's name", p.name))
		case seen[n]:
			errs = append(errs, fmt.Errorf("plugin %s: ShowOnMatter names %s twice", p.name, n))
		}
		seen[n] = true
	}
	return errors.Join(errs...)
}

// hiddenFromMatter is what the host takes of the plugins hidden from Matter.
func (h *Home) hiddenFromMatter() []wire.HiddenPlugin {
	var out []wire.HiddenPlugin
	for _, p := range h.plugins {
		if p.hidden {
			out = append(out, wire.HiddenPlugin{Plugin: p.name, Shown: slices.Clone(p.shown)})
		}
	}
	return out
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
	errs = append(errs, err, h.options.check(), h.checkGroups(decided), h.checkInventory())
	errs = append(errs, h.mistakes...)
	for _, d := range decided {
		for _, f := range d.own {
			if !slices.Contains(h.flows, f) {
				errs = append(errs, fmt.Errorf("decision %s %s: Flows(%s) is a flow this home doesn't have", d.device,
					d.capability, f.name))
			}
		}
		for _, c := range d.cases {
			key := name(c.key)
			r, err := c.rule()
			if err == nil {
				r.Key = key
			}
			add(c.key, r, err)
		}
	}
	for _, r := range h.flows {
		if r.name != "" {
			if err := ident.CheckName(ident.Flow, r.name); err != nil {
				errs = append(errs, err)
			}
		}
		f := r.toFlow(name(r.name), h.options, decided)
		flows[r.name] = f
		c, err := engine.NewFlow(f.Key, r.toEngine(f))
		add(r.name, c, errors.Join(err, r.once.err(), nilChange(r.on), needsLocation(h.options, r.at...),
			checkCondition(r.cond, h.options)))
	}
	plugins := map[string]bool{}
	for _, p := range h.plugins {
		switch named := ident.CheckName(ident.Plugin, p.name); {
		case named != nil:
			errs = append(errs, named)
		case plugins[p.name]:
			errs = append(errs, fmt.Errorf("plugin %q is declared twice", p.name))
		case p.factory == nil:
			errs = append(errs, fmt.Errorf("plugin %s has no factory", p.name))
		}
		errs = append(errs, p.check())
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
			info := wire.RuleInfo{
				Name: c.key, Kind: wire.KindCase, Safety: c.safety, Latch: c.latch,
				Device: d.device, Capability: d.capability, Case: c.name,
			}
			if c.safety && c.hold.Room != "" {
				// A deploy checks a declared room or type doesn't take a device out of it.
				info.Rooms = []wire.RoomOfTypes{{
					Room: c.hold.Room, Capability: d.capability, DeviceTypes: c.hold.DeviceTypes, Except: c.hold.Except,
				}}
			}
			m.Rules = append(m.Rules, info)
		}
		m.Warnings = append(m.Warnings, d.warnings...)
	}
	for _, r := range h.flows {
		m.Rules = append(m.Rules, wire.RuleInfo{Name: r.name, Kind: wire.KindFlow, Safety: flowSafety(decided, r.name)})
	}
	for _, p := range h.plugins {
		m.Plugins = append(m.Plugins, p.name)
	}
	m.Control = settingsOf(decided)
	m.Inventory = h.inventory()
	m.Vars = hostVars.names()
	m.HiddenFromMatter = h.hiddenFromMatter()
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
			t.Changes = append(t.Changes, c.changes()...)
		}
	}
	for _, a := range at {
		t.Timers = append(t.Timers, a.timer())
	}
	return t
}

// nilChange reports a nil in a flow's When, which trigger leaves out.
func nilChange(on []Change) error {
	if i := slices.Index(on, nil); i >= 0 {
		return fmt.Errorf("a nil change, at When[%d]: give a device's or a group's capability", i)
	}
	var errs []error
	for i, c := range on {
		if n, ok := c.(Number); ok && isAverage(n) {
			errs = append(errs, fmt.Errorf("the change at When[%d] is an average, "+
				"which never changes as a reading does: give the reading", i))
		}
		if err := checkGroup(c.handle()); err != nil {
			errs = append(errs, fmt.Errorf("the change at When[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

// toFlow is the flow as the runner takes it. A flow that a decision ranks above its override line
// makes Safety claims, so it acts however late it is.
func (r *Flow) toFlow(key ident.ID, o Options, decided []compiledDecision) *flow.Flow {
	f := &flow.Flow{Key: key, Late: r.late, Safety: flowSafety(decided, r.name)}
	if r.run != nil {
		f.Body = func(run *flow.Run) (err error) {
			fr := &FlowRun{run: run, name: r.name, decided: decided, options: o}
			defer func() {
				if p := recover(); p != nil {
					if _, ok := p.(failure); !ok {
						panic(p)
					}
					err = errors.Join(fr.failed...)
				}
			}()
			r.run(fr)
			// Fail's failure counts even if the flow recovered from it.
			return errors.Join(fr.failed...)
		}
	}
	return f
}

func (r *Flow) toEngine(f *flow.Flow) engine.Flow {
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
	if b.bands != nil {
		if err := checkAverage(b.bands.Reading.handle()); err != nil {
			return err
		}
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

// Run runs the home and doesn't return. The host starts this binary as "<binary> automations", and
// as "<binary> check" to judge a new version: that loads the home and prints what it finds, with no
// writes and no session. Anything else is an error, because the home has nothing to do without the
// host.
func (h *Home) Run() {
	log := slog.New(slog.NewJSONHandler(logOutput(), nil))
	if err := run(h, log); err != nil {
		log.Error("Stopped process", "err", err)
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
