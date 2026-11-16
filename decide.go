package oikade

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
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
	// delay and linger are how long the case's conditions must stay true, or false, first.
	delay, linger time.Duration
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
	if d.def != nil {
		d.mistakes = append(d.mistakes, fmt.Errorf("%s comes after Default, which is the last line", l.what()))
	}
	d.lines = append(d.lines, l)
	return d
}

// Case is a line that sets the capability to v while every condition is true. Its name is the
// capability's own, such as frost on pool-pump on, which oikade status, pause and resume use: keep
// it, because its delay and latch are kept by it.
func (d *Decision[T]) Case(name string, v T, when ...Condition) *Decision[T] {
	return d.add(line{kind: caseLine, name: name, value: v, when: slices.Clone(when)})
}

// Delay is how long the last case's conditions must stay true before it applies.
func (d *Decision[T]) Delay(dur time.Duration) *Decision[T] {
	return d.onCase("Delay", func(l *line) { l.delay = dur })
}

// Linger is how long the last case still applies once its conditions are false.
func (d *Decision[T]) Linger(dur time.Duration) *Decision[T] {
	return d.onCase("Linger", func(l *line) { l.linger = dur })
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
	if d.def != nil {
		d.mistakes = append(d.mistakes, errors.New("Default is given twice"))
	}
	d.def = &v
	return d
}

// decider is a Decision of any type.
type decider interface {
	compile(o Options) compiledDecision
}

// compiledDecision is a decision compiled: its arbitration settings, and a hold for each of its cases.
type compiledDecision struct {
	device, capability string
	entry              control.Entry
	cases              []decidedCase
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

// override finds the override line, at -1 for none, and sets what an outside change does.
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

func (d *Decision[T]) compile(o Options) compiledDecision {
	out := compiledDecision{errs: slices.Clone(d.mistakes)}
	if d.ref == nil {
		return out
	}
	h := d.ref.handle()
	out.device, out.capability = h.device, h.capability
	bad := func(err error) { out.errs = append(out.errs, err) }
	if h.device == "" || h.capability == "" {
		bad(errors.New("Decide needs a device's capability, such as PoolPump.On"))
		return out
	}
	entry := control.Entry{Capability: h.capability}
	if d.def == nil {
		bad(errors.New("it needs a Default, its last line"))
	} else if raw, err := json.Marshal(*d.def); err != nil {
		bad(fmt.Errorf("default: %w", err))
	} else {
		entry.Default = raw
	}

	over, err := d.override(&entry)
	if err != nil {
		bad(err)
	}
	none := over >= 0 && d.lines[over].none

	names := map[string]bool{}
	latched := false
	for i, l := range d.lines {
		safety := none || i < over
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
		}
		if err := caseName(l.name); err != nil {
			bad(fmt.Errorf("%s: %w", l.what(), err))
		}
		if names[l.name] {
			bad(fmt.Errorf("%s is given twice", l.what()))
		}
		names[l.name] = true
		key := control.CaseKey(h.device, h.capability, l.name)
		entry.Lines = append(entry.Lines, "Rule/"+key)
		c, err := l.compile(h, safety, o)
		if err != nil {
			bad(fmt.Errorf("%s: %w", l.what(), err))
		}
		c.key, c.name = key, l.name
		latched = latched || c.latch
		out.cases = append(out.cases, c)
	}
	if latched && over < 0 {
		out.warnings = append(out.warnings, fmt.Sprintf("%s has a latched case and no override line, "+
			"so a tap in the Home app wins until the case lets go: give OverrideFor, OverrideUntilNext or NoOverride",
			out.name()))
	}
	if l := d.limits; l != nil {
		entry.MinimumOn, entry.MaximumOn = control.Duration(l.MinimumOn), control.Duration(l.MaximumOn)
		entry.MinimumOff, entry.MaximumStartsPerDay = control.Duration(l.MinimumOff), l.MaximumStartsPerDay
	}
	if len(out.errs) == 0 {
		if _, err := engine.ControlSettings(entry, nil); err != nil {
			bad(err)
		}
	}
	out.entry = entry
	for _, c := range out.cases {
		if c.isBudget && len(out.errs) == 0 {
			bad(checkBudget(c.budget, h, c.hold.Value, entry))
		}
	}
	return out
}

// caseName reports a case's name that oikade pause can't take as one word.
func caseName(name string) error {
	switch {
	case name == "":
		return errors.New("it needs a name")
	case strings.ContainsFunc(name, unicode.IsSpace):
		return errors.New("its name has a space in it")
	}
	return nil
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
// manifest's rules.
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
		if c.device != "" && slices.ContainsFunc(out, func(o compiledDecision) bool {
			return o.device == c.device && o.capability == c.capability
		}) {
			errs = append(errs, fmt.Errorf("%s is decided twice: a capability has one decision", c.name()))
		}
		out = append(out, c)
	}
	return out, errors.Join(errs...)
}

// settings are the home's decisions as the host takes them.
func settingsOf(decided []compiledDecision) []wire.DeviceControl {
	out := make([]wire.DeviceControl, 0, len(decided))
	for _, d := range decided {
		if d.device != "" {
			out = append(out, wire.DeviceControl{Device: d.device, Entry: d.entry})
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
	entry.Lines = append(entry.Lines, "Rule/"+l.flow.name)
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

// decisionOf is the decision on a device's capability, if there is one.
func decisionOf(decided []compiledDecision, device, capability string) (compiledDecision, bool) {
	i := slices.IndexFunc(decided, func(d compiledDecision) bool {
		return d.device == device && d.capability == capability
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

// caseRule is a case's engine rule.
func (c decidedCase) rule() (*engine.Compiled, error) {
	return engine.NewHold(ident.ID{Name: c.key}, c.hold)
}
