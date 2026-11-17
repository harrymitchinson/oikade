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
//   - A choice of a few things is names, as a person says them (dry, fan), not a protocol's
//     numbers. Off isn't one of them when the device has an on: it's On false.
//
// Each name is a judgement call, so a new one is checked against these and the existing names.
// contract/v1 holds the names, package oikade gives them to homes as typed constants, and Spec.Matter
// says what the Matter bridge shows each one as. docs/capabilities.md is generated from here.
package capability

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

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
	// Setpoint is the temperature a heater or thermostat aims for, in degrees Celsius. It doesn't
	// turn the device on or off: On does.
	Setpoint = contractv1.Setpoint
	// Locked is true while a lock's bolt is fully thrown. A bolt partway, or jammed, isn't locked.
	Locked = contractv1.Locked
	// Position is how far closed a blind, shade or shutter is, in percent: 0 is fully open and 100
	// fully closed, as in Matter. A driver whose platform is the other way round, as Zigbee2MQTT's
	// is, inverts it.
	Position = contractv1.Position
	// Mode is what an air conditioner or heat pump does while it's on: one of ModeNames. It's kept
	// while the device is off: On turns it on and off.
	Mode = contractv1.Mode
)

// ModeNames are Mode's names, in the order that gives each its number. The list is append-only.
var ModeNames = []string{contractv1.ModeAuto, contractv1.ModeHeat, contractv1.ModeCool, contractv1.ModeDry, contractv1.ModeFan}

// Kind is a capability's value type.
type Kind string

const (
	KindBool   Kind = "Bool"
	KindNumber Kind = "Number"
	// KindName is one of a few names, such as a mode. Each has a fixed number, its index in Names,
	// which is what claims hold and metrics export, so Names is append-only.
	KindName Kind = "Name"
)

// Spec describes a capability's type and valid range.
type Spec struct {
	// Go is the capability's name in a home: package oikade's constant, and a handle's field.
	Go       string
	Kind     Kind
	Writable bool
	// Min and Max bound numeric values (inclusive).
	Min, Max float64
	// Unit is what a number is in, for people.
	Unit string
	// Names are a KindName capability's values, in the order of their numbers.
	Names []string
	// Matter is the Matter attribute the bridge shows it as, if any.
	Matter *Matter
	// Settle is how long after a write a report of another value is the device catching up, as a
	// blind does while it travels, rather than somebody changing it. Zero is DefaultSettle.
	Settle time.Duration
	// NoEndWrite is a bool a hold's end leaves as it is, rather than writing the opposite: only an
	// explicit write unlocks a door. A Budget, which turns its target on and off, can't have it.
	NoEndWrite bool
	// Step is what a number a hold follows is rounded to, so a reading's every wobble isn't a write.
	// Half of it, and 0.5 at most, is how far a report may be from a write and still be it (Near).
	// Zero is none: a Follow of it gives its own.
	Step float64
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
	Brightness: {Go: "Brightness", Kind: KindNumber, Writable: true, Min: 0, Max: 100, Unit: "%", Step: 1, Matter: &Matter{
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
	Setpoint: {Go: "Setpoint", Kind: KindNumber, Writable: true, Min: 0, Max: 50, Unit: "°C", Step: 0.5, Matter: &Matter{
		Cluster: "Thermostat", ClusterID: 0x0201, Attribute: "OccupiedHeatingSetpoint", AttributeID: 0x0012,
	}},
	Position: {Go: "Position", Kind: KindNumber, Writable: true, Min: 0, Max: 100, Unit: "%", Settle: 2 * time.Minute, Step: 1, Matter: &Matter{
		Cluster: "Window Covering", ClusterID: 0x0102, Attribute: "CurrentPositionLiftPercent100ths", AttributeID: 0x000E,
	}},
	Mode: {Go: "Mode", Kind: KindName, Writable: true, Names: ModeNames, Matter: &Matter{
		Cluster: "Thermostat", ClusterID: 0x0201, Attribute: "SystemMode", AttributeID: 0x001C,
	}},
	Locked: {Go: "Locked", Kind: KindBool, Writable: true, NoEndWrite: true, Settle: 30 * time.Second, Matter: &Matter{
		Cluster: "Door Lock", ClusterID: 0x0101, Attribute: "LockState", AttributeID: 0x0000,
	}},
}

// DefaultSettle is how long a write settles for a capability that doesn't say: a relay or a light
// reports within it.
const DefaultSettle = 5 * time.Second

// SettleOf is how long a write to the capability settles: its Settle, or DefaultSettle.
func SettleOf(name string) time.Duration {
	if s, ok := Lookup(name); ok && s.Settle > 0 {
		return s.Settle
	}
	return DefaultSettle
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

// Validate checks v against the capability and returns it normalised to bool, float64 or, for a
// KindName, string. Values of the wrong kind or out of range are rejected, never coerced.
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
	case KindName:
		n, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s takes one of %s, got %T", name, strings.Join(s.Names, ", "), v)
		}
		if !slices.Contains(s.Names, n) {
			return nil, fmt.Errorf("%s is one of %s, not %q", name, strings.Join(s.Names, ", "), n)
		}
		return n, nil
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

// tolerance is how far a reported number may be from a written one and still be it. Drivers round
// on the way to and from the device: a brightness of 40 comes back from Zigbee2MQTT as 40.2. Bools
// are 0 and 1, and names a whole number apart, so it can't confuse them.
const tolerance = 0.5

// Near reports whether a number reported for a capability is one written to it, allowing for the
// rounding drivers do: within half a step of it, and never more than tolerance, so a setpoint turned
// up by its 0.5 °C step is still a change.
func Near(name string, reported, wrote float64) bool {
	tol := tolerance
	if s, ok := Lookup(name); ok && s.Step > 0 {
		tol = min(tol, s.Step/2)
	}
	return math.Abs(reported-wrote) <= tol
}

// Float converts a validated value to a float64 for metrics and claims: bools become 0 or 1, and a
// name its number.
func Float(v any) float64 {
	switch v := v.(type) {
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		return numbers[v]
	}
	f, _ := toFloat(v)
	return f
}

// numbers are every KindName capability's names' numbers. A name means one number in all of them,
// which the registry's tests check, so Float needs no capability.
var numbers = func() map[string]float64 {
	out := map[string]float64{}
	for _, s := range registry {
		for i, n := range s.Names {
			out[n] = float64(i)
		}
	}
	return out
}()

// NameOf is the name a KindName capability's number stands for, and false for a number that isn't
// one of them.
func NameOf(name string, f float64) (string, bool) {
	s, ok := registry[name]
	if !ok || s.Kind != KindName || f != math.Trunc(f) || f < 0 || int(f) >= len(s.Names) {
		return "", false
	}
	return s.Names[int(f)], true
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

// Averaged is the capability a rolling average's name, such as "temperature over 24h", averages,
// or name itself when it isn't one.
func Averaged(name string) string {
	base, _, _ := strings.Cut(name, " over ")
	return base
}
