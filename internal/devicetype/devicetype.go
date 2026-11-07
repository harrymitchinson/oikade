// Package devicetype says what a device is, in Matter's names: an on/off light, a plug, a wall
// switch, or a sensor. A device's capabilities say what it can do, and only a device that's on or off
// can be more than one thing, so that's the type you or its driver can choose. The rest follow from
// its capabilities. One function, Of, gives the answer that the Matter bridge and oikade devices
// read.
package devicetype

import (
	"slices"
	"strings"

	"github.com/harrymitchinson/oikade/internal/capability"
)

// Type is a Matter device type's name.
type Type string

// The device types of a device that's on or off, which you or its driver choose between.
const (
	OnOffLight                 Type = "OnOffLight"
	DimmableLight              Type = "DimmableLight"
	OnOffPlugInUnit            Type = "OnOffPlugInUnit"
	DimmablePlugInUnit         Type = "DimmablePlugInUnit"
	MountedOnOffControl        Type = "MountedOnOffControl"
	MountedDimmableLoadControl Type = "MountedDimmableLoadControl"
)

// The device types that follow from a sensor's capabilities.
const (
	OccupancySensor   Type = "OccupancySensor"
	ContactSensor     Type = "ContactSensor"
	TemperatureSensor Type = "TemperatureSensor"
	HumiditySensor    Type = "HumiditySensor"
)

// OnOff are the types a device that's on or off can be, in the order oikade devices type lists them.
var OnOff = []Type{OnOffLight, DimmableLight, OnOffPlugInUnit, DimmablePlugInUnit, MountedOnOffControl, MountedDimmableLoadControl}

// dimmed is each dimmable type's on/off sibling, for a device that has no brightness to dim.
var dimmed = map[Type]Type{
	DimmableLight:              OnOffLight,
	DimmablePlugInUnit:         OnOffPlugInUnit,
	MountedDimmableLoadControl: MountedOnOffControl,
}

// sensors are the types that follow from a capability, in the order the Matter bridge adds them.
var sensors = []struct {
	capability string
	typ        Type
}{
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
// device that isn't on or off.
func OnOffOf(yours, drivers Type, capabilities []string) Type {
	if !slices.Contains(capabilities, capability.On) {
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

// Of is a device's device types: its on/off type, then any that follow from its other capabilities.
// A battery is a power source, which Matter shows on the device rather than as what it is, so it
// isn't one.
func Of(yours, drivers Type, capabilities []string) []Type {
	var out []Type
	if t := OnOffOf(yours, drivers, capabilities); t != "" {
		out = append(out, t)
	}
	for _, s := range sensors {
		if slices.Contains(capabilities, s.capability) {
			out = append(out, s.typ)
		}
	}
	return out
}
