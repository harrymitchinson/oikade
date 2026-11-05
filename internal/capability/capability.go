// Package capability is the registry of protocol-neutral capabilities. Drivers translate to and
// from these; Matter, metrics and rules only ever see them.
package capability

import (
	"encoding/json"
	"fmt"
	"math"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

// Canonical capability names. A capability is one Matter cluster attribute, named after that
// attribute, or after what it measures where the attribute's name is generic (OIK-135). One that
// Matter has no attribute for is named after its source. Values keep readable units: Matter's wire
// encodings stay in internal/matterbridge.
const (
	// OnOff is whether a device is on. What kind of device it is, such as a light or a plug, isn't
	// part of the capability.
	OnOff = "on-off"
	// CurrentLevel is the level in percent (0-100), such as a light's brightness. It is independent
	// of OnOff: setting level 0 does not turn the device off, and drivers report OnOff changes
	// separately.
	CurrentLevel = "current-level"
	// Temperature is in degrees Celsius.
	Temperature = "temperature"
	// RelativeHumidity is relative humidity in percent.
	RelativeHumidity = "relative-humidity"
	// Contact is true when a contact sensor's two parts touch: a door or window is shut. It's the
	// same way round as Matter's Boolean State and Zigbee2MQTT's contact.
	Contact = "contact"
	// Occupancy is true while motion or presence is detected.
	Occupancy = "occupancy"
	// BatPercentRemaining is battery charge in percent.
	BatPercentRemaining = "bat-percent-remaining"
	// LinkQuality is the Zigbee link quality indicator (0-255). Matter has no attribute for it.
	LinkQuality = "zigbee.link-quality"
)

// Spec describes a capability's type and valid range, and the Matter attribute it is.
type Spec struct {
	Kind     homev1alpha1.CapabilityKind
	Writable bool
	// Min and Max bound numeric values (inclusive).
	Min, Max float64
	// Cluster and Attribute are the capability's Matter cluster and attribute, as the spec names
	// them. They're empty for a capability Matter has no attribute for.
	Cluster, Attribute string
}

var registry = map[string]Spec{
	OnOff: {Kind: homev1alpha1.CapabilityKindBool, Writable: true,
		Cluster: "On/Off", Attribute: "OnOff"},
	CurrentLevel: {Kind: homev1alpha1.CapabilityKindNumber, Writable: true, Min: 0, Max: 100,
		Cluster: "Level Control", Attribute: "CurrentLevel"},
	Temperature: {Kind: homev1alpha1.CapabilityKindNumber, Min: -273.15, Max: 1000,
		Cluster: "Temperature Measurement", Attribute: "MeasuredValue"},
	RelativeHumidity: {Kind: homev1alpha1.CapabilityKindNumber, Min: 0, Max: 100,
		Cluster: "Relative Humidity Measurement", Attribute: "MeasuredValue"},
	Contact: {Kind: homev1alpha1.CapabilityKindBool,
		Cluster: "Boolean State", Attribute: "StateValue"},
	Occupancy: {Kind: homev1alpha1.CapabilityKindBool,
		Cluster: "Occupancy Sensing", Attribute: "Occupancy"},
	BatPercentRemaining: {Kind: homev1alpha1.CapabilityKindNumber, Min: 0, Max: 100,
		Cluster: "Power Source", Attribute: "BatPercentRemaining"},
	LinkQuality: {Kind: homev1alpha1.CapabilityKindNumber, Min: 0, Max: 255},
}

// Lookup returns the spec for a capability name.
func Lookup(name string) (Spec, bool) {
	s, ok := registry[name]
	return s, ok
}

// Status returns the Device status entry for a capability.
func Status(name string) (homev1alpha1.DeviceCapability, bool) {
	s, ok := registry[name]
	return homev1alpha1.DeviceCapability{Name: name, Kind: s.Kind, Writable: s.Writable}, ok
}

// Validate checks v against the capability and returns it normalised to bool or float64.
// Values of the wrong kind or out of range are rejected, never coerced.
func Validate(name string, v any) (any, error) {
	s, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown capability %q", name)
	}
	switch s.Kind {
	case homev1alpha1.CapabilityKindBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s takes a bool, got %T", name, v)
		}
		return b, nil
	default:
		f, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("%s takes a number, got %T", name, v)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) || f < s.Min || f > s.Max {
			return nil, fmt.Errorf("%s must be between %g and %g, got %g", name, s.Min, s.Max, f)
		}
		return f, nil
	}
}

// ParseJSON decodes a JSON scalar and validates it for the capability.
func ParseJSON(name string, raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return Validate(name, v)
}

// Float converts a validated value to a float64 for metrics: bools become 0 or 1.
func Float(v any) float64 {
	if b, ok := v.(bool); ok {
		if b {
			return 1
		}
		return 0
	}
	f, _ := toFloat(v)
	return f
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
