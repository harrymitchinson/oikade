// Package oikade stands in for the real one, which analysistest can't load from the module.
package oikade

type Set struct{}

func (s Set) UntilNext() Set                   { return s }
func (s Set) For(d int) Set                    { return s }
func (r *FlowRun) Do(name string) (int, error) { return 0, nil }

type Condition interface{ holds() bool }

type Change interface{ change() }

type Bool struct{}

func (b Bool) To(v bool) Set         { return Set{} }
func (b Bool) holds() bool           { return false }
func (b Bool) Becomes(v bool) Change { return nil }

type Number struct{}

func (n Number) Above(v float64) Condition { return nil }

type Group struct{ On Bool }

func (h *Home) Room(name string) Group { return Group{} }

type Hours struct{}

func Daily(start, end string) Hours { return Hours{} }
func (h Hours) holds() bool         { return false }

func Not(c Condition) Condition { return c }

type Home struct{}

func NewHome() *Home { return &Home{} }

func (h *Home) Run() {}

// Decision and Flow are added to their home as they're built.
type Decision struct{}

func (h *Home) Decide(b Bool) *Decision                             { return &Decision{} }
func (d *Decision) Case(name string, v bool, c Condition) *Decision { return d }
func (d *Decision) Default(v bool) *Decision                        { return d }

type Flow struct{}

type Declaration struct{}

func (h *Home) Device(name string) *Declaration                { return &Declaration{} }
func (d *Declaration) Hardware(plugin, id string) *Declaration { return d }
func (d *Declaration) Room(room string) *Declaration           { return d }

func (h *Home) Flow(name string, run func(*FlowRun)) *Flow { return &Flow{} }
func (f *Flow) When(c ...Change) *Flow                     { return f }

type Plugin struct{}

func (h *Home) Plugin(name string, factory any) *Plugin { return &Plugin{} }
func (p *Plugin) HideFromMatter() *Plugin               { return p }

// Secret declares a secret as it's made, so a discarded one still does something.
func Secret(name string) string { return name }

type FlowRun struct{}

func (r *FlowRun) Set(s Set)           {}
func (r *FlowRun) WaitFor() bool       { return false }
func (r *FlowRun) Release(rule string) {}

type Snapshot struct{}

func (s Snapshot) Get(b Bool) bool { return false }
