package oikade

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"oikade.dev/internal/control"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/ident"
)

// Decision is everything that sets one capability, as a list of lines read from the top: the
// first that applies wins, and Default is the last. Home.Decide makes one:
//
//	home.Decide(PorchLight.On).
//		Case("motion", true, PorchSensor.Occupied).Linger(5 * time.Minute).
//		Default(false)
//
// A change from outside your automations, such as a tap in the Home app, ranks where the override
// line is: OverrideFor, OverrideUntilNext or NoOverride. A line above it is a safety line, which an
// outside change can't beat. With no override line, an outside change wins until the list's answer
// next changes.
type Decision[T Value] struct {
	ref      Ref[T]
	lines    []line
	limits   *Limits
	def      *T
	then     Ref[T]
	mistakes []error
}

type lineKind int

const (
	caseLine lineKind = iota
	budgetLine
	overrideLine
	flowsLine
	followLine
)

// line is one line of a decision.
type line struct {
	kind  lineKind
	name  string
	value any
	when  []Condition
	// delay and linger are how long the case's conditions must stay true, or false, first, and
	// lingerUntil is nil for no LingerUntil.
	delay, linger time.Duration
	lingerUntil   *Time
	latch         bool
	until         Condition
	budget        *Budget
	// one is a Flows line for flow alone, rather than every flow without a line of its own.
	one  bool
	flow *Flow
	// overrideFor, untilNext and none say what an override line does.
	overrideFor time.Duration
	untilNext   bool
	none        bool
	once        once
	// reading, follow and step are a Follow line's.
	reading Number
	follow  func(float64) any
	step    float64
}

// what names the line in a mistake.
func (l line) what() string {
	switch l.kind {
	case caseLine:
		return fmt.Sprintf("Case %q", l.name)
	case budgetLine:
		return fmt.Sprintf("Budget %q", l.name)
	case overrideLine:
		return "the override line"
	case followLine:
		return fmt.Sprintf("Follow %q", l.name)
	}
	return "Flows"
}

// Limits hold a capability back, whichever line wins, safety lines included: an on that's too
// short, too long or too soon. They need a Bool.
type Limits struct {
	// MinimumOn is the shortest run.
	MinimumOn time.Duration
	// MaximumOn stops a longer run, and then rests for MinimumOff, or until nothing wants it on.
	MaximumOn time.Duration
	// MinimumOff is the shortest rest.
	MinimumOff time.Duration
	// MaximumStartsPerDay limits off-to-on changes in a day. Zero means unlimited.
	MaximumStartsPerDay int
}

// Decide starts the decision for a capability, such as PoolPump.On, which every line after it
// adds to. A capability has one decision.
func (h *Home) Decide[T Value](r Ref[T]) *Decision[T] {
	d := &Decision[T]{ref: r}
	if r == nil {
		d.mistakes = append(d.mistakes, errors.New("Decide needs a capability, such as PoolPump.On"))
	}
	h.decisions = append(h.decisions, d)
	return d
}

func (d *Decision[T]) add(l line) *Decision[T] {
	switch {
	case d.def != nil:
		d.mistakes = append(d.mistakes, fmt.Errorf("%s comes after Default, which is the last line", l.what()))
	case d.then != nil:
		d.mistakes = append(d.mistakes, fmt.Errorf("%s comes after Then, which is the last line", l.what()))
	}
	d.lines = append(d.lines, l)
	return d
}

// Case is a line that sets the capability to v while every condition is true. Its name is unique
// within the decision, and oikade status, pause and resume name the case by it, as
// case/hall-heater/on/occupied. Keep it: a case's delay and latch are kept under its name.
func (d *Decision[T]) Case(name string, v T, when ...Condition) *Decision[T] {
	return d.add(line{kind: caseLine, name: name, value: raw(v), when: slices.Clone(when)})
}

// Delay is how long the last case's conditions must stay true before it applies.
func (d *Decision[T]) Delay(dur time.Duration) *Decision[T] {
	return d.onCase("Delay", func(l *line) { l.delay = dur })
}

// Linger is how long the last case still applies once its conditions are false.
func (d *Decision[T]) Linger(dur time.Duration) *Decision[T] {
	return d.onCase("Linger", func(l *line) { l.linger = dur })
}

// LingerUntil keeps the last case applying once its conditions are false, until the next t: a
// time such as "23:00", or a Sunrise or Sunset, as in Daily. After a Linger, it's the next t once
// the Linger is over. Once someone's been out on the terrace after dark, the light stays on until
// 23:00:
//
//	home.Decide(TerraceLight.On).
//		Case("out", true, TerraceSensor.Occupied, oikade.Daily(oikade.Sunset(0), oikade.Clock("23:00"))).
//		LingerUntil("23:00").
//		Default(false)
//
// A t less than a second past counts as now, since a look runs a moment late: a case whose Daily
// ends at t ends there, not a day later.
func (d *Decision[T]) LingerUntil[U string | Time](t U) *Decision[T] {
	return d.onCase("LingerUntil", func(l *line) { l.lingerUntil = new(timeOf(t)) })
}

// LatchUntil keeps the last case applying once it starts, until c is true, such as
// GarageDoor.Open. A Release of the case's rule, "<device> <capability> <case>", ends it too.
func (d *Decision[T]) LatchUntil(c Condition) *Decision[T] {
	return d.onCase("LatchUntil", func(l *line) {
		l.latch, l.until = true, c
		if c == nil {
			d.mistakes = append(d.mistakes, fmt.Errorf("%s: LatchUntil needs a condition", l.what()))
		}
	})
}

func (d *Decision[T]) onCase(option string, f func(*line)) *Decision[T] {
	if len(d.lines) == 0 || d.lines[len(d.lines)-1].kind != caseLine {
		d.mistakes = append(d.mistakes, fmt.Errorf("%s needs a Case before it", option))
		return d
	}
	l := &d.lines[len(d.lines)-1]
	l.once = l.once.note(option)
	f(l)
	return d
}

// Budget is a line that sets the capability to v for as many minutes a day as b says, in the slots
// it plans, around the Limits, while every condition is true:
//
//	Budget("summer-extra", true, extra, oikade.Yearly("1 Jun", "15 Sep"))
//
// It needs a Bool, with v true. A slot the conditions rule out isn't made up later.
func (d *Decision[T]) Budget(name string, v T, b Budget, when ...Condition) *Decision[T] {
	return d.add(line{kind: budgetLine, name: name, value: v, budget: &b, when: slices.Clone(when)})
}

// OverrideFor is the override line: an outside change, such as a tap in the Home app, wins over
// the lines below it for dur.
func (d *Decision[T]) OverrideFor(dur time.Duration) *Decision[T] {
	if dur <= 0 {
		d.mistakes = append(d.mistakes, fmt.Errorf("OverrideFor %v must be positive", dur))
	}
	return d.add(line{kind: overrideLine, overrideFor: dur})
}

// OverrideUntilNext is the override line: an outside change wins over the lines below it until
// their answer next changes.
func (d *Decision[T]) OverrideUntilNext() *Decision[T] {
	return d.add(line{kind: overrideLine, untilNext: true})
}

// NoOverride writes an outside change back at once, so every line is a safety line. It goes before
// the first line.
func (d *Decision[T]) NoOverride() *Decision[T] {
	if len(d.lines) > 0 {
		d.mistakes = append(d.mistakes, errors.New("NoOverride goes before the first line"))
	}
	return d.add(line{kind: overrideLine, none: true})
}

// Flows is where a flow's Set ranks. With no flows, it's every flow without a line of its own. With
// some, it's a line for each, in the order given, so a leak flow can rank above a tap and the others
// below it:
//
//	home.Decide(MainsValve.Open).Flows(leak).OverrideFor(time.Hour).Flows().Default(true)
//
// A Set on a decided capability with no Flows line for its flow fails the run, and that's logged.
func (d *Decision[T]) Flows(flows ...*Flow) *Decision[T] {
	if len(flows) == 0 {
		return d.add(line{kind: flowsLine})
	}
	for _, f := range flows {
		d.add(line{kind: flowsLine, one: true, flow: f})
	}
	return d
}

// Limits hold the capability back, whichever line wins.
func (d *Decision[T]) Limits(l Limits) *Decision[T] {
	if d.limits != nil {
		d.mistakes = append(d.mistakes, errors.New("Limits is given twice"))
	}
	d.limits = &l
	return d
}

// Default is the capability's value when no line applies, and the list's last line.
func (d *Decision[T]) Default(v T) *Decision[T] {
	switch {
	case d.def != nil:
		d.mistakes = append(d.mistakes, errors.New("Default is given twice"))
	case d.then != nil:
		d.mistakes = append(d.mistakes, errors.New("Default comes after Then, which is the last line"))
	}
	d.def = &v
	return d
}

// Then hands a member of a group over to the group's decision on the capability when none of its
// own lines apply. It's the last line, in place of Default:
//
//	home.Decide(StudyRadiator.Setpoint).
//		Case("working", 21, StudySensor.Occupied, oikade.Daily("09:00", "17:30")).
//		Then(radiators.Setpoint)
//
// The member's lines rank above all of the group's, so with no override line of their own, they're
// safety lines. With one, it takes the place of the group's for this member, and the group's lines
// above its override line are still safety lines. The group's Limits and Default are the member's.
func (d *Decision[T]) Then(r Ref[T]) *Decision[T] {
	switch {
	case d.then != nil:
		d.mistakes = append(d.mistakes, errors.New("Then is given twice"))
	case d.def != nil:
		d.mistakes = append(d.mistakes, errors.New("Then comes after Default, which is the last line"))
	case r == nil:
		d.mistakes = append(d.mistakes, errors.New("Then needs a group's capability, such as radiators.Setpoint"))
		return d
	}
	d.then = r
	return d
}

// decider is a Decision of any type.
type decider interface {
	compile(o Options) compiledDecision
	// conditions names the decision as its mistakes do, with every condition its lines read.
	conditions() (string, []Condition)
}

func (d *Decision[T]) conditions() (string, []Condition) {
	what := compiledDecision{}
	if d.ref != nil {
		h := d.ref.handle()
		what.device, what.capability = h.device, h.capability
	}
	var out []Condition
	for _, l := range d.lines {
		out = append(out, l.when...)
		if l.until != nil {
			out = append(out, l.until)
		}
	}
	return what.name(), out
}

// compiledDecision is a decision compiled: its arbitration settings, and a hold for each of its cases.
// A group's is under the group's name, with g set, and each of its members has one of its own, made
// from it, with derived set.
type compiledDecision struct {
	device, capability string
	g                  *group
	// then is the group's capability a member's decision hands over to, and ownOverride whether
	// the member's decision has an override line of its own.
	then        *handle
	ownOverride bool
	derived     bool
	entry       control.Entry
	cases       []decidedCase
	// flows is whether the decision has a Flows line for every flow, and flowsSafety whether it's a
	// safety line.
	flows, flowsSafety bool
	// ranked are the flows with a line of their own, by name, and whether it's a safety line, and
	// own are those flows, which must be the home's.
	ranked   map[string]bool
	own      []*Flow
	errs     []error
	warnings []string
}

type decidedCase struct {
	key      string
	name     string
	hold     engine.Hold
	safety   bool
	latch    bool
	isBudget bool
	budget   *Budget
}

// name is how the decision's mistakes start.
func (d compiledDecision) name() string {
	if d.device == "" {
		return "a decision"
	}
	return fmt.Sprintf("decision %s %s", d.device, d.capability)
}

// override finds the override line, at -1 for none, and sets what an outside change does. A decision
// that hands over with Then and has none takes its group's.
func (d *Decision[T]) override(entry *control.Entry) (int, error) {
	over := -1
	var err error
	for i, l := range d.lines {
		if l.kind != overrideLine {
			continue
		}
		if over >= 0 {
			err = errors.New("it has one override line: OverrideFor, OverrideUntilNext or NoOverride")
		}
		over = i
	}
	switch {
	case over >= 0 && d.lines[over].none:
		entry.External = control.NameRevert
	case over < 0 && d.then != nil:
	case over < 0:
		// An outside change ranks above every line, until their answer next changes.
		entry.External, entry.OverrideHold = control.NameOverride, control.NameNext
		entry.Lines = append(entry.Lines, control.LineOverride)
	case d.lines[over].untilNext:
		entry.External, entry.OverrideHold = control.NameOverride, control.NameNext
	default:
		entry.External, entry.OverrideHold = control.NameOverride, d.lines[over].overrideFor.String()
	}
	return over, err
}

// ends checks how the decision ends: with a Default, or a Then to a group's capability, and sets the
// entry's default.
func (d *Decision[T]) ends(h handle, entry *control.Entry) []error {
	switch {
	case d.then != nil:
		t := d.then.handle()
		var errs []error
		switch {
		case h.g != nil:
			errs = append(errs, errors.New("Then hands a device's decision over to its group's: "+
				"a group's decision needs a Default"))
		case t.g == nil:
			errs = append(errs, fmt.Errorf("Then(%s) needs a group's or a room's capability, such as radiators.Setpoint",
				t.named()))
		case t.capability != h.capability:
			errs = append(errs, fmt.Errorf("Then(%s) is another capability", t.named()))
		case t.g.room == "" && !slices.Contains(t.g.has(t.capability), h.device):
			errs = append(errs, fmt.Errorf("Then(%s): %s isn't a member with %s", t.named(), h.device, h.capability))
		}
		if d.limits != nil {
			errs = append(errs, errors.New("Limits are the group's for a decision that hands over with Then"))
		}
		return errs
	case d.def == nil:
		return []error{errors.New("it needs a Default, its last line")}
	}
	raw, err := json.Marshal(*d.def)
	if err != nil {
		return []error{fmt.Errorf("default: %w", err)}
	}
	entry.Default = raw
	return nil
}

func (d *Decision[T]) compile(o Options) compiledDecision {
	out := compiledDecision{errs: slices.Clone(d.mistakes)}
	if d.ref == nil {
		return out
	}
	h := d.ref.handle()
	out.device, out.capability, out.g = h.device, h.capability, h.g
	bad := func(err error) { out.errs = append(out.errs, err) }
	if h.device == "" || h.capability == "" {
		bad(errors.New("Decide needs a device's capability, such as PoolPump.On"))
		return out
	}
	if err := checkGroup(h); err != nil {
		bad(err)
	}
	entry := control.Entry{Capability: h.capability}
	out.errs = append(out.errs, d.ends(h, &entry)...)
	if d.then != nil {
		t := d.then.handle()
		out.then = &t
	}

	over, err := d.override(&entry)
	if err != nil {
		bad(err)
	}
	none := over >= 0 && d.lines[over].none
	out.ownOverride = over >= 0

	names := map[string]bool{}
	latched := false
	for i, l := range d.lines {
		// With Then and no override line, every line is above the group's.
		safety := none || i < over || d.then != nil && over < 0
		switch l.kind {
		case overrideLine:
			if !none {
				entry.Lines = append(entry.Lines, control.LineOverride)
			}
			continue
		case flowsLine:
			if err := out.addFlowsLine(l, safety, &entry); err != nil {
				bad(err)
			}
			continue
		case budgetLine:
			if h.g != nil {
				bad(fmt.Errorf("%s: a budget is per device, so it can't be in %s's decision", l.what(), h.g.what()))
			}
		}
		if err := ident.CheckName(ident.Case, l.name); err != nil {
			bad(fmt.Errorf("%s: %w", l.what(), err))
		}
		if names[l.name] {
			bad(fmt.Errorf("%s is given twice", l.what()))
		}
		names[l.name] = true
		key := control.CaseKey(h.device, h.capability, l.name)
		entry.Lines = append(entry.Lines, control.SourceRule+key)
		c, err := l.compile(h, safety, o)
		if err != nil {
			bad(fmt.Errorf("%s: %w", l.what(), err))
		}
		c.key, c.name = key, l.name
		c.target(h)
		latched = latched || c.latch
		out.cases = append(out.cases, c)
	}
	if latched && over < 0 && d.then == nil {
		out.warnings = append(out.warnings, fmt.Sprintf("%s has a latched case and no override line, "+
			"so a tap in the Home app wins until the case lets go: give OverrideFor, OverrideUntilNext or NoOverride",
			out.name()))
	}
	if l := d.limits; l != nil {
		entry.MinimumOn, entry.MaximumOn = control.Duration(l.MinimumOn), control.Duration(l.MaximumOn)
		entry.MinimumOff, entry.MaximumStartsPerDay = control.Duration(l.MinimumOff), l.MaximumStartsPerDay
	}
	out.entry = entry
	if len(out.errs) == 0 && d.then == nil {
		// One that hands over is checked once it's merged with its group's.
		out.errs = append(out.errs, out.checkEntry(h))
	}
	return out
}

// checkEntry checks the decision's arbitration settings, and its budgets against them.
func (out *compiledDecision) checkEntry(h handle) error {
	if _, err := engine.ControlSettings(out.entry, nil); err != nil {
		return err
	}
	var errs []error
	for _, c := range out.cases {
		if c.isBudget {
			errs = append(errs, checkBudget(c.budget, h, c.hold.Value, out.entry))
		}
	}
	return errors.Join(errs...)
}

// target points a case's hold at its decision's capability: a device's, a group's members' or a
// room's devices'.
func (c *decidedCase) target(h handle) {
	g := h.g
	if g == nil {
		return
	}
	c.hold.Device = ""
	if g.room != "" {
		c.hold.Room, c.hold.DeviceTypes = g.room, g.types
		return
	}
	c.hold.Devices = g.has(h.capability)
}

func (l line) compile(h handle, safety bool, o Options) (decidedCase, error) {
	c := decidedCase{safety: safety, latch: l.latch}
	priority := engine.PriorityNormal
	if safety {
		priority = engine.PrioritySafety
	}
	c.hold = engine.Hold{Device: h.device, Capability: h.capability, Value: l.value, Priority: priority}
	errs := []error{l.once.err()}
	var cond Condition = allOf(slices.Clone(l.when))
	if len(l.when) == 1 {
		cond = l.when[0]
	}
	switch {
	case l.kind == followLine:
		return c, l.compileFollow(&c)
	case l.kind == budgetLine:
		if l.value != true {
			errs = append(errs, errors.New("a budget sets a Bool to true"))
		}
		c.isBudget, c.budget = true, l.budget
		c.hold.Budget = l.budget.toSchedule()
		errs = append(errs, l.budget.once.err())
		if len(l.when) > 0 {
			errs = append(errs, checkCondition(cond, o))
			c.hold.When, c.hold.WhyNot = toEngine(cond), whyNot(cond)
		}
		return c, errors.Join(errs...)
	case len(l.when) == 0:
		errs = append(errs, errors.New("it needs a condition: Default is for always"))
		return c, errors.Join(errs...)
	}
	errs = append(errs, checkCondition(cond, o), checkCondition(l.until, o))
	if l.delay < 0 {
		errs = append(errs, fmt.Errorf("Delay %v must not be negative", l.delay))
	}
	if l.linger < 0 {
		errs = append(errs, fmt.Errorf("Linger %v must not be negative", l.linger))
	}
	c.hold.OnDelay, c.hold.OffDelay = max(l.delay, 0), max(l.linger, 0)
	until, err := lingerUntil(l.lingerUntil, o)
	errs = append(errs, err)
	c.hold.OffUntil = until
	c.hold.WhyNot = whyNot(cond)
	if l.latch {
		// A latch is the engine's on a When, so a latched case keeps its Daily there.
		c.hold.When, c.hold.Latch, c.hold.Until = toEngine(cond), true, toEngine(l.until)
		return c, errors.Join(errs...)
	}
	hours, when := splitHours(cond)
	c.hold.When = toEngine(when)
	if hours != nil {
		c.hold.Window = hours.toEngine()
	}
	return c, errors.Join(errs...)
}

// whyNot says why c is false, for oikade status, and never panics, since the engine holds its lock.
func whyNot(c Condition) func(engine.Vars) string {
	return func(vars engine.Vars) (why string) {
		defer func() {
			if r := recover(); r != nil {
				why = fmt.Sprint(r)
			}
		}()
		return explain(c, vars)
	}
}

// compileDecisions compiles the home's decisions: the engine's holds, the host's settings and the
// manifest's rules. A member of a decided group gets a decision of its own, the group's, or its own
// lines above the group's when it hands over with Then.
func (h *Home) compileDecisions() ([]compiledDecision, error) {
	out := make([]compiledDecision, 0, len(h.decisions))
	var errs []error
	for _, d := range h.decisions {
		c := d.compile(h.options)
		for _, err := range c.errs {
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.name(), err))
			}
		}
		if i := slices.IndexFunc(out, func(o compiledDecision) bool {
			return c.device != "" && o.device == c.device && o.capability == c.capability
		}); i >= 0 {
			if o := out[i]; o.g != c.g && (o.g != nil || c.g != nil) {
				// A room, a group and a device of one name would key their cases alike.
				errs = append(errs, fmt.Errorf("%s and %s are both decided, and would name their cases alike: rename one",
					whatDecided(o), whatDecided(c)))
			} else {
				errs = append(errs, fmt.Errorf("%s is decided twice: a capability has one decision", c.name()))
			}
		}
		out = append(out, c)
	}
	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	out, err := handOver(out)
	return out, err
}

// whatDecided names what a decision is on: a device's, a group's or a room's capability.
func whatDecided(d compiledDecision) string {
	return handle{d.device, d.capability, d.g}.named()
}

// groupDecision is the decision on a group's capability, if there is one.
func groupDecision(decided []compiledDecision, g *group, capability string) (int, bool) {
	i := slices.IndexFunc(decided, func(d compiledDecision) bool { return d.g == g && d.capability == capability })
	return i, i >= 0
}

// handOver merges each Then into its group's decision, gives each member of a decided group a
// decision of its own, and leaves a room's devices with decisions of their own out of its holds.
func handOver(decided []compiledDecision) ([]compiledDecision, error) {
	var errs []error
	// Members that hand over, by group decision and member.
	handed := map[int]map[string]bool{}
	for i := range decided {
		d := &decided[i]
		if d.then == nil {
			continue
		}
		gi, ok := groupDecision(decided, d.then.g, d.capability)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: Then(%s) has no decision", d.name(), d.then.named()))
			continue
		}
		if handed[gi] == nil {
			handed[gi] = map[string]bool{}
		}
		handed[gi][d.device] = true
		if err := d.merge(decided[gi]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.name(), err))
		}
	}
	// Members of a decided group, by name and capability, and the group that decides them.
	in := map[[2]string]*group{}
	var derived []compiledDecision
	for gi, g := range decided {
		if g.g == nil || g.g.room != "" {
			continue
		}
		for _, m := range g.g.has(g.capability) {
			switch other := in[[2]string{m, g.capability}]; {
			case other != nil:
				errs = append(errs, fmt.Errorf("%s %s is in %s and %s, which both decide it",
					m, g.capability, other.what(), g.g.what()))
				continue
			case handed[gi][m]:
			case slices.ContainsFunc(decided, func(o compiledDecision) bool {
				return o.g == nil && o.device == m && o.capability == g.capability
			}):
				errs = append(errs, fmt.Errorf("%s %s is decided on its own and in %s: hand its decision over with Then(%s)",
					m, g.capability, g.g.what(), handle{g.device, g.capability, g.g}.named()))
			default:
				own := g
				own.device, own.g, own.derived, own.cases, own.warnings, own.errs = m, nil, true, nil, nil, nil
				derived = append(derived, own)
			}
			in[[2]string{m, g.capability}] = g.g
		}
	}
	decided = append(decided, derived...)
	for ri := range decided {
		r := &decided[ri]
		if r.g == nil || r.g.room == "" {
			continue
		}
		// A device with a decision of its own on the capability keeps it, unless it hands over to
		// this room's.
		var except []string
		for _, o := range decided {
			if o.g == nil && o.capability == r.capability && !handed[ri][o.device] {
				except = append(except, o.device)
			}
		}
		slices.Sort(except)
		for ci := range r.cases {
			r.cases[ci].hold.Except = slices.Compact(except)
		}
	}
	return decided, errors.Join(errs...)
}

// merge makes a member's decision that hands over with Then its own lines above its group's, with
// the group's Default and Limits, and its own override line in place of the group's if it has one.
func (d *compiledDecision) merge(g compiledDecision) error {
	own := d.entry
	e := g.entry
	e.Lines = slices.Clone(own.Lines)
	for _, l := range g.entry.Lines {
		if l == control.LineOverride && d.ownOverride || slices.Contains(e.Lines, l) {
			continue
		}
		e.Lines = append(e.Lines, l)
	}
	if d.ownOverride {
		e.External, e.OverrideHold = own.External, own.OverrideHold
	}
	d.entry = e
	err := d.checkEntry(handle{device: d.device, capability: d.capability})
	if !d.flows && g.flows {
		d.flows, d.flowsSafety = true, g.flowsSafety && !d.ownOverride
	}
	for name, safety := range g.ranked {
		if _, mine := d.ranked[name]; !mine {
			if d.ranked == nil {
				d.ranked = map[string]bool{}
			}
			d.ranked[name] = safety
		}
	}
	return err
}

// settingsOf is the home's decisions as the host takes them: one for each device, and each room's.
func settingsOf(decided []compiledDecision) []wire.DeviceControl {
	out := make([]wire.DeviceControl, 0, len(decided))
	for _, d := range decided {
		switch {
		case d.device == "":
		case d.g == nil:
			out = append(out, wire.DeviceControl{Device: d.device, Entry: d.entry})
		case d.g.room != "":
			out = append(out, wire.DeviceControl{Room: d.g.room, DeviceTypes: d.g.types, Entry: d.entry})
		}
	}
	return out
}

// addFlowsLine adds a Flows line to the decision and its entry: one for every flow, or for l's.
func (d *compiledDecision) addFlowsLine(l line, safety bool, entry *control.Entry) error {
	switch {
	case !l.one && d.flows:
		return errors.New("Flows is given twice")
	case !l.one:
		d.flows, d.flowsSafety = true, safety
		entry.Lines = append(entry.Lines, control.LineFlows)
		return nil
	case l.flow == nil:
		return errors.New("Flows is given a nil flow")
	case l.flow.name == "":
		return errors.New("Flows is given a flow with no name")
	}
	if _, twice := d.ranked[l.flow.name]; twice {
		return fmt.Errorf("Flows(%s) is given twice", l.flow.name)
	}
	if d.ranked == nil {
		d.ranked = map[string]bool{}
	}
	d.ranked[l.flow.name] = safety
	d.own = append(d.own, l.flow)
	entry.Lines = append(entry.Lines, control.SourceRule+l.flow.name)
	return nil
}

// flowLine says where a decision ranks a flow's Set: whether it has a line for it, and whether that's
// a safety line. A flow's own line comes before the line for every flow.
func (d compiledDecision) flowLine(flowName string) (ranked, safety bool) {
	if safety, ok := d.ranked[flowName]; ok {
		return true, safety
	}
	return d.flows, d.flows && d.flowsSafety
}

// decisionOf is the decision on a device's capability, or a room's, if there is one.
func decisionOf(decided []compiledDecision, device, capability string) (compiledDecision, bool) {
	i := slices.IndexFunc(decided, func(d compiledDecision) bool {
		return d.device == device && d.capability == capability && (d.g == nil || d.g.room != "")
	})
	if i < 0 {
		return compiledDecision{}, false
	}
	return decided[i], true
}

// claimPriority is the level of a flow's Set on a device's capability: Safety if the decision ranks
// the flow on a safety line, and Normal otherwise.
func claimPriority(decided []compiledDecision, flowName, device, capability string) engine.Priority {
	if d, ok := decisionOf(decided, device, capability); ok {
		if _, safety := d.flowLine(flowName); safety {
			return engine.PrioritySafety
		}
	}
	return ""
}

// needsFlowsLine refuses a flow's Set on a decided capability whose decision has no Flows line for
// it to rank at.
func needsFlowsLine(decided []compiledDecision, flowName, device, capability string) error {
	if d, ok := decisionOf(decided, device, capability); ok {
		if ranked, _ := d.flowLine(flowName); !ranked {
			return fmt.Errorf("%s %s is decided, and its decision has no Flows line for %s", device, capability, flowName)
		}
	}
	return nil
}

// flowSafety says whether any decision ranks the flow on a safety line, so that it makes Safety
// claims, and acts however late it is.
func flowSafety(decided []compiledDecision, flowName string) bool {
	return slices.ContainsFunc(decided, func(d compiledDecision) bool {
		_, safety := d.flowLine(flowName)
		return safety
	})
}

func (c decidedCase) rule() (*engine.Compiled, error) {
	return engine.NewHold(ident.ID{Name: c.key}, c.hold)
}
