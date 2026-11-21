package oikade

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"oikade.dev/internal/engine"
)

// averages are the rolling averages the home's code makes with AverageOver, which the engine keeps
// from the start, and badAverages those it can't keep, by the name the average reads as.
var (
	averagesMu  sync.Mutex
	averages    []engine.Average
	badAverages = map[handle]string{}
)

// AverageOver is the reading's average over the last d, each value weighted by how long it held,
// which a condition, a Follow or a budget's Bands reads like the reading itself:
//
//	PoolWater.Temperature.AverageOver(24 * time.Hour).Below(22)
//
// The host keeps it across restarts, so a restart doesn't start it again. Until a window has gone
// by, it averages what it has. Make it where the home is built, not inside a ConditionFunc, so it's
// kept from the start.
func (n Number) AverageOver(d time.Duration) Number {
	out := Number{Device: n.Device, Capability: Capability[float64](engine.AverageVar(string(n.Capability), d))}
	averagesMu.Lock()
	defer averagesMu.Unlock()
	switch {
	case n.Device == "" || n.Capability == "":
	case n.g != nil:
		badAverages[out.handle()] = fmt.Sprintf("%s is a group's, which has no one reading to average", n.handle().named())
	case isAverage(n):
		badAverages[out.handle()] = fmt.Sprintf("%s %s is an average already", n.Device, n.Capability)
	case d < time.Minute:
		badAverages[out.handle()] = fmt.Sprintf("AverageOver(%v) must be at least a minute", d)
	default:
		a := engine.Average{Device: n.Device, Capability: string(n.Capability), Over: d}
		if !slices.Contains(averages, a) {
			averages = append(averages, a)
		}
	}
	return out
}

// isAverage says whether n was made by AverageOver.
func isAverage(n Number) bool { return strings.Contains(string(n.Capability), " over ") }

// averagesUsed are the averages the home's code has made.
func averagesUsed() []engine.Average {
	averagesMu.Lock()
	defer averagesMu.Unlock()
	return slices.Clone(averages)
}

// checkAverage says why a reading made by AverageOver can't be kept, if it can't.
func checkAverage(h handle) error {
	averagesMu.Lock()
	defer averagesMu.Unlock()
	if why, bad := badAverages[h]; bad {
		return errors.New(why)
	}
	return nil
}

// Follow is a line that sets the capability to f of a reading, such as a radiator's setpoint
// worked out from the temperature outside:
//
//	home.Decide(StudyRadiator.Setpoint).
//		OverrideFor(2 * time.Hour).
//		Follow("weather", OutdoorSensor.Temperature, func(outside float64) float64 { return 21 - outside/4 }).
//		Default(18)
//
// The value is rounded to the capability's step, 0.5 for a setpoint and 1 for brightness and
// position, or to Step's, and kept within the capability's bounds. It's written only when the
// rounded value changes, and the host keeps the last one written, so a restart doesn't write it
// again. It needs a number capability. Until the reading is reported, the lines below it apply.
func (d *Decision[T]) Follow(name string, reading Number, f func(float64) T) *Decision[T] {
	l := line{kind: followLine, name: name, reading: reading}
	var zero T
	if _, isNumber := any(zero).(float64); !isNumber {
		d.mistakes = append(d.mistakes, fmt.Errorf("Follow %q needs a number capability", name))
	}
	if f == nil {
		d.mistakes = append(d.mistakes, fmt.Errorf("Follow %q needs a function", name))
	} else {
		l.follow = func(v float64) any { return f(v) }
	}
	return d.add(l)
}

// Step is what the last Follow rounds to, in place of the capability's own step.
func (d *Decision[T]) Step(n float64) *Decision[T] {
	if len(d.lines) == 0 || d.lines[len(d.lines)-1].kind != followLine {
		d.mistakes = append(d.mistakes, errors.New("Step needs a Follow before it"))
		return d
	}
	l := &d.lines[len(d.lines)-1]
	l.once = l.once.note("Step")
	if n <= 0 {
		d.mistakes = append(d.mistakes, fmt.Errorf("%s: Step %v must be positive", l.what(), n))
	}
	l.step = n
	return d
}

// compileFollow makes a Follow line's hold, which reads the reading on every look.
func (l line) compileFollow(c *decidedCase) error {
	errs := []error{l.once.err(), checkHandle(l.reading.handle()), checkAverage(l.reading.handle())}
	if l.follow == nil {
		return errors.Join(errs...)
	}
	reading, f := l.reading, l.follow
	c.hold.Value, c.hold.Step = nil, l.step
	c.hold.Follow = func(vars engine.Vars) (out float64, err error) {
		err = evaluate(vars, func(s Snapshot) {
			v, _ := f(s.Get(reading)).(float64)
			out = v
		})
		return out, err
	}
	return errors.Join(errs...)
}
