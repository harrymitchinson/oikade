// Package capability is the registry of protocol-neutral capabilities. Drivers translate to and
// from these; Matter, metrics and rules only ever see them.
package capability

import (
	"encoding/json"
	"fmt"
	"math"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

// Canonical capability names.
const (
	// LightOn is a light's on/off state.
	LightOn = "light.on"
	// LightLevel is brightness in percent (0-100). It is independent of LightOn: setting level 0
	// does not turn the light off, and drivers report LightOn changes separately.
	LightLevel = "light.level"
	// SwitchOn is a generic switch. Prefer LightOn or OutletOn when the physical class is known.
	SwitchOn = "switch.on"
	// OutletOn is a smart plug's relay.
	OutletOn = "outlet.on"
	// Temperature is in degrees Celsius.
	Temperature = "sensor.temperature"
	// Humidity is relative humidity in percent.
	Humidity = "sensor.relative-humidity"
	// ContactOpen is true when a door or window is open. Note Zigbee2MQTT's "contact" is the inverse.
	ContactOpen = "sensor.contact-open"
	// Occupancy is true while motion or presence is detected.
	Occupancy = "sensor.occupancy-detected"
	// Battery is battery charge in percent.
	Battery = "battery.level"
	// LinkQuality is the Zigbee link quality indicator (0-255).
	LinkQuality = "link.quality"
)

// Spec describes a capability's type and valid range.
type Spec struct {
	Kind     homev1alpha1.CapabilityKind
	Writable bool
	// Min and Max bound numeric values (inclusive).
	Min, Max float64
}

var registry = map[string]Spec{
	LightOn:     {Kind: homev1alpha1.CapabilityKindBool, Writable: true},
	LightLevel:  {Kind: homev1alpha1.CapabilityKindNumber, Writable: true, Min: 0, Max: 100},
	SwitchOn:    {Kind: homev1alpha1.CapabilityKindBool, Writable: true},
	OutletOn:    {Kind: homev1alpha1.CapabilityKindBool, Writable: true},
	Temperature: {Kind: homev1alpha1.CapabilityKindNumber, Min: -273.15, Max: 1000},
	Humidity:    {Kind: homev1alpha1.CapabilityKindNumber, Min: 0, Max: 100},
	ContactOpen: {Kind: homev1alpha1.CapabilityKindBool},
	Occupancy:   {Kind: homev1alpha1.CapabilityKindBool},
	Battery:     {Kind: homev1alpha1.CapabilityKindNumber, Min: 0, Max: 100},
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
