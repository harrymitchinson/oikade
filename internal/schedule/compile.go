package schedule

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/google/cel-go/cel"
	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
)

// defaultMaxAge is the CRD's default for a bands input's maxAge.
const defaultMaxAge = 30 * time.Minute

var minutesEnv = mustMinutesEnv()

// mustMinutesEnv is the CEL environment of minutes.expression: while.when's, evaluated once at
// planningTime.
func mustMinutesEnv() *cel.Env {
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

// hhmm matches the CRD's pattern for planningTime and window times.
var hhmm = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

var weekdays = map[homev1alpha1.Weekday]time.Weekday{
	"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday,
	"Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday,
}

// Budget is a compiled budget: everything the planner needs except the day's clock and readings,
// which Input adds.
type Budget struct {
	// Device and Capability are the hold's target, which the budget switches on.
	Device, Capability string
	// PlanningMinute is planningTime in minutes since local midnight.
	PlanningMinute int
	SlotMinutes    int
	Rules          []Rule
	Constraints    Constraints
	// bands is nil when the budget is an expression.
	bands *Bands
	// inputDevice and inputCapability are what bands read.
	inputDevice, inputCapability string
	minutes                      MinutesFunc
}

// MinutesFunc works out a day's budget at planningTime, from the namespace's values.
type MinutesFunc func(devices map[string]map[string]any, now time.Time, loc *time.Location) (int, error)

// Spec is a budget as written, in a Rule or in Go.
type Spec struct {
	// Device and Capability are the hold's target, and Value what it holds it at.
	Device, Capability string
	Value              any
	// PlanningTime starts each planning day, as "HH:MM". Empty means midnight.
	PlanningTime string
	// SlotMinutes is 15, 30 or 60. Zero means 30.
	SlotMinutes int
	// Exactly one of Bands or Minutes is set.
	Bands   *BandSpec
	Minutes MinutesFunc
	Prefer  []PreferSpec
	Forbid  []WindowSpec
}

// BandSpec picks the budget from the band an input's value falls in.
type BandSpec struct {
	Device, Capability string
	// MaxAge is how old the value may be. Zero means 30 minutes.
	MaxAge         time.Duration
	MissingMinutes int
	// Ranges ascend by Below, and only the last has none.
	Ranges []Band
}

// WindowSpec is a span of local time, "HH:MM" to "HH:MM", on Days or on every day.
type WindowSpec struct {
	Start, End string
	Days       []time.Weekday
}

// PreferSpec weights the slots in a window.
type PreferSpec struct {
	Window WindowSpec
	Score  int
}

// Compile checks a budget Rule against the control entry of its target's capability, and compiles
// it. The Rule must have while.budget and a hold on a device. Every problem is reported.
func Compile(r *homev1alpha1.Rule, entry *control.Entry) (*Budget, error) {
	if r.Spec.While == nil || r.Spec.While.Budget == nil || r.Spec.Hold == nil {
		return nil, errors.New("not a budget hold")
	}
	spec, errs := specOf(r)
	b, err := CompileSpec(spec, entry)
	if err = errors.Join(append(errs, err)...); err != nil {
		return nil, err
	}
	return b, nil
}

// specOf reads a budget Rule, and says what it can't read.
func specOf(r *homev1alpha1.Rule) (Spec, []error) {
	b, h := r.Spec.While.Budget, r.Spec.Hold
	out := Spec{Device: h.Device, Capability: h.Capability, PlanningTime: b.PlanningTime, SlotMinutes: int(b.SlotMinutes)}
	var errs []error
	// A value that can't be read is reported as the wrong value.
	out.Value, _ = capability.ParseJSON(h.Capability, h.Value.Raw)
	switch m := &b.Minutes; {
	case (m.Bands == nil) == (m.Expression == ""):
		errs = append(errs, errors.New("budget.minutes: exactly one of bands or expression is required"))
		out.Minutes = noMinutes
	case m.Expression != "":
		f, err := compileExpression(m.Expression)
		if err != nil {
			errs = append(errs, err)
			f = noMinutes
		}
		out.Minutes = f
	default:
		bands := &BandSpec{
			Device: m.Bands.Input.Device, Capability: m.Bands.Input.Capability,
			MissingMinutes: int(m.Bands.MissingMinutes),
		}
		if age := m.Bands.Input.MaxAge; age != nil {
			if bands.MaxAge = age.Duration; bands.MaxAge <= 0 {
				errs = append(errs, errors.New("budget.minutes.bands.input.maxAge must be positive"))
				bands.MaxAge = defaultMaxAge
			}
		}
		for _, r := range m.Bands.Ranges {
			band := Band{Minutes: int(r.Minutes)}
			if r.Below != nil {
				v := r.Below.AsApproximateFloat64()
				band.Below = &v
			}
			bands.Ranges = append(bands.Ranges, band)
		}
		out.Bands = bands
	}
	for i, p := range b.Prefer {
		w, err := specWindow(&p.Window)
		if err != nil {
			errs = append(errs, fmt.Errorf("budget.prefer[%d].window: %w", i, err))
		}
		out.Prefer = append(out.Prefer, PreferSpec{Window: w, Score: int(p.Score)})
	}
	for i, f := range b.Forbid {
		w, err := specWindow(&f.Window)
		if err != nil {
			errs = append(errs, fmt.Errorf("budget.forbid[%d].window: %w", i, err))
		}
		out.Forbid = append(out.Forbid, w)
	}
	return out, errs
}

// noMinutes stands in for minutes that are already reported as wrong.
func noMinutes(map[string]map[string]any, time.Time, *time.Location) (int, error) { return 0, nil }

// CompileSpec checks a budget against the control entry of its target's capability, and compiles
// it. Every problem is reported.
func CompileSpec(s Spec, entry *control.Entry) (*Budget, error) {
	out := &Budget{Device: s.Device, Capability: s.Capability, SlotMinutes: s.SlotMinutes}
	var errs []error

	if s.Device == "" {
		errs = append(errs, errors.New("hold: a budget holds a device, not a room"))
	}
	if spec, ok := capability.Lookup(s.Capability); !ok || spec.Kind != homev1alpha1.CapabilityKindBool || !spec.Writable {
		errs = append(errs, fmt.Errorf("hold: a budget needs a writable bool capability, not %q", s.Capability))
	} else if s.Value != true {
		errs = append(errs, errors.New("hold: a budget holds its capability at true"))
	}

	if out.SlotMinutes == 0 {
		out.SlotMinutes = 30
	}
	if out.SlotMinutes != 15 && out.SlotMinutes != 30 && out.SlotMinutes != 60 {
		errs = append(errs, fmt.Errorf("budget.slotMinutes must be 15, 30 or 60, not %d", out.SlotMinutes))
	}
	if s.PlanningTime != "" {
		m, err := minuteOfDay(s.PlanningTime)
		if err != nil {
			errs = append(errs, fmt.Errorf("budget.planningTime: %w", err))
		}
		out.PlanningMinute = m
	}

	errs = append(errs, out.compileMinutes(&s)...)

	for i, p := range s.Prefer {
		w, err := compileWindow(p.Window)
		if err != nil {
			errs = append(errs, fmt.Errorf("budget.prefer[%d].window: %w", i, err))
		}
		out.Rules = append(out.Rules, Rule{Name: fmt.Sprintf("prefer[%d]", i), Window: w, Effect: Prefer, Prefer: p.Score})
	}
	for i, f := range s.Forbid {
		w, err := compileWindow(f)
		if err != nil {
			errs = append(errs, fmt.Errorf("budget.forbid[%d].window: %w", i, err))
		}
		out.Rules = append(out.Rules, Rule{Name: fmt.Sprintf("forbid[%d]", i), Window: w, Effect: Forbid})
	}

	if entry == nil {
		errs = append(errs, fmt.Errorf("%s on %s has no control entry", s.Capability, s.Device))
	} else {
		slot := time.Duration(out.SlotMinutes) * time.Minute
		for _, d := range []struct {
			name string
			v    time.Duration
			into *time.Duration
		}{
			{"minimumOn", time.Duration(entry.MinimumOn), &out.Constraints.MinimumRun},
			{"minimumOff", time.Duration(entry.MinimumOff), &out.Constraints.MinimumRest},
		} {
			*d.into = d.v
			if slot > 0 && d.v%slot != 0 {
				errs = append(errs, fmt.Errorf("control %s %s isn't a multiple of budget.slotMinutes (%d)", d.name, d.v, out.SlotMinutes))
			}
		}
		out.Constraints.MaximumStarts = entry.MaximumStartsPerDay
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func (out *Budget) compileMinutes(s *Spec) []error {
	if (s.Bands == nil) == (s.Minutes == nil) {
		return []error{errors.New("budget.minutes: exactly one of bands or expression is required")}
	}
	if s.Minutes != nil {
		out.minutes = s.Minutes
		return nil
	}

	bands := s.Bands
	var errs []error
	if spec, ok := capability.Lookup(bands.Capability); !ok || spec.Kind != homev1alpha1.CapabilityKindNumber {
		errs = append(errs, fmt.Errorf("budget.minutes.bands.input: %q isn't a number capability", bands.Capability))
	}
	out.inputDevice, out.inputCapability = bands.Device, bands.Capability
	out.bands = &Bands{MaxAge: bands.MaxAge, MissingMinutes: bands.MissingMinutes}
	switch {
	case bands.MaxAge == 0:
		out.bands.MaxAge = defaultMaxAge
	case bands.MaxAge < 0:
		errs = append(errs, errors.New("budget.minutes.bands.input.maxAge must be positive"))
	}
	if len(bands.Ranges) == 0 {
		errs = append(errs, errors.New("budget.minutes.bands.ranges: at least one band is required"))
	}
	for i, r := range bands.Ranges {
		last := i == len(bands.Ranges)-1
		switch {
		case r.Below == nil && !last:
			errs = append(errs, fmt.Errorf("budget.minutes.bands.ranges[%d]: only the last band omits below", i))
		case r.Below != nil && last:
			errs = append(errs, fmt.Errorf("budget.minutes.bands.ranges[%d]: the last band omits below, to take every other value", i))
		case r.Below != nil && i > 0 && out.bands.Ranges[i-1].Below != nil && *r.Below <= *out.bands.Ranges[i-1].Below:
			errs = append(errs, fmt.Errorf("budget.minutes.bands.ranges[%d]: below must be greater than the band before", i))
		}
		out.bands.Ranges = append(out.bands.Ranges, r)
	}
	return errs
}

// compileExpression compiles a minutes expression.
func compileExpression(expression string) (MinutesFunc, error) {
	ast, iss := minutesEnv.Compile(expression)
	if iss.Err() != nil {
		return nil, fmt.Errorf("budget.minutes.expression: %w", iss.Err())
	}
	switch ast.OutputType() {
	case cel.IntType, cel.UintType, cel.DoubleType, cel.DynType:
	default:
		return nil, fmt.Errorf("budget.minutes.expression must evaluate to a number, not %s", ast.OutputType())
	}
	prg, err := minutesEnv.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("budget.minutes.expression: %w", err)
	}
	return func(devices map[string]map[string]any, now time.Time, loc *time.Location) (int, error) {
		v, _, err := prg.Eval(map[string]any{"devices": devices, "now": now, "tz": loc.String()})
		if err != nil {
			return 0, err
		}
		// A dyn expression, such as one reading a device value, may give any number.
		switch n := v.Value().(type) {
		case int64:
			return int(n), nil
		case uint64:
			return int(min(n, math.MaxInt32)), nil
		case float64:
			if !math.IsNaN(n) && !math.IsInf(n, 0) {
				return int(math.Round(n)), nil
			}
		}
		return 0, fmt.Errorf("evaluated to %v, not a number of minutes", v.Value())
	}, nil
}

// Input returns the PlanInput for the planning day that starts on the given local date at
// planningTime. now is the planning time, devices are the namespace's values for the minutes, and
// read returns the bands input's latest reading, or nil if it has never reported.
func (out *Budget) Input(year int, month time.Month, day int, now time.Time, loc *time.Location,
	devices map[string]map[string]any, read func(device, name string) *Reading,
) PlanInput {
	in := PlanInput{
		Now:         now,
		NotBefore:   now,
		Slots:       DaySlots(year, month, day, out.PlanningMinute, out.SlotMinutes, loc),
		Rules:       out.Rules,
		Constraints: out.Constraints,
	}
	switch {
	case out.bands != nil:
		bands := *out.bands
		bands.Input = read(out.inputDevice, out.inputCapability)
		in.Minutes = &Minutes{Bands: &bands}
	case out.minutes != nil:
		in.Minutes = &Minutes{Expression: func() (int, error) { return out.minutes(devices, now, loc) }}
	}
	return in
}

// specWindow reads a Rule's budget window. Budgets plan a day ahead, so their windows are local
// times, not sun times. A sun time is reported here, and read as midnight.
func specWindow(w *homev1alpha1.Window) (WindowSpec, error) {
	out := WindowSpec{Start: w.Start.Time, End: w.End.Time}
	var errs []error
	if w.Start.Sun != "" || w.End.Sun != "" {
		errs = append(errs, errors.New(`a budget window's start and end are "HH:MM", not sun times`))
	}
	if w.Start.Sun != "" {
		out.Start = "00:00"
	}
	if w.End.Sun != "" {
		out.End = "00:00"
	}
	for _, d := range w.Days {
		wd, ok := weekdays[d]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown day %q", d))
			continue
		}
		out.Days = append(out.Days, wd)
	}
	return out, errors.Join(errs...)
}

// compileWindow compiles a budget window.
func compileWindow(w WindowSpec) (*Window, error) {
	var errs []error
	start, err := minuteOfDay(w.Start)
	if err != nil {
		errs = append(errs, fmt.Errorf("start: %w", err))
	}
	end, err := minuteOfDay(w.End)
	if err != nil {
		errs = append(errs, fmt.Errorf("end: %w", err))
	}
	return &Window{Start: start, End: end, Days: w.Days}, errors.Join(errs...)
}

func minuteOfDay(s string) (int, error) {
	parts := hhmm.FindStringSubmatch(s)
	if parts == nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	h, _ := strconv.Atoi(parts[1])
	m, _ := strconv.Atoi(parts[2])
	return h*60 + m, nil
}
