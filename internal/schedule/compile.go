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

// Budget is a compiled budget Rule: everything the planner needs except the day's clock and
// readings, which Input adds.
type Budget struct {
	// Device and Capability are the hold's target, which the budget switches on.
	Device, Capability string
	// PlanningMinute is planningTime in minutes since local midnight.
	PlanningMinute int
	SlotMinutes    int
	Rules          []Rule
	Constraints    Constraints
	// bands is nil when the budget is an expression.
	bands      *Bands
	input      homev1alpha1.MinutesInput
	expression cel.Program
}

// Compile checks a budget Rule against the control entry of its target's capability, and compiles
// it. The Rule must have while.budget and a hold on a device. Every problem is reported.
func Compile(r *homev1alpha1.Rule, entry *control.Entry) (*Budget, error) {
	if r.Spec.While == nil || r.Spec.While.Budget == nil || r.Spec.Hold == nil {
		return nil, errors.New("not a budget hold")
	}
	b, h := r.Spec.While.Budget, r.Spec.Hold
	out := &Budget{Device: h.Device, Capability: h.Capability, SlotMinutes: int(b.SlotMinutes)}
	var errs []error

	if h.Device == "" {
		errs = append(errs, errors.New("hold: a budget holds a device, not a room"))
	}
	if spec, ok := capability.Lookup(h.Capability); !ok || spec.Kind != homev1alpha1.CapabilityKindBool || !spec.Writable {
		errs = append(errs, fmt.Errorf("hold: a budget needs a writable bool capability, not %q", h.Capability))
	} else if v, err := capability.ParseJSON(h.Capability, h.Value.Raw); err != nil || v != true {
		errs = append(errs, errors.New("hold: a budget holds its capability at true"))
	}

	if out.SlotMinutes == 0 {
		out.SlotMinutes = 30
	}
	if out.SlotMinutes != 15 && out.SlotMinutes != 30 && out.SlotMinutes != 60 {
		errs = append(errs, fmt.Errorf("budget.slotMinutes must be 15, 30 or 60, not %d", out.SlotMinutes))
	}
	if b.PlanningTime != "" {
		m, err := minuteOfDay(b.PlanningTime)
		if err != nil {
			errs = append(errs, fmt.Errorf("budget.planningTime: %w", err))
		}
		out.PlanningMinute = m
	}

	errs = append(errs, out.compileMinutes(&b.Minutes)...)

	for i, p := range b.Prefer {
		w, err := compileWindow(&p.Window)
		if err != nil {
			errs = append(errs, fmt.Errorf("budget.prefer[%d].window: %w", i, err))
		}
		out.Rules = append(out.Rules, Rule{Name: fmt.Sprintf("prefer[%d]", i), Window: w, Effect: Prefer, Prefer: int(p.Score)})
	}
	for i, f := range b.Forbid {
		w, err := compileWindow(&f.Window)
		if err != nil {
			errs = append(errs, fmt.Errorf("budget.forbid[%d].window: %w", i, err))
		}
		out.Rules = append(out.Rules, Rule{Name: fmt.Sprintf("forbid[%d]", i), Window: w, Effect: Forbid})
	}

	if entry == nil {
		errs = append(errs, fmt.Errorf("%s on %s has no control entry", h.Capability, h.Device))
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

func (out *Budget) compileMinutes(m *homev1alpha1.BudgetMinutes) []error {
	if (m.Bands == nil) == (m.Expression == "") {
		return []error{errors.New("budget.minutes: exactly one of bands or expression is required")}
	}
	if m.Expression != "" {
		ast, iss := minutesEnv.Compile(m.Expression)
		if iss.Err() != nil {
			return []error{fmt.Errorf("budget.minutes.expression: %w", iss.Err())}
		}
		switch ast.OutputType() {
		case cel.IntType, cel.UintType, cel.DoubleType, cel.DynType:
		default:
			return []error{fmt.Errorf("budget.minutes.expression must evaluate to a number, not %s", ast.OutputType())}
		}
		prg, err := minutesEnv.Program(ast)
		if err != nil {
			return []error{fmt.Errorf("budget.minutes.expression: %w", err)}
		}
		out.expression = prg
		return nil
	}

	bands := m.Bands
	var errs []error
	if spec, ok := capability.Lookup(bands.Input.Capability); !ok || spec.Kind != homev1alpha1.CapabilityKindNumber {
		errs = append(errs, fmt.Errorf("budget.minutes.bands.input: %q isn't a number capability", bands.Input.Capability))
	}
	out.input = bands.Input
	out.bands = &Bands{MaxAge: defaultMaxAge, MissingMinutes: int(bands.MissingMinutes)}
	if bands.Input.MaxAge != nil {
		if out.bands.MaxAge = bands.Input.MaxAge.Duration; out.bands.MaxAge <= 0 {
			errs = append(errs, errors.New("budget.minutes.bands.input.maxAge must be positive"))
		}
	}
	if len(bands.Ranges) == 0 {
		errs = append(errs, errors.New("budget.minutes.bands.ranges: at least one band is required"))
	}
	for i, r := range bands.Ranges {
		last := i == len(bands.Ranges)-1
		band := Band{Minutes: int(r.Minutes)}
		switch {
		case r.Below == nil && !last:
			errs = append(errs, fmt.Errorf("budget.minutes.bands.ranges[%d]: only the last band omits below", i))
		case r.Below != nil && last:
			errs = append(errs, fmt.Errorf("budget.minutes.bands.ranges[%d]: the last band omits below, to take every other value", i))
		case r.Below != nil:
			v := r.Below.AsApproximateFloat64()
			if i > 0 && out.bands.Ranges[i-1].Below != nil && v <= *out.bands.Ranges[i-1].Below {
				errs = append(errs, fmt.Errorf("budget.minutes.bands.ranges[%d]: below must be greater than the band before", i))
			}
			band.Below = &v
		}
		out.bands.Ranges = append(out.bands.Ranges, band)
	}
	return errs
}

// Input returns the PlanInput for the planning day that starts on the given local date at
// planningTime. now is the planning time, devices are the namespace's values for the expression,
// and read returns the bands input's latest reading, or nil if it has never reported.
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
		bands.Input = read(out.input.Device, out.input.Capability)
		in.Minutes = &Minutes{Bands: &bands}
	case out.expression != nil:
		vars := map[string]any{"devices": devices, "now": now, "tz": loc.String()}
		in.Minutes = &Minutes{Expression: func() (int, error) {
			v, _, err := out.expression.Eval(vars)
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
		}}
	}
	return in
}

// compileWindow compiles a budget window. Budgets plan a day ahead, so their windows are local
// times, not sun events.
func compileWindow(w *homev1alpha1.Window) (*Window, error) {
	var errs []error
	if w.Start.Sun != "" || w.End.Sun != "" {
		errs = append(errs, errors.New(`a budget window's start and end are "HH:MM", not sun times`))
	}
	start, err := minuteOfDay(w.Start.Time)
	if err != nil && w.Start.Sun == "" {
		errs = append(errs, fmt.Errorf("start: %w", err))
	}
	end, err := minuteOfDay(w.End.Time)
	if err != nil && w.End.Sun == "" {
		errs = append(errs, fmt.Errorf("end: %w", err))
	}
	out := &Window{Start: start, End: end}
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

func minuteOfDay(s string) (int, error) {
	parts := hhmm.FindStringSubmatch(s)
	if parts == nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	h, _ := strconv.Atoi(parts[1])
	m, _ := strconv.Atoi(parts[2])
	return h*60 + m, nil
}
