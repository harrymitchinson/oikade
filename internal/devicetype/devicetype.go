// Package devicetype says what a device is, in Matter's names: an on/off light, a plug, a wall
// switch, or a sensor. A device's capabilities say what it can do, and only a device that's on or off
// can be more than one thing, so that's the type you or its driver can choose. The rest follow from
// its capabilities. One function, Of, gives the answer that the Matter bridge and oikade devices
// read.
package devicetype

import (
	"slices"
	"strings"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/capability"
)

// Type is a Matter device type's name.
type Type string

// The device types of a device that's on or off, which you or its driver choose between. They're
// the plugin contract's, so a plugin can report any of them.
const (
	OnOffLight                 Type = contractv1.OnOffLight
	DimmableLight              Type = contractv1.DimmableLight
	OnOffPlugInUnit            Type = contractv1.OnOffPlugInUnit
	DimmablePlugInUnit         Type = contractv1.DimmablePlugInUnit
	MountedOnOffControl        Type = contractv1.MountedOnOffControl
	MountedDimmableLoadControl Type = contractv1.MountedDimmableLoadControl
)

// The device types that follow from a sensor's or a lock's capabilities.
const (
	OccupancySensor   Type = "OccupancySensor"
	ContactSensor     Type = "ContactSensor"
	TemperatureSensor Type = "TemperatureSensor"
	HumiditySensor    Type = "HumiditySensor"
	DoorLock          Type = "DoorLock"
	WindowCovering    Type = "WindowCovering"
)

// Thermostat is a heater or thermostat: a device with a setpoint. Its on is the heater's, so it isn't
// also a light or a plug, and its temperature is what it heats to, so it isn't a Temperature Sensor.
const Thermostat Type = "Thermostat"

// RoomAirConditioner is an air conditioner or heat pump: a device with a setpoint and a mode. Its
// on and temperature are its own, as a Thermostat's are.
const RoomAirConditioner Type = "RoomAirConditioner"

// OnOff are the types a device that's on or off can be, in the order an error lists them.
var OnOff = []Type{OnOffLight, DimmableLight, OnOffPlugInUnit, DimmablePlugInUnit, MountedOnOffControl, MountedDimmableLoadControl}

// dimmed is each dimmable type's on/off sibling, for a device that has no brightness to dim.
var dimmed = map[Type]Type{
	DimmableLight:              OnOffLight,
	DimmablePlugInUnit:         OnOffPlugInUnit,
	MountedDimmableLoadControl: MountedOnOffControl,
}

// followed are the types that follow from a capability, in the order the Matter bridge adds them.
var followed = []struct {
	capability string
	typ        Type
}{
	{capability.Locked, DoorLock},
	{capability.Position, WindowCovering},
	{capability.Occupied, OccupancySensor},
	{capability.Open, ContactSensor},
	{capability.Temperature, TemperatureSensor},
	{capability.Humidity, HumiditySensor},
}

// Join lists types, comma-separated.
func Join(types []Type) string {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}

// Any reports whether have has one of want. No want means anything does.
func Any(have, want []Type) bool {
	return len(want) == 0 || slices.ContainsFunc(have, func(t Type) bool { return slices.Contains(want, t) })
}

// Rooms are the types a room rule can be limited to: the on/off types, Thermostat and
// RoomAirConditioner.
var Rooms = append(slices.Clone(OnOff), Thermostat, RoomAirConditioner)

// ForRooms says a room rule can be limited to t.
func ForRooms(t Type) bool { return slices.Contains(Rooms, t) }

// Choosable says t is one you or a driver can choose for a device that's on or off.
func Choosable(t Type) bool { return slices.Contains(OnOff, t) }

// Dimmable says t dims, so it needs a brightness.
func Dimmable(t Type) bool {
	_, ok := dimmed[t]
	return ok
}

// Light says t is a light.
func Light(t Type) bool { return t == OnOffLight || t == DimmableLight }

// OnOffOf is the type of a device that's on or off: yours, else its driver's, else a dimmable light
// with a brightness and a plug without, since a bare relay could be driving anything. A dimmable type
// on a device with no brightness is its on/off sibling, as there's nothing to dim. It's empty for a
// device that isn't on or off, and for a thermostat or an air conditioner, whose on is its own.
func OnOffOf(yours, drivers Type, capabilities []string) Type {
	if !slices.Contains(capabilities, capability.On) || slices.Contains(capabilities, capability.Setpoint) {
		return ""
	}
	brightness := slices.Contains(capabilities, capability.Brightness)
	t := yours
	if !Choosable(t) {
		t = drivers
	}
	if !Choosable(t) {
		t = OnOffPlugInUnit
		if brightness {
			t = DimmableLight
		}
	}
	if sibling, ok := dimmed[t]; ok && !brightness {
		t = sibling
	}
	return t
}

// Of is a device's device types: its on/off type, Thermostat or RoomAirConditioner, then any that
// follow from its other capabilities. A battery is a power source, which Matter shows on the device rather than as what it
// is, so it isn't one.
func Of(yours, drivers Type, capabilities []string) []Type {
	var out []Type
	thermostat := slices.Contains(capabilities, capability.Setpoint)
	switch {
	case thermostat && slices.Contains(capabilities, capability.Mode):
		out = append(out, RoomAirConditioner)
	case thermostat:
		out = append(out, Thermostat)
	default:
		if t := OnOffOf(yours, drivers, capabilities); t != "" {
			out = append(out, t)
		}
	}
	for _, s := range followed {
		if thermostat && s.capability == capability.Temperature {
			continue
		}
		if slices.Contains(capabilities, s.capability) {
			out = append(out, s.typ)
		}
	}
	return out
}
