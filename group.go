package oikade

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/ident"
)

// Group is some devices together, from Home.Group or Home.Room, with a field for each capability. A
// group's capability is a Bool, a Number or a Choice that a decision, a flow's Set and a trigger take,
// and a Bool's Any and All are conditions:
//
//	radiators := home.Group("radiators", BedroomRadiator, StudyRadiator, KitchenRadiator)
//	home.Decide(radiators.Setpoint).Case("windows-open", 7, windows.Open.Any()).Default(18)
//
// Every capability has a field, so radiators.Humidity compiles, and oikade check refuses one no
// member has.
type Group struct {
	// Name is the group's name, or the room's.
	Name string

	On       Bool
	Open     Bool
	Occupied Bool
	Locked   Bool

	Brightness  Number
	Temperature Number
	Humidity    Number
	Battery     Number
	LinkQuality Number
	Setpoint    Number
	Position    Number

	Mode Choice
}

// group is what a group's capabilities know of it.
type group struct {
	name string
	// room is a room's name, whose members are whichever devices are there now, of types.
	room  string
	types []devicetype.Type
	// members are a group's devices, in the order given, and what each has.
	members []groupMember
}

type groupMember struct {
	name         string
	capabilities []string
}

// has says which members have capability, in the order given.
func (g *group) has(capability string) []string {
	var out []string
	for _, m := range g.members {
		if slices.Contains(m.capabilities, capability) {
			out = append(out, m.name)
		}
	}
	return out
}

// what names the group in a mistake.
func (g *group) what() string {
	if g.room != "" {
		return "room " + g.room
	}
	return "group " + g.name
}

// Group is some devices together, under a name of their own:
//
//	radiators := home.Group("radiators", BedroomRadiator, StudyRadiator, KitchenRadiator)
//
// Its name is how oikade status and pause name its decision's cases, as with a device's, so it can't
// be a device's name too.
func (h *Home) Group(name string, members ...member) Group {
	g := &group{name: name}
	bad := func(err error) { h.mistakes = append(h.mistakes, fmt.Errorf("group %q: %w", name, err)) }
	if err := ident.CheckName(ident.Group, name); err != nil {
		h.mistakes = append(h.mistakes, err)
	}
	if len(members) == 0 {
		bad(errors.New("it needs a member"))
	}
	for _, m := range members {
		if v := reflect.ValueOf(m); m == nil || v.Kind() == reflect.Pointer && v.IsNil() {
			bad(errors.New("it has a nil member"))
			continue
		}
		gm := groupMember{name: m.hardware().Key, capabilities: capabilitiesOf(m)}
		switch {
		case gm.name == "":
			bad(errors.New("it has a member with no name"))
		case gm.name == name:
			bad(errors.New("it has a member of its own name"))
		case slices.ContainsFunc(g.members, func(o groupMember) bool { return o.name == gm.name }):
			bad(fmt.Errorf("%s is a member twice", gm.name))
		}
		g.members = append(g.members, gm)
	}
	h.groups = append(h.groups, g)
	return g.fields()
}

// Room is the devices in a room now, of the kinds given, as a Group, or every one, as the room is,
// with none:
//
//	home.Room(Salon, oikade.Lights).On.To(false) // the lights, not the plugs
//
// A room's devices change as a device is paired, removed or placed in another room, so what it
// covers is worked out when it's read or written. A device in the room with a decision of its own on a
// capability keeps it, rather than taking the room's.
func (h *Home) Room(room RoomGroup, kinds ...Kind) Group {
	if room.g == nil {
		h.mistakes = append(h.mistakes, errors.New("Room needs a room from oikade.Room"))
		return Group{}
	}
	var types []devicetype.Type
	for _, k := range kinds {
		if len(k.types) == 0 {
			// Everything is every device.
			types = nil
			break
		}
		for _, t := range k.types {
			if !slices.Contains(types, t) {
				types = append(types, t)
			}
		}
	}
	if len(types) == 0 {
		return room.Group
	}
	// A room of these kinds is one group however often it's made, so a Then finds its decision.
	if i := slices.IndexFunc(h.groups, func(o *group) bool {
		return o.room == room.g.room && sameTypes(o.types, types)
	}); i >= 0 {
		return h.groups[i].fields()
	}
	g := &group{name: room.g.name, room: room.g.room, types: types}
	h.groups = append(h.groups, g)
	return g.fields()
}

// sameTypes says a and b have the same device types, in any order.
func sameTypes(a, b []devicetype.Type) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// fields are g's capabilities.
func (g *group) fields() Group {
	out := Group{Name: g.name}
	v := reflect.ValueOf(&out).Elem()
	for i := range v.NumField() {
		switch f := v.Field(i).Addr().Interface().(type) {
		case *Bool:
			*f = Bool{Device: g.name, Capability: Capability[bool](capabilityNamed(v.Type().Field(i).Name)), g: g}
		case *Number:
			*f = Number{Device: g.name, Capability: Capability[float64](capabilityNamed(v.Type().Field(i).Name)), g: g}
		case *Choice:
			*f = Choice{Device: g.name, Capability: Capability[ClimateMode](capabilityNamed(v.Type().Field(i).Name)), g: g}
		}
	}
	return out
}

// capabilityNamed is the capability whose field in Group is field.
func capabilityNamed(field string) string {
	c, ok := map[string]string{
		"On": string(On), "Open": string(Open), "Occupied": string(Occupied), "Locked": string(Locked),
		"Brightness": string(Brightness), "Temperature": string(Temperature), "Humidity": string(Humidity),
		"Battery": string(Battery), "LinkQuality": string(LinkQuality), "Setpoint": string(Setpoint),
		"Position": string(Position), "Mode": string(Mode),
	}[field]
	if !ok {
		panic("no capability for Group." + field)
	}
	return c
}

// capabilitiesOf are the capabilities a device has: its Bool, Number and Choice fields, its device
// type's among them.
func capabilitiesOf(m member) []string {
	v := reflect.ValueOf(m)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	return fieldCapabilities(v)
}

func fieldCapabilities(v reflect.Value) []string {
	if v.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for field, f := range v.Fields() {
		switch c := f.Interface().(type) {
		case Bool:
			out = append(out, string(c.Capability))
		case Number:
			out = append(out, string(c.Capability))
		case Choice:
			out = append(out, string(c.Capability))
		default:
			if field.Anonymous {
				out = append(out, fieldCapabilities(f)...)
			}
		}
	}
	return out
}

// checkGroup refuses a group's capability that no member has. A room's members aren't known until
// it's read.
func checkGroup(h handle) error {
	if h.g == nil || h.g.room != "" || len(h.g.has(h.capability)) > 0 {
		return nil
	}
	return fmt.Errorf("no member of %s has %s", h.g.what(), h.capability)
}

// groupNotReported ends a condition that read a group where no member has reported the capability.
type groupNotReported struct{ group, capability string }

// groupValues are the reported values of a group's or room's capability, in member order for a
// group and device name order for a room.
func groupValues[T Value](s Snapshot, h handle) []T {
	g := h.g
	if g == nil {
		panic(fmt.Sprintf("%s %s isn't a group's or a room's: read it with Get", h.device, h.capability))
	}
	if g.room != "" {
		return roomValues[T](s, g.room, g.types, h.capability)
	}
	var out []T
	for _, name := range g.has(h.capability) {
		if v, ok := s.devices[name][h.capability]; ok {
			out = append(out, as[T](name, h.capability, v))
		}
	}
	if len(out) == 0 {
		panic(groupNotReported{g.name, h.capability})
	}
	return out
}

// groupChanges are a change on any of a group's members, or a room's devices.
func groupChanges(h handle) []engine.Change {
	if h.g.room != "" {
		return []engine.Change{{Room: h.g.room, DeviceTypes: h.g.types, Capability: h.capability}}
	}
	var out []engine.Change
	for _, name := range h.g.has(h.capability) {
		out = append(out, engine.Change{Device: name, Capability: h.capability})
	}
	return out
}

// Any is true while the capability is true on any of the group's members that have reported it, such
// as windows.Open.Any(). It needs a group's or a room's capability.
func (b Bool) Any() Condition { return groupCondition{b: b} }

// All is true while the capability is true on all of the group's members that have reported it.
func (b Bool) All() Condition { return groupCondition{b: b, all: true} }

// groupCondition is a group's Any or All.
type groupCondition struct {
	b   Bool
	all bool
}

func (c groupCondition) holds(s Snapshot) bool {
	if c.all {
		return s.All(c.b)
	}
	return s.Any(c.b)
}

func (c groupCondition) explain(s Snapshot, want bool) string {
	what := c.b.handle().said()
	switch {
	case c.all && want:
		return fmt.Sprintf("%s isn't true on all of them", what)
	case c.all:
		return fmt.Sprintf("%s is true on all of them", what)
	case want:
		return fmt.Sprintf("%s isn't true on any of them", what)
	}
	return fmt.Sprintf("%s is true on %d of them", what, s.Count(c.b))
}

func (c groupCondition) check(Options) error {
	h := c.b.handle()
	if h.g == nil {
		if c.all {
			return fmt.Errorf("All of %s needs a group's or a room's capability, such as windows.Open", h.named())
		}
		return fmt.Errorf("Any of %s needs a group's or a room's capability, such as windows.Open", h.named())
	}
	return checkGroup(h)
}

// ref is its ref: a device's capability's, a room's or a group's.
func (h handle) ref() string {
	switch {
	case h.g == nil:
		return ident.CapabilityRef(h.device, h.capability).String()
	case h.g.room != "":
		return ident.NewRef(ident.Room, h.g.room, h.capability).String()
	}
	return ident.NewRef(ident.Group, h.g.name, h.capability).String()
}

// said is how a why-not names it: its ref, unescaped, so an average reads
// device/pool-water/temperature over 24h, not temperature%20over%2024h.
func (h handle) said() string {
	switch {
	case h.g == nil:
		return string(ident.Device) + "/" + h.device + "/" + h.capability
	case h.g.room != "":
		return string(ident.Room) + "/" + h.g.room + "/" + h.capability
	}
	return string(ident.Group) + "/" + h.g.name + "/" + h.capability
}

// named is how a mistake or oikade status names a capability: "study-radiator setpoint", or
// "group radiators setpoint".
func (h handle) named() string {
	if h.g != nil {
		return h.g.what() + " " + h.capability
	}
	return h.device + " " + h.capability
}

// checkGroups refuses two groups of one name, and a group named as a device a decision or another
// group names, since its cases are keyed by its name as a device's are.
func (h *Home) checkGroups(decided []compiledDecision) error {
	var errs []error
	seen := map[string]*group{}
	devices := map[string]bool{}
	for _, g := range h.groups {
		for _, m := range g.members {
			devices[m.name] = true
		}
	}
	for _, d := range decided {
		if d.g == nil {
			devices[d.device] = true
		}
	}
	for _, g := range h.groups {
		if g.room != "" {
			continue
		}
		if seen[g.name] != nil {
			errs = append(errs, fmt.Errorf("group %q is made twice", g.name))
		}
		seen[g.name] = g
		if devices[g.name] {
			errs = append(errs, fmt.Errorf("group %q has a device's name", g.name))
		}
	}
	return errors.Join(errs...)
}
