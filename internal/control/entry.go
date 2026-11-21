package control

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// The names an Entry uses.
const (
	NameOverride = "Override"
	NameRevert   = "Revert"
	// NameNext is Entry.OverrideHold for an outside change that holds until the next change.
	NameNext = "Next"
)

// LineOverride and LineFlows are the override line and the flows' line among Settings.Lines. A
// claim's source is never one: it's Matter, Device, Command or Rule/<key>.
const (
	LineOverride = "override"
	LineFlows    = "flows"
)

// The levels a Status names. NameOverride is one too.
const (
	NameSafety  = "Safety"
	NameNormal  = "Normal"
	NameDefault = "Default"
)

// Status says who holds an arbitrated capability, and why.
type Status struct {
	Capability string `json:"capability"`
	// Value is the JSON value the device was last given.
	Value json.RawMessage `json:"value,omitempty"`
	// Level is the winning claim's level.
	Level string `json:"level,omitempty"`
	// Claims are the current claims, the winner among them.
	Claims []ClaimStatus `json:"claims,omitempty"`
	// Limit names the limit holding the winner back, such as MinimumOff, until LimitUntil, which is
	// zero if it doesn't end at a time.
	Limit      string    `json:"limit,omitempty"`
	LimitUntil time.Time `json:"limitUntil,omitzero"`
}

// ClaimStatus is one claim on a capability.
type ClaimStatus struct {
	// Source is the path the claim came in by: Matter, Device, Command or Rule/<key>.
	Source string `json:"source"`
	Level  string `json:"level"`
	// Value is the claimed JSON value.
	Value json.RawMessage `json:"value"`
	// Expires is when the claim ends, if it ends at a time.
	Expires time.Time `json:"expires,omitzero"`
	// Latched is true for a latched hold, which lasts until a release action.
	Latched bool `json:"latched,omitempty"`
	// Winner is true for the claim the device takes.
	Winner bool `json:"winner,omitempty"`
	// Reverted is true for an outside change that was written back.
	Reverted bool `json:"reverted,omitempty"`
}

// Entry is how a device arbitrates one capability, as written: a home's declaration in Go, or what
// the host keeps for a device. engine.ControlSettings checks it.
type Entry struct {
	Capability string `json:"capability"`
	// Default is the JSON value the capability has when nothing claims it.
	Default json.RawMessage `json:"default"`
	// Lines are the decision's lines, highest first, as Settings.Lines.
	Lines []string `json:"lines,omitempty"`
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

// Equal reports whether two entries are written the same, byte for byte in Default. An empty
// Default equals a missing one.
func (e Entry) Equal(other Entry) bool {
	same := bytes.Equal(e.Default, other.Default)
	e.Default, other.Default = nil, nil
	return same && reflect.DeepEqual(e, other)
}

// Duration is a time.Duration written as "30m".
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = Duration(v)
	return err
}

// CaseKey is a decision's case as a rule: "<device> <capability> <case>". Its claims' source is
// "Rule/" and the key.
func CaseKey(device, capability, name string) string { return device + " " + capability + " " + name }

// CaseOf is the name of the case whose claims come from source, if it's one of device's
// capability's.
func CaseOf(source, device, capability string) (string, bool) {
	return strings.CutPrefix(source, SourceRule+CaseKey(device, capability, ""))
}

// RenamedKey is a case's key, or an average's, "<device> <capability> <rest>", once its device has
// been renamed, by renamed's old names to new. Any other key is as it is.
func RenamedKey(key string, renamed map[string]string) string {
	if strings.Count(key, " ") != 2 {
		return key
	}
	device, rest, _ := strings.Cut(key, " ")
	if to, ok := renamed[device]; ok {
		return to + " " + rest
	}
	return key
}
