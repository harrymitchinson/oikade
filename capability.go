package oikade

import (
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/engine"
)

// Value is what a capability holds: a bool or a number.
type Value interface{ bool | float64 }

// Capability is the name of a capability whose values are T. The constants below are every
// capability there is.
type Capability[T Value] string

// Bool is a capability that's true or false.
type Bool = Capability[bool]

// Number is a capability that's a number.
type Number = Capability[float64]

// The capabilities, as oikade devices shows them and plugins name them in contract/v1.
const (
	// On is whether a light, switch or outlet is on.
	On Bool = contractv1.On
	// Brightness is a light's brightness in percent, which doesn't turn it on or off.
	Brightness Number = contractv1.Brightness
	// Temperature is in degrees Celsius.
	Temperature Number = contractv1.Temperature
	// Humidity is relative humidity in percent.
	Humidity Number = contractv1.Humidity
	// Open is true while a door or window is open.
	Open Bool = contractv1.Open
	// Occupied is true while motion or presence is detected.
	Occupied Bool = contractv1.Occupied
	// Battery is a battery's charge in percent.
	Battery Number = contractv1.Battery
	// LinkQuality is a radio link's quality, from 0 to 255.
	LinkQuality Number = contractv1.LinkQuality
	// Setpoint is the temperature a heater or thermostat aims for, in degrees Celsius. It doesn't
	// turn it on or off: On does.
	Setpoint Number = contractv1.Setpoint
	// Locked is true while a lock's bolt is fully thrown. A hold's end leaves a lock as it is: only
	// To(false) unlocks it.
	Locked Bool = contractv1.Locked
	// Position is how far closed a blind, shade or shutter is, in percent: 0 is fully open and 100
	// fully closed. A write settles for 2 minutes while it travels.
	Position Number = contractv1.Position
)

// Handle is one capability of one device. oikade types writes one for each capability each of your
// devices has, as PorchLight.On, so that a condition, a trigger or a write naming a capability
// the device doesn't have, or a value of the wrong type, doesn't compile.
type Handle[T Value] struct {
	Device     string
	Capability Capability[T]
}

// To is a write of v to the device: an action, a hold's Set or a Control's default.
func (h Handle[T]) To(v T) Set {
	return Set{device: h.Device, capability: string(h.Capability), value: v}
}

func (h Handle[T]) change() engine.Change {
	return engine.Change{Device: h.Device, Capability: string(h.Capability)}
}

// DeviceType is what a device that's on or off is, in Matter's names, as oikade devices shows it. A
// Group from Kinds is some of them.
type DeviceType string

// Thermostat is a heater or thermostat: any device with a Setpoint.
const Thermostat DeviceType = "Thermostat"

// The device types a device that's on or off can be.
const (
	OnOffLight                 DeviceType = contractv1.OnOffLight
	DimmableLight              DeviceType = contractv1.DimmableLight
	OnOffPlugInUnit            DeviceType = contractv1.OnOffPlugInUnit
	DimmablePlugInUnit         DeviceType = contractv1.DimmablePlugInUnit
	MountedOnOffControl        DeviceType = contractv1.MountedOnOffControl
	MountedDimmableLoadControl DeviceType = contractv1.MountedDimmableLoadControl
)

// Group is which of a room's devices InRoom means: Lights, Plugs, Heaters, Everything, or some
// device types from Kinds. A room write says what it's for, so turning on the lights at dusk doesn't
// also turn on the heater.
type Group struct {
	types []DeviceType
}

// The groups a room's devices fall into, by device type.
var (
	// Lights are the on/off and dimmable lights.
	Lights = Kinds(OnOffLight, DimmableLight)
	// Plugs are the plug-in units and the mounted controls: whatever is wired to a relay or an outlet.
	Plugs = Kinds(OnOffPlugInUnit, DimmablePlugInUnit, MountedOnOffControl, MountedDimmableLoadControl)
	// Heaters are the heaters and thermostats.
	Heaters = Kinds(Thermostat)
	// Everything is every device in the room with the capability, whatever it is: any of its sensors,
	// or every device there that has on, lights, plugs and heaters alike.
	Everything = Group{}
)

// Kinds is a group of the devices of these types, such as only the dimmable lights.
//
//	oikade.InRoom("study", oikade.Kinds(oikade.DimmableLight), oikade.Brightness).To(40)
func Kinds(t DeviceType, more ...DeviceType) Group {
	return Group{types: append([]DeviceType{t}, more...)}
}

// Room is a capability of a room's devices in a group, from InRoom. A room's devices can change
// without a deploy, so it takes the capability rather than handles.
type Room[T Value] struct {
	room       string
	group      Group
	capability Capability[T]
}

// InRoom is capability c of room's devices in group g: a trigger, which fires for a change on any of
// them, a condition's read, as with State's Any, or with To a write to them all.
//
//	oikade.InRoom("living-room", oikade.Lights, oikade.On).To(false) // the lights, not the plugs
//	oikade.InRoom("hallway", oikade.Everything, oikade.Occupied)    // any sensor in the hallway
func InRoom[T Value](room string, g Group, c Capability[T]) Room[T] {
	return Room[T]{room: room, group: g, capability: c}
}

// To is a write of v to the room's devices in the group that have the capability.
func (r Room[T]) To(v T) Set {
	return Set{room: r.room, types: r.engineTypes(), capability: string(r.capability), value: v}
}

func (r Room[T]) change() engine.Change {
	return engine.Change{Room: r.room, DeviceTypes: r.engineTypes(), Capability: string(r.capability)}
}

// engineTypes are the group's device types. None means every device.
func (r Room[T]) engineTypes() []devicetype.Type {
	if len(r.group.types) == 0 {
		return nil
	}
	out := make([]devicetype.Type, 0, len(r.group.types))
	for _, t := range r.group.types {
		out = append(out, devicetype.Type(t))
	}
	return out
}

// Change is what triggers a moment or a flow: a capability changing on a device, as a Handle, or on
// any device in a room, from InRoom.
type Change interface {
	change() engine.Change
}
