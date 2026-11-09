// Package framework stands in for the real one, which analysistest can't load from the module.
package framework

type Hold struct{}

func NewHold(name string) Hold { return Hold{} }

func (r Hold) Latch() Hold { return r }

type Set struct{}

func (s Set) UntilNext() Set { return s }

type Handle[T any] struct{}

func (h Handle[T]) To(v T) Set { return Set{} }

type FlowRun struct{}

func (r *FlowRun) Set(s Set) error { return nil }
