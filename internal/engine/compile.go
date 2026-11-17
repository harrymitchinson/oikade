// Package engine runs Rules: holds, and flows, which run when a trigger fires and the condition holds.
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
	// body is what a flow runs.
	body Body
	// hold is nil for a flow.
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

// matches reports whether a capability change to value on a device, currently in room and of types,
// triggers the Rule. room is empty when the device has none.
func (c *Compiled) matches(device, room string, types []devicetype.Type, name string, value any) bool {
	for _, t := range c.changes {
		if t.Capability != name || t.To != nil && t.To != value {
			continue
		}
		if (t.Device != "" && t.Device == device) || (t.Room != "" && t.Room == room && devicetype.Any(types, t.DeviceTypes)) {
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
