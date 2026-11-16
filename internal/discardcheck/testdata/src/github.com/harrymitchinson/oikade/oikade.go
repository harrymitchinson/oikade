// Package oikade stands in for the real one, which analysistest can't load from the module.
package oikade

type Part interface{ part() }

type Moment struct{}

func NewMoment(name string, set Set) Moment { return Moment{} }

func (r Moment) Restart() Moment { return r }
func (Moment) part()             {}

type Set struct{}

func (s Set) UntilNext() Set { return s }

type Condition interface{ holds() bool }

type Change interface{ change() }

type Bool struct{}

func (b Bool) To(v bool) Set         { return Set{} }
func (b Bool) holds() bool           { return false }
func (b Bool) Becomes(v bool) Change { return nil }

type Number struct{}

func (n Number) Above(v float64) Condition { return nil }

type Room[T any] struct{}

func InRoom(room string, b Bool) Room[bool] { return Room[bool]{} }

func (r Room[T]) To(v T) Set { return Set{} }

type Hours struct{}

func Daily(start, end string) Hours { return Hours{} }
func (h Hours) holds() bool         { return false }

func Not(c Condition) Condition { return c }

type Bundle struct{}

func NewBundle(parts ...Part) Bundle { return Bundle{} }
func (Bundle) part()                 {}

type Home struct{}

func NewHome(parts ...Part) *Home { return &Home{} }

func Run(h *Home) {}

// Decision is added to its home as it's built.
type Decision struct{}

func (h *Home) Decide(b Bool) *Decision                             { return &Decision{} }
func (d *Decision) Case(name string, v bool, c Condition) *Decision { return d }
func (d *Decision) Default(v bool) *Decision                        { return d }

// Secret declares a secret as it's made, so a discarded one still does something.
func Secret(name string) string { return name }

type FlowRun struct{}

func (r *FlowRun) Set(s Set) error { return nil }
func (r *FlowRun) WaitFor() bool   { return false }
func (r *FlowRun) Release() error  { return nil }

type Snapshot struct{}

func (s Snapshot) Get(b Bool) bool { return false }
