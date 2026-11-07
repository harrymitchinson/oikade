// Package capability is the registry of protocol-neutral capabilities. Drivers translate to and
// from these; Matter, metrics and rules only ever see them.
//
// # Naming a capability
//
// A capability is named for the person writing a rule, not for a protocol:
//
//   - What a person would say: "is the porch occupied", not "occupancy-detected".
//   - One word if it can be, in kebab case if it can't (link-quality). No protocol, vendor or
//     domain prefix, because any driver reports it: not zigbee.link-quality or sensor.temperature.
//   - What the device is, a light or an outlet, isn't in the name: one on serves both.
//   - A bool is true in the state its name says: open is true while the door is open, whatever
//     Zigbee2MQTT's contact or Matter's Boolean State say. Inverting is the mapping's job.
//   - Units are the ones a person writes: degrees Celsius and percent, never a protocol's scale
//     such as 0-254.
//
// Each name is a judgement call, so a new one is checked against these and the existing names.
// contract/v1 holds the names, framework gives them to homes as typed constants, and Spec.Matter
// says what the Matter bridge shows each one as. docs/capabilities.md is generated from here.
package capability

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// The capabilities. contract/v1 holds their names.
const (
	// On is whether a light, switch or outlet is on.
	On = contractv1.On
	// Brightness is a light's brightness in percent (0-100). It doesn't turn the light on or off:
	// drivers report On separately.
	Brightness = contractv1.Brightness
	// Temperature is in degrees Celsius.
	Temperature = contractv1.Temperature
	// Humidity is relative humidity in percent.
	Humidity = contractv1.Humidity
	// Open is true while a door or window is open. Zigbee2MQTT's "contact" and Matter's Boolean
	// State are both true while it's shut.
	Open = contractv1.Open
	// Occupied is true while motion or presence is detected.
	Occupied = contractv1.Occupied
	// Battery is a battery's charge in percent.
	Battery = contractv1.Battery
	// LinkQuality is a radio link's quality, from 0 to 255.
	LinkQuality = contractv1.LinkQuality
)

// Kind is a capability's value type.
type Kind string

const (
	KindBool   Kind = "Bool"
	KindNumber Kind = "Number"
)

// Spec describes a capability's type and valid range.
type Spec struct {
	// Go is the capability's name in a home: framework's constant, and a handle's field.
	Go       string
	Kind     Kind
	Writable bool
	// Min and Max bound numeric values (inclusive).
	Min, Max float64
	// Unit is what a number is in, for people.
	Unit string
	// Matter is the Matter attribute the bridge shows it as, if any.
	Matter *Matter
}

// Matter is a Matter cluster's attribute.
type Matter struct {
	Cluster, Attribute     string
	ClusterID, AttributeID uint32
	// Inverted is true when the attribute is true while the capability is false.
	Inverted bool
}

var registry = map[string]Spec{
	On: {Go: "On", Kind: KindBool, Writable: true, Matter: &Matter{
		Cluster: "On/Off", ClusterID: 0x0006, Attribute: "OnOff", AttributeID: 0x0000,
	}},
	Brightness: {Go: "Brightness", Kind: KindNumber, Writable: true, Min: 0, Max: 100, Unit: "%", Matter: &Matter{
		Cluster: "Level Control", ClusterID: 0x0008, Attribute: "CurrentLevel", AttributeID: 0x0000,
	}},
	Temperature: {Go: "Temperature", Kind: KindNumber, Min: -273.15, Max: 1000, Unit: "°C", Matter: &Matter{
		Cluster: "Temperature Measurement", ClusterID: 0x0402, Attribute: "MeasuredValue", AttributeID: 0x0000,
	}},
	Humidity: {Go: "Humidity", Kind: KindNumber, Min: 0, Max: 100, Unit: "%", Matter: &Matter{
		Cluster: "Relative Humidity Measurement", ClusterID: 0x0405, Attribute: "MeasuredValue", AttributeID: 0x0000,
	}},
	Open: {Go: "Open", Kind: KindBool, Matter: &Matter{
		Cluster: "Boolean State", ClusterID: 0x0045, Attribute: "StateValue", AttributeID: 0x0000, Inverted: true,
	}},
	Occupied: {Go: "Occupied", Kind: KindBool, Matter: &Matter{
		Cluster: "Occupancy Sensing", ClusterID: 0x0406, Attribute: "Occupancy", AttributeID: 0x0000,
	}},
	Battery: {Go: "Battery", Kind: KindNumber, Min: 0, Max: 100, Unit: "%", Matter: &Matter{
		Cluster: "Power Source", ClusterID: 0x002F, Attribute: "BatPercentRemaining", AttributeID: 0x000C,
	}},
	LinkQuality: {Go: "LinkQuality", Kind: KindNumber, Min: 0, Max: 255},
}

// Names are every capability's name, sorted.
func Names() []string {
	return slices.Sorted(maps.Keys(registry))
}

// Lookup returns the spec for a capability name.
func Lookup(name string) (Spec, bool) {
	s, ok := registry[name]
	return s, ok
}

// Validate checks v against the capability and returns it normalised to bool or float64.
// Values of the wrong kind or out of range are rejected, never coerced.
func Validate(name string, v any) (any, error) {
	s, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown capability %q", name)
	}
	switch s.Kind {
	case KindBool:
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
