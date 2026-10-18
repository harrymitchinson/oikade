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

// Package engine runs Automations: when a trigger fires and the condition holds, run the actions.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
	"github.com/harry/oikade/internal/capability"
)

var env = mustEnv()

func mustEnv() *cel.Env {
	e, err := cel.NewEnv(
		cel.Variable("devices", cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.DynType))),
		cel.Variable("trigger", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("now", cel.TimestampType),
		// tz is the household's IANA timezone. CEL's timestamp accessors default to UTC,
		// so conditions use now.getHours(tz).
		cel.Variable("tz", cel.StringType),
	)
	if err != nil {
		panic(err)
	}
	return e
}

// Compiled is a validated Automation, ready to run.
type Compiled struct {
	Key types.NamespacedName
	// Generation is the Automation generation this was compiled from.
	Generation int64
	Mode       homev1alpha1.AutomationMode
	Trigger    []homev1alpha1.Trigger
	// program is nil when there is no condition.
	program cel.Program
	actions []action
}

type action struct {
	device     string
	room       string
	capability string
	value      any
	delay      time.Duration
}

// Compile validates an Automation: the condition must be boolean CEL, triggers must name known
// capabilities, and set actions must target writable capabilities with values of the right kind.
// Whether the devices and rooms exist is only checked when the Automation runs.
func Compile(a *homev1alpha1.Automation) (*Compiled, error) {
	c := &Compiled{
		Key:        types.NamespacedName{Namespace: a.Namespace, Name: a.Name},
		Generation: a.Generation,
		Mode:       a.Spec.Mode,
		Trigger:    a.Spec.Triggers,
	}
	if c.Mode == "" {
		c.Mode = homev1alpha1.AutomationModeSingle
	}
	var errs []error
	for i, t := range a.Spec.Triggers {
		if t.Device == "" && t.Room == "" {
			errs = append(errs, fmt.Errorf("triggers[%d]: one of device or room is required", i))
		}
		if _, ok := capability.Lookup(t.Capability); !ok {
			errs = append(errs, fmt.Errorf("triggers[%d]: unknown capability %q", i, t.Capability))
		}
	}
	if a.Spec.Condition != "" {
		ast, iss := env.Compile(a.Spec.Condition)
		switch {
		case iss.Err() != nil:
			errs = append(errs, fmt.Errorf("condition: %w", iss.Err()))
		case ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType:
			errs = append(errs, fmt.Errorf("condition must evaluate to a bool, not %s", ast.OutputType()))
		default:
			prg, err := env.Program(ast)
			if err != nil {
				errs = append(errs, fmt.Errorf("condition: %w", err))
			}
			c.program = prg
		}
	}
	for i, act := range a.Spec.Actions {
		switch {
		case act.Set != nil:
			s := act.Set
			if s.Device == "" && s.Room == "" {
				errs = append(errs, fmt.Errorf("actions[%d]: one of device or room is required", i))
				continue
			}
			spec, ok := capability.Lookup(s.Capability)
			if !ok {
				errs = append(errs, fmt.Errorf("actions[%d]: unknown capability %q", i, s.Capability))
				continue
			}
			if !spec.Writable {
				errs = append(errs, fmt.Errorf("actions[%d]: capability %s is read-only", i, s.Capability))
				continue
			}
			var raw any
			if err := json.Unmarshal(s.Value.Raw, &raw); err != nil {
				errs = append(errs, fmt.Errorf("actions[%d]: value: %w", i, err))
				continue
			}
			v, err := capability.Validate(s.Capability, raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("actions[%d]: %w", i, err))
				continue
			}
			c.actions = append(c.actions, action{device: s.Device, room: s.Room, capability: s.Capability, value: v})
		case act.Delay != nil:
			if act.Delay.Duration <= 0 {
				errs = append(errs, fmt.Errorf("actions[%d]: delay must be positive", i))
				continue
			}
			c.actions = append(c.actions, action{delay: act.Delay.Duration})
		default:
			errs = append(errs, fmt.Errorf("actions[%d]: one of set or delay is required", i))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// matches reports whether a capability change triggers the Automation.
func (c *Compiled) matches(device, name string) bool {
	for _, t := range c.Trigger {
		if t.Device == device && t.Capability == name {
			return true
		}
	}
	return false
}

// eval evaluates the condition. No condition means true.
func (c *Compiled) eval(vars map[string]any) (bool, error) {
	if c.program == nil {
		return true, nil
	}
	out, _, err := c.program.Eval(vars)
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("condition evaluated to %T, not bool", out.Value())
	}
	return b, nil
}
