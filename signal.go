package oikade

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"oikade.dev/internal/engine"
	"oikade.dev/internal/ident"
)

// Signal is a fact about the home worked out from conditions, under a name of its own, such as
// whether anyone's home. Home.Signal makes one. It's a condition, true while the signal is, and a
// change, which fires as it changes. Every rule reads the same value, worked out once:
//
//	presence := home.Group("presence", HallSensor, BathroomSensor)
//	anyoneHome := home.Signal("anyone-home", presence.Occupied.Any()).Linger(36 * time.Hour)
//
//	home.Decide(TowelRail.Setpoint).Case("away", 7, oikade.Not(anyoneHome)).Default(19)
//	home.Flow("arrived", arrived).When(anyoneHome.Becomes(true))
//
// Conditions that can't be worked out, as before a sensor has reported, count as false, so a rule
// that reads a signal never waits for it. A signal reads an offline device as if it had never
// reported, and MaxAge does the same for one that has been quiet too long, so a dead sensor can't
// hold it true. The host keeps a signal's value, and when its Delay or Linger ends, under its name.
type Signal struct {
	s *madeSignal
}

type madeSignal struct {
	name                  string
	when                  []Condition
	delay, linger, maxAge time.Duration
	once                  once
}

// Signal adds a signal to the home, true while every condition is. Give each a name of its own, and
// keep it: renaming a signal starts it afresh. A signal can read the signals made before it.
func (h *Home) Signal(name string, when ...Condition) Signal {
	s := Signal{&madeSignal{name: name, when: slices.Clone(when)}}
	h.signals = append(h.signals, s.s)
	return s
}

// Delay is how long the signal's conditions must stay true before it turns true. A new signal, or a
// renamed one, has no past to wait through, so it starts at what its conditions say.
func (s Signal) Delay(d time.Duration) Signal {
	return s.option("Delay", func(sg *madeSignal) { sg.delay = d })
}

// Linger is how long the signal stays true once its conditions are false.
func (s Signal) Linger(d time.Duration) Signal {
	return s.option("Linger", func(sg *madeSignal) { sg.linger = d })
}

// MaxAge leaves out a device that hasn't reported for d, as if it had never reported, as well as
// one that's offline. Zero, the default, means no limit, which suits a sensor that reports only when
// it changes.
func (s Signal) MaxAge(d time.Duration) Signal {
	return s.option("MaxAge", func(sg *madeSignal) { sg.maxAge = d })
}

func (s Signal) option(name string, set func(*madeSignal)) Signal {
	if s.s != nil {
		s.s.once = s.s.once.note(name)
		set(s.s)
	}
	return s
}

// Becomes is a change to v: a trigger that fires when the signal changes to v, and not when it
// changes away from it.
func (s Signal) Becomes(v bool) Change { return signalBecomes{s, v} }

// String is its ref, such as signal/anyone-home.
func (s Signal) String() string {
	if s.s == nil {
		return "a zero Signal"
	}
	return ident.NewRef(ident.Signal, s.s.name).String()
}

// errZeroSignal is the mistake of a Signal that Home.Signal didn't make.
var errZeroSignal = errors.New("a zero Signal: make one with Home.Signal")

func (s Signal) holds(snap Snapshot) bool {
	if s.s == nil {
		panic(errZeroSignal.Error())
	}
	if snap.signal == nil {
		panic(fmt.Sprintf("no signal %s", s.s.name))
	}
	v, err := snap.signal(s.s.name)
	if err != nil {
		panic(err.Error())
	}
	return v
}

func (s Signal) explain(snap Snapshot, _ bool) string {
	return fmt.Sprintf("%s is %t", s, s.holds(snap))
}

func (s Signal) check(Options) error {
	if s.s == nil {
		return errZeroSignal
	}
	return nil
}

func (s Signal) changes() []engine.Change {
	if s.s == nil {
		return nil // checkChange reports it.
	}
	return []engine.Change{{Signal: s.s.name}}
}

func (s Signal) checkChange() error { return s.check(Options{}) }

type signalBecomes struct {
	s  Signal
	to bool
}

func (c signalBecomes) changes() []engine.Change {
	out := c.s.changes()
	for i := range out {
		out[i].To = c.to
	}
	return out
}

func (c signalBecomes) checkChange() error { return c.s.checkChange() }

// compileSignals checks the home's signals and turns them into the engine's, in the order they
// were made, which is an order each one's reads come before it in.
func (h *Home) compileSignals() ([]engine.Signal, error) {
	out := make([]engine.Signal, 0, len(h.signals))
	var errs []error
	seen := map[string]bool{}
	for _, s := range h.signals {
		bad := func(err error) { errs = append(errs, fmt.Errorf("signal %s: %w", s.name, err)) }
		switch err := ident.CheckName(ident.Signal, s.name); {
		case err != nil:
			errs = append(errs, err)
		case seen[s.name]:
			errs = append(errs, fmt.Errorf("signal %q is made twice", s.name))
		}
		seen[s.name] = true
		if len(s.when) == 0 {
			bad(errors.New("it needs a condition"))
		}
		var cond Condition = allOf(slices.Clone(s.when))
		if len(s.when) == 1 {
			cond = s.when[0]
		}
		if len(s.when) > 0 {
			if err := cond.check(h.options); err != nil {
				bad(err)
			}
		}
		if s.delay < 0 {
			bad(fmt.Errorf("Delay %v must not be negative", s.delay))
		}
		if s.linger < 0 {
			bad(fmt.Errorf("Linger %v must not be negative", s.linger))
		}
		if s.maxAge < 0 {
			bad(fmt.Errorf("MaxAge %v must not be negative", s.maxAge))
		}
		if err := s.once.err(); err != nil {
			bad(err)
		}
		out = append(out, engine.Signal{
			Name: s.name, When: toEngine(cond), OnDelay: s.delay, OffDelay: s.linger, MaxAge: s.maxAge,
		})
	}
	return out, errors.Join(errs...)
}

// signalsRead are the signals c reads, apart from any inside a ConditionFunc, which can't be seen.
func signalsRead(c Condition) []Signal {
	switch c := c.(type) {
	case Signal:
		return []Signal{c}
	case anyOf:
		return signalsReadAll(c)
	case allOf:
		return signalsReadAll(c)
	case not:
		return signalsRead(c.c)
	}
	return nil
}

func signalsReadAll(cs []Condition) []Signal {
	out := make([]Signal, 0, len(cs))
	for _, c := range cs {
		out = append(out, signalsRead(c)...)
	}
	return out
}

// checkSignalReads refuses a condition that reads a signal this home didn't make, which would fail
// at every look.
func (h *Home) checkSignalReads() error {
	var errs []error
	refuse := func(what string, cs []Condition) {
		for _, s := range signalsReadAll(cs) {
			if s.s != nil && !slices.Contains(h.signals, s.s) {
				errs = append(errs, fmt.Errorf("%s reads %s, which is another home's", what, s))
			}
		}
	}
	for _, d := range h.decisions {
		refuse(d.conditions())
	}
	for _, f := range h.flows {
		if f.cond != nil {
			refuse("flow "+f.name, []Condition{f.cond})
		}
	}
	for _, s := range h.signals {
		refuse("signal "+s.name, s.when)
	}
	return errors.Join(errs...)
}

// checkSignalChanges refuses a flow's change of a signal this home didn't make, which would never
// fire.
func (h *Home) checkSignalChanges(on []Change) error {
	var errs []error
	for i, c := range on {
		var s Signal
		switch c := c.(type) {
		case Signal:
			s = c
		case signalBecomes:
			s = c.s
		default:
			continue
		}
		if s.s != nil && !slices.Contains(h.signals, s.s) {
			errs = append(errs, fmt.Errorf("the change at When[%d]: %s is another home's", i, s))
		}
	}
	return errors.Join(errs...)
}
