// Package engine runs Rules: when a trigger fires and the condition holds, run the actions.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
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
	Mode       Mode
	// changes are the Rule's change triggers, and timers its at and sun triggers.
	changes []Change
	timers  []timeTrigger
	// StartingDeadline is how late a missed timer may still fire on startup.
	StartingDeadline time.Duration
	// Since is when a missed timer was last accounted for: the Rule's status.lastFire, or its
	// creation when it has never fired.
	Since time.Time
	// condition is nil when there is none.
	condition Condition
	actions   []action
	// hold is nil for a moment.
	hold *hold
	// staleBudget is set when the Rule's status has a budget's plan, but it has no budget.
	staleBudget bool
}

type action struct {
	device     string
	room       string
	capability string
	value      any
	delay      time.Duration
	// release names the Rule a release action releases.
	release string
	// claimFor, until and priority shape a set on an arbitrated capability as a claim.
	claimFor time.Duration
	until    Until
	priority Priority
}

// Compile validates a Rule: the condition must be boolean CEL, triggers must name known
// capabilities, and set actions must target writable capabilities with values of the right kind.
// Whether the devices and rooms exist is only checked when the Rule runs.
func Compile(a *homev1alpha1.Rule) (*Compiled, error) {
	c := &Compiled{
		Key:        types.NamespacedName{Namespace: a.Namespace, Name: a.Name},
		Generation: a.Generation,
		Mode:       Mode(a.Spec.Mode),
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
		c.Mode = ModeSingle
	}
	var errs []error
	if (len(a.Spec.Triggers) == 0) == (a.Spec.While == nil) {
		errs = append(errs, errors.New("exactly one of triggers or while is required"))
	}
	if a.Spec.While != nil {
		h, err := compileHold(a.Spec.While, a.Spec.Hold, a.Spec.Priority)
		if err != nil {
			errs = append(errs, err)
		}
		c.hold = h
		if a.Spec.Condition != "" {
			errs = append(errs, errors.New("condition applies to triggers; a hold uses while.when"))
		}
		if a.Spec.StartingDeadline != nil {
			errs = append(errs, errors.New("startingDeadline applies to triggers"))
		}
	} else {
		if a.Spec.Hold != nil {
			errs = append(errs, errors.New("hold is only allowed with while"))
		}
		if a.Spec.Priority != "" {
			errs = append(errs, errors.New("priority applies to a hold; a set action takes its own"))
		}
	}
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
		t := Change{Device: trig.Change.Device, Room: trig.Change.Room, Capability: trig.Change.Capability}
		for _, err := range t.check() {
			errs = append(errs, fmt.Errorf("triggers[%d].change: %w", i, err))
		}
		c.changes = append(c.changes, t)
	}
	if a.Spec.Condition != "" {
		prg, err := compileBool(env, a.Spec.Condition)
		if err != nil {
			errs = append(errs, fmt.Errorf("condition: %w", err))
		} else {
			c.condition = celCondition(prg)
		}
	}
	actions, actErrs := compileActions(a.Spec.Do)
	c.actions = actions
	errs = append(errs, actErrs...)
	if len(a.Spec.Triggers) > 0 && len(a.Spec.Do) == 0 {
		errs = append(errs, errors.New("do is required with triggers"))
	}
	if a.Spec.While != nil && len(a.Spec.Do) > 0 {
		errs = append(errs, errors.New("do is only allowed with triggers"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	keepBudget(a, c)
	return c, nil
}

// keepBudget keeps a budget hold's spec, to compile against its target's control entry when it
// plans, and the plan its status holds. A Rule with no budget but a plan in its status, left by
// one it no longer has, is marked to clear it.
func keepBudget(a *homev1alpha1.Rule, c *Compiled) {
	planned := meta.FindStatusCondition(a.Status.Conditions, homev1alpha1.ConditionPlanned)
	if c.hold == nil || a.Spec.While.Budget == nil {
		c.staleBudget = a.Status.Today != nil || planned != nil
		return
	}
	c.hold.budget = &budgetHold{spec: *a.Spec.DeepCopy(), today: a.Status.Today.DeepCopy(), planned: planned.DeepCopy()}
}

// compileActions validates a moment's actions.
func compileActions(do []homev1alpha1.Action) ([]action, []error) {
	var actions []action
	var errs []error
	for i, act := range do {
		switch {
		case act.Set != nil:
			set, err := setAction(act.Set)
			if err != nil {
				errs = append(errs, fmt.Errorf("do[%d]: %w", i, err))
				continue
			}
			actions = append(actions, set)
		case act.Release != nil:
			if act.Release.Rule == "" {
				errs = append(errs, fmt.Errorf("do[%d]: release needs a rule", i))
				continue
			}
			actions = append(actions, action{release: act.Release.Rule})
		case act.Delay != nil:
			if act.Delay.Duration <= 0 {
				errs = append(errs, fmt.Errorf("do[%d]: delay must be positive", i))
				continue
			}
			actions = append(actions, action{delay: act.Delay.Duration})
		default:
			errs = append(errs, fmt.Errorf("do[%d]: one of set, delay or release is required", i))
		}
	}
	return actions, errs
}

// setAction compiles a set action.
func setAction(s *homev1alpha1.SetAction) (action, error) {
	set := Set{
		Device: s.Device, Room: s.Room, Capability: s.Capability, Value: json.RawMessage(s.Value.Raw),
		Until: Until(s.Until), Priority: Priority(s.Priority), forSet: s.For != nil,
	}
	if s.For != nil {
		set.For = s.For.Duration
	}
	return set.action()
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
func (c *Compiled) eval(vars Vars) (bool, error) {
	if c.condition == nil {
		return true, nil
	}
	return c.condition(vars)
}

// notBool is the error from a CEL condition that didn't evaluate to a bool.
type notBool struct{ value any }

func (e notBool) Error() string { return fmt.Sprintf("condition evaluated to %T, not bool", e.value) }

// celCondition wraps a CEL program as a Condition.
func celCondition(prg cel.Program) Condition {
	return func(vars Vars) (bool, error) {
		out, _, err := prg.Eval(vars)
		if err != nil {
			return false, err
		}
		b, ok := out.Value().(bool)
		if !ok {
			return false, notBool{out.Value()}
		}
		return b, nil
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
