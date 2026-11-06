// Package framework stands in for the real one, which analysistest can't load from the module.
package framework

import (
	"context"
	"time"
)

type FlowRun struct{}

func (r *FlowRun) Now() time.Time { return time.Time{} }

func (r *FlowRun) Do(name string, result any, call func(context.Context) error) error { return nil }

type Flow struct {
	Run func(*FlowRun) error
}
