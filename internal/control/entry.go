package control

import (
	"encoding/json"
	"time"
)

// The names an Entry uses, which are the ones people write.
const (
	NamePreferOn  = "PreferOn"
	NamePreferOff = "PreferOff"
	NameHighest   = "Highest"
	NameLowest    = "Lowest"
	NameOverride  = "Override"
	NameRevert    = "Revert"
	// NameNext is Entry.OverrideHold for an outside change that holds until the next change.
	NameNext = "Next"
)

// Entry is how a device arbitrates one capability, as written: a Device's control entry, a home's
// declaration in Go, or what the host keeps for a device. engine.ControlSettings checks it.
type Entry struct {
	Capability string `json:"capability"`
	// Default is the JSON value the capability has when nothing claims it.
	Default json.RawMessage `json:"default"`
	// Resolve is NamePreferOn, NamePreferOff, NameHighest or NameLowest.
	Resolve string `json:"resolve"`
	// External is NameOverride or NameRevert: what an outside change does.
	External string `json:"external"`
	// OverrideHold is how long an outside change holds, as a duration, or NameNext.
	OverrideHold string   `json:"overrideHold,omitempty"`
	MinimumOn    Duration `json:"minimumOn,omitzero"`
	MaximumOn    Duration `json:"maximumOn,omitzero"`
	MinimumOff   Duration `json:"minimumOff,omitzero"`
	// MaximumStartsPerDay limits off-to-on transitions. Zero means unlimited.
	MaximumStartsPerDay int `json:"maximumStartsPerDay,omitempty"`
}

// Duration is a time.Duration written as "30m".
type Duration time.Duration

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = Duration(v)
	return err
}
