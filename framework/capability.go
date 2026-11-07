package framework

import (
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
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
)

// Handle is one capability of one device. oikade types writes one for each capability each of your
// devices has, as PorchLight.On, so that a condition, a trigger or a write naming a capability
// the device doesn't have, or a value of the wrong type, doesn't compile.
type Handle[T Value] struct {
	Device     string
	Capability Capability[T]
}

// To is a write of v to the device: an action, a hold's Set or a Control's Default.
func (h Handle[T]) To(v T) Set {
	return Set{device: h.Device, capability: string(h.Capability), value: v}
}

func (h Handle[T]) change() engine.Change {
	return engine.Change{Device: h.Device, Capability: string(h.Capability)}
}

// Room is a capability of every device in a room that has it, from InRoom. A room's devices can
// change without a deploy, so it takes the capability rather than handles.
type Room[T Value] struct {
	room       string
	capability Capability[T]
}

// InRoom is capability c of every device in room: a trigger, or with To a write to them all.
func InRoom[T Value](room string, c Capability[T]) Room[T] {
	return Room[T]{room: room, capability: c}
}

// To is a write of v to every device in the room with the capability.
func (r Room[T]) To(v T) Set {
	return Set{room: r.room, capability: string(r.capability), value: v}
}

func (r Room[T]) change() engine.Change {
	return engine.Change{Room: r.room, Capability: string(r.capability)}
}

// Change is what triggers a moment or a flow: a capability changing on a device, as a Handle, or on
// any device in a room, from InRoom.
type Change interface {
	change() engine.Change
}
