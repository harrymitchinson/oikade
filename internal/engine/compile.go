// Package engine runs Rules: when a trigger fires and the condition holds, run the actions.
package engine

import (
	"time"

	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// Compiled is a validated rule, ready to run.
type Compiled struct {
	Key ident.ID
	// Generation is the rule's version.
	Generation int64
	Mode       Mode
	// changes are the rule's change triggers, and timers its at and sun triggers.
	changes []Change
	timers  []timeTrigger
	// StartingDeadline is how late a missed timer may still fire on startup.
	StartingDeadline time.Duration
	// Since is when a missed timer was last accounted for.
	Since time.Time
	// sinceLoad makes Since the time of the first Load, for a rule with no Since of its own.
	sinceLoad bool
	// condition is nil when there is none.
	condition Condition
	actions   []action
	// body runs in place of actions, for a flow.
	body Body
	// hold is nil for a moment.
	hold *hold
}

type action struct {
	device     string
	room       string
	capability string
	// types limits a room's set to devices of these types. None means every device.
	types []devicetype.Type
	value any
	delay time.Duration
	// release names the Rule a release action releases.
	release string
	// claimFor, until and priority shape a set on an arbitrated capability as a claim.
	claimFor time.Duration
	until    Until
	priority Priority
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

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
