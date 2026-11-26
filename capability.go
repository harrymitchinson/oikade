package oikade

import (
	"oikade.dev/contract/v1"
	"oikade.dev/internal/engine"
)

// Value is what a capability holds: a bool, a number or a ClimateMode.
type Value interface{ bool | float64 | ClimateMode }

// ClimateMode is what an air conditioner or heat pump does while it's on: one of ModeAuto, ModeHeat,
// ModeCool, ModeDry and ModeFan. Off isn't one: On is.
type ClimateMode string

// The modes Mode takes. oikade check refuses any other in a decision's Case or Default, and a flow
// that writes one fails its run.
const (
	ModeAuto ClimateMode = contract.ModeAuto
	ModeHeat ClimateMode = contract.ModeHeat
	ModeCool ClimateMode = contract.ModeCool
	ModeDry  ClimateMode = contract.ModeDry
	ModeFan  ClimateMode = contract.ModeFan
)

// Capability is the name of a capability whose values are T. The constants below are every
// capability there is.
type Capability[T Value] string

// The capabilities, as oikade list devices shows them and plugins name them in contract/v1.
const (
	// On is whether a light, switch or outlet is on.
	On Capability[bool] = contract.On
	// Brightness is a light's brightness in percent, which doesn't turn it on or off.
	Brightness Capability[float64] = contract.Brightness
	// Temperature is in degrees Celsius.
	Temperature Capability[float64] = contract.Temperature
	// Humidity is relative humidity in percent.
	Humidity Capability[float64] = contract.Humidity
	// Open is true while a door or window is open.
	Open Capability[bool] = contract.Open
	// Occupied is true while motion or presence is detected.
	Occupied Capability[bool] = contract.Occupied
	// Battery is a battery's charge in percent.
	Battery Capability[float64] = contract.Battery
	// LinkQuality is a radio link's quality, from 0 to 255.
	LinkQuality Capability[float64] = contract.LinkQuality
	// Setpoint is the temperature a heater or thermostat aims for, in degrees Celsius. It doesn't
	// turn it on or off: On does.
	Setpoint Capability[float64] = contract.Setpoint
	// Locked is true while a lock's bolt is fully thrown. A hold's end leaves a lock as it is: only
	// To(false) unlocks it.
	Locked Capability[bool] = contract.Locked
	// Position is how far closed a blind, shade or shutter is, in percent: 0 is fully open and 100
	// fully closed. A write settles for 2 minutes while it travels.
	Position Capability[float64] = contract.Position
	// Mode is what an air conditioner or heat pump does while it's on, such as ModeDry. It's kept
	// while the unit is off: On turns it on and off.
	Mode Capability[ClimateMode] = contract.Mode
)

// Bool is one capability of one device that's true or false, such as PorchLight.On. oikade types
// writes one for each such capability each of your devices has, so that naming a capability the
// device doesn't have, or a value of the wrong type, doesn't compile. A Bool is a condition, true
// while the capability is, and a change, which fires whenever it changes. A group's, such as
// windows.Open, is a change on any member, and Any and All make conditions of it.
type Bool struct {
	// Device is the device's key, as Device.Name has it, or the group's or room's name for a
	// group's capability.
	Device     string
	Capability Capability[bool]
	// g is the group or room the capability is of, or nil for a device's.
	g *group
}

// Number is one capability of one device that's a number, such as Study.Setpoint. oikade types writes
// one for each such capability each of your devices has. Above, Below and Within make conditions of
// it, and it's a change, which fires whenever it changes.
type Number struct {
	// Device is the device's key, as Device.Name has it, or the group's or room's name for a
	// group's capability.
	Device     string
	Capability Capability[float64]
	g          *group
}

// Choice is one capability of one device that's one of a few names, such as BedroomAC.Mode. oikade
// types writes one for each such capability each of your devices has. It's a change, which fires
// whenever it changes.
//
//	home.Decide(BedroomAC.Mode).Case("humid", oikade.ModeDry, BedroomSensor.Humidity.Above(65)).Default(oikade.ModeCool)
type Choice struct {
	// Device is the device's key, as Device.Name has it, or the group's or room's name for a
	// group's capability.
	Device     string
	Capability Capability[ClimateMode]
	g          *group
}

// String is its ref, such as device/porch-light/on, or group/radiators/setpoint for a group's.
func (b Bool) String() string { return b.handle().ref() }

// String is its ref, such as device/study/setpoint, or room/hall/temperature for a room's.
func (n Number) String() string { return n.handle().ref() }

// String is its ref, such as device/bedroom-ac/mode.
func (c Choice) String() string { return c.handle().ref() }

// Ref is a Bool, a Number or a Choice: a capability of one device, or of a group, whose values are
// T, which a decision and oikadetest's helpers take.
type Ref[T Value] interface {
	// To is a write of v to the device, or to the group's members.
	To(v T) Set
	handle() handle
}

// handle is a device's capability, as Bool and Number name it, or a group's.
type handle struct {
	device, capability string
	g                  *group
}

func (h handle) changes() []engine.Change {
	if h.g != nil {
		return groupChanges(h)
	}
	return []engine.Change{{Device: h.device, Capability: h.capability}}
}

// set is a write of v to the device, the group's members that have the capability, or the room's.
func (h handle) set(v any) Set {
	s := Set{device: h.device, capability: h.capability, value: raw(v)}
	if g := h.g; g != nil {
		s.device, s.group = "", g
		if g.room != "" {
			s.room, s.types = g.room, g.types
		} else {
			s.devices = g.has(h.capability)
		}
	}
	return s
}

// To is a write of v to the device, or the group's members: a flow's Set.
func (b Bool) To(v bool) Set { return b.handle().set(v) }

// To is a write of v to the device, or the group's members: a flow's Set.
func (n Number) To(v float64) Set { return n.handle().set(v) }

// To is a write of v to the device, or the group's members: a flow's Set.
func (c Choice) To(v ClimateMode) Set { return c.handle().set(v) }

func (b Bool) handle() handle   { return handle{b.Device, string(b.Capability), b.g} }
func (n Number) handle() handle { return handle{n.Device, string(n.Capability), n.g} }
func (c Choice) handle() handle { return handle{c.Device, string(c.Capability), c.g} }

func (b Bool) changes() []engine.Change   { return b.handle().changes() }
func (n Number) changes() []engine.Change { return n.handle().changes() }
func (c Choice) changes() []engine.Change { return c.handle().changes() }

func (b Bool) checkChange() error   { return checkGroup(b.handle()) }
func (n Number) checkChange() error { return checkGroup(n.handle()) }
func (c Choice) checkChange() error { return checkGroup(c.handle()) }

// raw is a value as the engine holds it: a ClimateMode is its name.
func raw(v any) any {
	if m, ok := v.(ClimateMode); ok {
		return string(m)
	}
	return v
}

// Becomes is a change to v: a trigger that fires when the capability changes to v, and not when it
// changes away from it.
//
//	home.Flow("front-door-arrival", arrival).When(FrontDoor.Open.Becomes(true))
func (b Bool) Becomes(v bool) Change { return becomes{b.handle(), v} }

type becomes struct {
	h  handle
	to bool
}

func (c becomes) checkChange() error { return checkGroup(c.h) }

func (c becomes) changes() []engine.Change {
	out := c.h.changes()
	for i := range out {
		out[i].To = c.to
	}
	return out
}

// DeviceType is what a device is, in Matter's names, as oikade list devices shows it. Kinds takes
// any of these to pick a room's devices. Declaration.Is takes only the types of a device that's on
// or off, since the rest follow from a device's capabilities.
type DeviceType string

// Thermostat is a heater or thermostat: a device with a Setpoint that isn't a RoomAirConditioner.
const Thermostat DeviceType = "Thermostat"

// RoomAirConditioner is an air conditioner or heat pump: a device with a Setpoint, a Mode and an On. A
// room's on covers it with Everything, and Kinds(RoomAirConditioner) means only them.
const RoomAirConditioner DeviceType = "RoomAirConditioner"

// The device types a device that's on or off can be.
const (
	OnOffLight                 DeviceType = contract.OnOffLight
	DimmableLight              DeviceType = contract.DimmableLight
	OnOffPlugInUnit            DeviceType = contract.OnOffPlugInUnit
	DimmablePlugInUnit         DeviceType = contract.DimmablePlugInUnit
	MountedOnOffControl        DeviceType = contract.MountedOnOffControl
	MountedDimmableLoadControl DeviceType = contract.MountedDimmableLoadControl
)

// Kind is which of a room's devices Home.Room means: Lights, Plugs, Heaters, Everything, or some
// device types from Kinds. A room write says what it's for, so turning on the lights at dusk doesn't
// also turn on the heater.
type Kind struct {
	types []DeviceType
}

// The kinds a room's devices fall into, by device type.
var (
	// Lights are the on/off and dimmable lights.
	Lights = Kinds(OnOffLight, DimmableLight)
	// Plugs are the plug-in units and the mounted controls: whatever is wired to a relay or an outlet.
	Plugs = Kinds(OnOffPlugInUnit, DimmablePlugInUnit, MountedOnOffControl, MountedDimmableLoadControl)
	// Heaters are the heaters and thermostats.
	Heaters = Kinds(Thermostat)
	// Everything is every device in the room with the capability, whatever it is: any of its sensors,
	// or every device there that has on, lights, plugs and heaters alike.
	Everything = Kind{}
)

// Kinds is a kind of the devices of these types, such as only the dimmable lights.
//
//	home.Room("study", oikade.Kinds(oikade.DimmableLight)).Brightness.To(40)
func Kinds(t DeviceType, more ...DeviceType) Kind {
	return Kind{types: append([]DeviceType{t}, more...)}
}

// Change is what triggers a flow, in its When: a capability changing on a device, as a Bool, a
// Number or a Choice, a Bool's Becomes, a change on any member of a group or device in a room, or a
// Signal changing.
type Change interface {
	changes() []engine.Change
	// checkChange says what's wrong with the change, for oikade check.
	checkChange() error
}
