// Package oikade stands in for the real one, which analysistest can't load from the module.
package oikade

type Part interface{ part() }

type Hold struct{}

func NewHold(name string, set Set) Hold { return Hold{} }

func (r Hold) Latch() Hold { return r }
func (Hold) part()         {}

type Set struct{}

func (s Set) UntilNext() Set { return s }

type Handle[T any] struct{}

func (h Handle[T]) To(v T) Set { return Set{} }

type Room[T any] struct{}

func InRoom[T any](room string, h Handle[T]) Room[T] { return Room[T]{} }

func (r Room[T]) To(v T) Set { return Set{} }

type Window struct{}

func NewWindow() Window { return Window{} }

type Bundle struct{}

func NewBundle(parts ...Part) Bundle { return Bundle{} }
func (Bundle) part()                 {}

type Home struct{}

func NewHome(parts ...Part) Home { return Home{} }

func Run(h Home) {}

// Secret declares a secret as it's made, so a discarded one still does something.
func Secret(name string) string { return name }

type FlowRun struct{}

func (r *FlowRun) Set(s Set) error { return nil }
func (r *FlowRun) WaitFor() bool   { return false }
func (r *FlowRun) Release() error  { return nil }

type State struct{}

func (s State) Get(h Handle[bool]) bool { return false }
