/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
)

// whenEnv is the CEL environment of while.when: a condition with no trigger.
var whenEnv = mustWhenEnv()

func mustWhenEnv() *cel.Env {
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

// hold is a compiled while and hold. The runtime doesn't run holds yet.
type hold struct {
	device     string
	room       string
	capability string
	value      any
	priority   homev1alpha1.RulePriority
	// window is nil when the hold has none.
	window *window
	// when is nil when the hold has no when.
	when              cel.Program
	onDelay, offDelay time.Duration
	latch             bool
}

// window is a compiled while.window. Its start and end are at or sun times with no days.
type window struct {
	start, end timeTrigger
	days       []time.Weekday
}

func compileHold(w *homev1alpha1.While, h *homev1alpha1.Hold, priority homev1alpha1.RulePriority) (*hold, error) {
	if h == nil {
		return nil, errors.New("hold is required with while")
	}
	c := &hold{device: h.Device, room: h.Room, capability: h.Capability, priority: priority, latch: w.Latch}
	var errs []error
	if (h.Device == "") == (h.Room == "") {
		errs = append(errs, errors.New("hold: exactly one of device or room is required"))
	}
	if v, err := writableValue(h.Capability, h.Value.Raw); err != nil {
		errs = append(errs, fmt.Errorf("hold: %w", err))
	} else {
		c.value = v
	}
	if err := checkPriority(priority); err != nil {
		errs = append(errs, err)
	}
	if w.Window == nil && w.When == "" {
		errs = append(errs, errors.New("while: a window or when is required"))
	}
	if w.Latch && w.When == "" {
		errs = append(errs, errors.New("while: latch needs when"))
	}
	if w.Window != nil {
		win, err := compileWindow(w.Window)
		if err != nil {
			errs = append(errs, fmt.Errorf("while.window: %w", err))
		}
		c.window = win
	}
	if w.When != "" {
		prg, err := compileBool(whenEnv, w.When)
		if err != nil {
			errs = append(errs, fmt.Errorf("while.when: %w", err))
		}
		c.when = prg
	}
	if w.OnDelay != nil {
		if c.onDelay = w.OnDelay.Duration; c.onDelay < 0 {
			errs = append(errs, errors.New("while.onDelay must not be negative"))
		}
	}
	if w.OffDelay != nil {
		if c.offDelay = w.OffDelay.Duration; c.offDelay < 0 {
			errs = append(errs, errors.New("while.offDelay must not be negative"))
		}
	}
	return c, errors.Join(errs...)
}

func compileWindow(w *homev1alpha1.Window) (*window, error) {
	start, err := compileWindowTime(w.Start)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	end, err := compileWindowTime(w.End)
	if err != nil {
		return nil, fmt.Errorf("end: %w", err)
	}
	days, err := compileDays(w.Days)
	return &window{start: start, end: end, days: days}, err
}

func compileWindowTime(t homev1alpha1.WindowTime) (timeTrigger, error) {
	switch {
	case t.Time != "" && t.Sun == "":
		if t.Offset != nil {
			return timeTrigger{}, errors.New("offset only applies to a sun time")
		}
		return compileAt(&homev1alpha1.AtTrigger{Time: t.Time})
	case t.Time == "" && t.Sun != "":
		return compileSun(&homev1alpha1.SunTrigger{Event: t.Sun, Offset: t.Offset})
	default:
		return timeTrigger{}, errors.New(`exactly one of "HH:MM" or {sun, offset} is required`)
	}
}

// compileBool compiles a CEL expression that must evaluate to a bool.
func compileBool(e *cel.Env, expr string) (cel.Program, error) {
	ast, iss := e.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
		return nil, fmt.Errorf("must evaluate to a bool, not %s", ast.OutputType())
	}
	return e.Program(ast)
}

// writableValue validates a JSON value for a writable capability.
func writableValue(name string, raw []byte) (any, error) {
	spec, ok := capability.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown capability %q", name)
	}
	if !spec.Writable {
		return nil, fmt.Errorf("capability %s is read-only", name)
	}
	return capability.ParseJSON(name, raw)
}

func checkPriority(p homev1alpha1.RulePriority) error {
	switch p {
	case "", homev1alpha1.RulePrioritySafety, homev1alpha1.RulePriorityOverride, homev1alpha1.RulePriorityNormal:
		return nil
	}
	return fmt.Errorf("unknown priority %q", p)
}
