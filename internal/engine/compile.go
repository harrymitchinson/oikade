// Package engine runs Rules: when a trigger fires and the condition holds, run the actions.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
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

// Compiled is a validated Rule, ready to run.
type Compiled struct {
	Key types.NamespacedName
	// Generation is the Rule generation this was compiled from.
	Generation int64
	Mode       homev1alpha1.RuleMode
	// changes are the Rule's change triggers, and timers its at and sun triggers.
	changes []homev1alpha1.ChangeTrigger
	timers  []timeTrigger
	// StartingDeadline is how late a missed timer may still fire on startup.
	StartingDeadline time.Duration
	// Since is when a missed timer was last accounted for: the Rule's status.lastFire, or its
	// creation when it has never fired.
	Since time.Time
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

// Compile validates a Rule: the condition must be boolean CEL, triggers must name known
// capabilities, and set actions must target writable capabilities with values of the right kind.
// Whether the devices and rooms exist is only checked when the Rule runs.
func Compile(a *homev1alpha1.Rule) (*Compiled, error) {
	c := &Compiled{
		Key:        types.NamespacedName{Namespace: a.Namespace, Name: a.Name},
		Generation: a.Generation,
		Mode:       a.Spec.Mode,
		Since:      a.CreationTimestamp.Time,
	}
	// A lastFire from before the Rule was created belongs to an earlier Rule of the same name.
	if a.Status.LastFire.After(c.Since) {
		c.Since = a.Status.LastFire.Time
	}
	if d := a.Spec.StartingDeadline; d != nil {
		if d.Duration < 0 {
			return nil, errors.New("startingDeadline must not be negative")
		}
		c.StartingDeadline = d.Duration
	}
	if c.Mode == "" {
		c.Mode = homev1alpha1.RuleModeSingle
	}
	var errs []error
	for i, trig := range a.Spec.Triggers {
		if n := btoi(trig.Change != nil) + btoi(trig.At != nil) + btoi(trig.Sun != nil); n != 1 {
			errs = append(errs, fmt.Errorf("triggers[%d]: exactly one of change, at or sun is required", i))
			continue
		}
		if trig.At != nil || trig.Sun != nil {
			var tt timeTrigger
			var err error
			if trig.At != nil {
				tt, err = compileAt(trig.At)
			} else {
				tt, err = compileSun(trig.Sun)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("triggers[%d]: %w", i, err))
				continue
			}
			c.timers = append(c.timers, tt)
			continue
		}
		t := trig.Change
		if (t.Device == "") == (t.Room == "") {
			errs = append(errs, fmt.Errorf("triggers[%d].change: exactly one of device or room is required", i))
		}
		if _, ok := capability.Lookup(t.Capability); !ok {
			errs = append(errs, fmt.Errorf("triggers[%d].change: unknown capability %q", i, t.Capability))
		}
		c.changes = append(c.changes, *t)
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
	for i, act := range a.Spec.Do {
		switch {
		case act.Set != nil:
			s := act.Set
			if (s.Device == "") == (s.Room == "") {
				errs = append(errs, fmt.Errorf("do[%d]: exactly one of device or room is required", i))
				continue
			}
			spec, ok := capability.Lookup(s.Capability)
			if !ok {
				errs = append(errs, fmt.Errorf("do[%d]: unknown capability %q", i, s.Capability))
				continue
			}
			if !spec.Writable {
				errs = append(errs, fmt.Errorf("do[%d]: capability %s is read-only", i, s.Capability))
				continue
			}
			var raw any
			if err := json.Unmarshal(s.Value.Raw, &raw); err != nil {
				errs = append(errs, fmt.Errorf("do[%d]: value: %w", i, err))
				continue
			}
			v, err := capability.Validate(s.Capability, raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("do[%d]: %w", i, err))
				continue
			}
			c.actions = append(c.actions, action{device: s.Device, room: s.Room, capability: s.Capability, value: v})
		case act.Delay != nil:
			if act.Delay.Duration <= 0 {
				errs = append(errs, fmt.Errorf("do[%d]: delay must be positive", i))
				continue
			}
			c.actions = append(c.actions, action{delay: act.Delay.Duration})
		default:
			errs = append(errs, fmt.Errorf("do[%d]: one of set or delay is required", i))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// matches reports whether a capability change on a device, currently in room, triggers the
// Rule. room is empty when the device has none.
func (c *Compiled) matches(device, room, name string) bool {
	for _, t := range c.changes {
		if t.Capability != name {
			continue
		}
		if (t.Device != "" && t.Device == device) || (t.Room != "" && t.Room == room) {
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

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
