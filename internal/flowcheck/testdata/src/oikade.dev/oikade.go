// Package oikade stands in for the real one, which analysistest can't load from the module.
package oikade

import (
	"context"
	"time"
)

type FlowRun struct{}

func (r *FlowRun) Now() time.Time { return time.Time{} }

func (r *FlowRun) Do[T any](name string, call func(context.Context) (T, error)) (T, error) {
	var v T
	return v, nil
}

type Home struct{}

func (h *Home) Flow(name string, run func(*FlowRun)) {}
