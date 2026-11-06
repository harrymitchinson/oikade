package schedule

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
)

// defaultMaxAge is a bands input's maxAge when it has none.
const defaultMaxAge = 30 * time.Minute

// hhmm is planningTime's and a window's time of day.
var hhmm = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

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
	// bands is nil when the budget is a function.
	bands *Bands
	// inputDevice and inputCapability are what bands read.
	inputDevice, inputCapability string
	minutes                      MinutesFunc
}

// MinutesFunc works out a day's budget at planningTime, from the namespace's values.
type MinutesFunc func(devices map[string]map[string]any, now time.Time, loc *time.Location) (int, error)

// Spec is a budget as written.
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

// CompileSpec checks a budget against the control entry of its target's capability, and compiles
// it. Every problem is reported.
func CompileSpec(s Spec, entry *control.Entry) (*Budget, error) {
	out := &Budget{Device: s.Device, Capability: s.Capability, SlotMinutes: s.SlotMinutes}
	var errs []error

	if s.Device == "" {
		errs = append(errs, errors.New("hold: a budget holds a device, not a room"))
	}
	if spec, ok := capability.Lookup(s.Capability); !ok || spec.Kind != capability.KindBool || !spec.Writable {
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
		return []error{errors.New("budget.minutes: exactly one of bands or minutes is required")}
	}
	if s.Minutes != nil {
		out.minutes = s.Minutes
		return nil
	}

	bands := s.Bands
	var errs []error
	if spec, ok := capability.Lookup(bands.Capability); !ok || spec.Kind != capability.KindNumber {
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
