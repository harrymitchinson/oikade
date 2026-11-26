package oikade

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/ident"
)

// Hardware is the hardware a device is. Every device type embeds it, as does each device oikade
// types writes, which makes a device a member a group can take.
type Hardware struct {
	// Key is how the host knows the hardware: its ref without hardware/, as oikade list devices --wide
	// shows it, which is its plugin's name and the ID the plugin gives it, percent-encoded, such as
	// hue/0x00178801091863f2. A simulated device's is its ID, and a reserved name's the name. A rename
	// doesn't change it.
	Key string
}

func (h Hardware) hardware() Hardware { return h }

func (h *Hardware) setKey(key string) { h.Key = key }

// member is a device, which embeds Hardware.
type member interface{ hardware() Hardware }

// DeviceType is what a device is, in Matter's names. Each is a struct below, with the capabilities
// a device of its type always has, which Device takes, and the zero value of one is the type, which
// Kinds and Declaration.Is take: oikade.Kinds(oikade.DimmableLight{}).
type DeviceType interface{ deviceType() devicetype.Type }

// The types of a device that's on or off. Only these can be more than one thing, since a relay can
// switch a lamp or a heater, so a device's type says which.
type (
	// OnOffLight is a light that's on or off.
	OnOffLight struct {
		Hardware
		On Bool
	}
	// DimmableLight is a light that dims.
	DimmableLight struct {
		Hardware
		On         Bool
		Brightness Number
	}
	// OnOffPlugInUnit is an outlet, or anything else on a relay.
	OnOffPlugInUnit struct {
		Hardware
		On Bool
	}
	// DimmablePlugInUnit is an outlet that dims what's plugged in.
	DimmablePlugInUnit struct {
		Hardware
		On         Bool
		Brightness Number
	}
	// MountedOnOffControl is a switch in the wall.
	MountedOnOffControl struct {
		Hardware
		On Bool
	}
	// MountedDimmableLoadControl is a dimmer in the wall.
	MountedDimmableLoadControl struct {
		Hardware
		On         Bool
		Brightness Number
	}
)

// The types that follow from a device's capabilities.
type (
	// OccupancySensor senses motion or presence.
	OccupancySensor struct {
		Hardware
		Occupied Bool
	}
	// ContactSensor is on a door or window.
	ContactSensor struct {
		Hardware
		Open Bool
	}
	// TemperatureSensor reads the temperature.
	TemperatureSensor struct {
		Hardware
		Temperature Number
	}
	// HumiditySensor reads the humidity.
	HumiditySensor struct {
		Hardware
		Humidity Number
	}
	// DoorLock is a lock.
	DoorLock struct {
		Hardware
		Locked Bool
	}
	// WindowCovering is a blind, shade or shutter.
	WindowCovering struct {
		Hardware
		Position Number
	}
	// Thermostat is a heater or thermostat: a device with a Setpoint that isn't a
	// RoomAirConditioner.
	Thermostat struct {
		Hardware
		Setpoint Number
	}
	// RoomAirConditioner is an air conditioner or heat pump. A room's on covers it with Everything,
	// and Kinds(RoomAirConditioner{}) means only them.
	RoomAirConditioner struct {
		Hardware
		On       Bool
		Setpoint Number
		Mode     Choice
	}
)

func (OnOffLight) deviceType() devicetype.Type          { return devicetype.OnOffLight }
func (DimmableLight) deviceType() devicetype.Type       { return devicetype.DimmableLight }
func (OnOffPlugInUnit) deviceType() devicetype.Type     { return devicetype.OnOffPlugInUnit }
func (DimmablePlugInUnit) deviceType() devicetype.Type  { return devicetype.DimmablePlugInUnit }
func (MountedOnOffControl) deviceType() devicetype.Type { return devicetype.MountedOnOffControl }
func (MountedDimmableLoadControl) deviceType() devicetype.Type {
	return devicetype.MountedDimmableLoadControl
}
func (OccupancySensor) deviceType() devicetype.Type    { return devicetype.OccupancySensor }
func (ContactSensor) deviceType() devicetype.Type      { return devicetype.ContactSensor }
func (TemperatureSensor) deviceType() devicetype.Type  { return devicetype.TemperatureSensor }
func (HumiditySensor) deviceType() devicetype.Type     { return devicetype.HumiditySensor }
func (DoorLock) deviceType() devicetype.Type           { return devicetype.DoorLock }
func (WindowCovering) deviceType() devicetype.Type     { return devicetype.WindowCovering }
func (Thermostat) deviceType() devicetype.Type         { return devicetype.Thermostat }
func (RoomAirConditioner) deviceType() devicetype.Type { return devicetype.RoomAirConditioner }

// typeOf is a device type's name, or none for nil.
func typeOf(t DeviceType) devicetype.Type {
	if t == nil {
		return ""
	}
	return t.deviceType()
}

// DeviceOption is what Device takes after the hardware: the device's room, from Room, or its Label.
type DeviceOption interface{ applyTo(d *declaredDevice) }

// Label is the name Matter controllers, such as the Home app, show for the device, in place of the
// one the host has for it.
func Label(name string) DeviceOption { return labelOption(name) }

type labelOption string

func (l labelOption) applyTo(d *declaredDevice) {
	switch {
	case strings.TrimSpace(string(l)) == "":
		d.errs = append(d.errs, errors.New("its Label is empty"))
	case strings.ContainsFunc(string(l), unicode.IsControl):
		d.errs = append(d.errs, fmt.Errorf("its Label %q has a control character", string(l)))
	case d.label != "":
		d.errs = append(d.errs, errors.New("it has two Labels"))
	}
	d.label = string(l)
}

// Device declares a device: the hardware it is, its Key, which a reserved name can't be, and what it
// is, T, a device type or a struct of one and the device's other capabilities.
//
//	var (
//		Cuisine        = oikade.Room("cuisine")
//		CuisinePlafond = oikade.Device[oikade.DimmableLight]("hue/0x001788010f9c0392", Cuisine)
//		CuisineCapteur = oikade.Device[struct {
//			oikade.OccupancySensor
//			Temperature, Battery oikade.Number
//		}]("hue/0x00178801091863f2", Cuisine)
//		BureauPrise = oikade.Device[oikade.OnOffPlugInUnit]("ewelink/1000a1b2c3") // in no room
//	)
//
// Each of T's capabilities is the hardware's, so CuisinePlafond.On is that lamp's, and a deploy
// refuses hardware that doesn't report them all. A room, from Room, places the device there, and one
// without is in no room. Declare devices at package level: every device the program declares is its
// home's, under the name of its variable, which only your code sees. Hardware declared here can't be
// declared with Home.Device too.
func Device[T DeviceType](hardware string, options ...DeviceOption) T {
	var t T
	d := &declaredDevice{key: hardware}
	defer d.declare()
	if err := checkHardware(hardware); err != nil {
		d.errs = append(d.errs, err)
	}
	v := reflect.ValueOf(&t).Elem()
	if v.Kind() != reflect.Struct {
		d.errs = append(d.errs, fmt.Errorf("its type, %s, isn't a device type, or a struct of one and other capabilities",
			v.Type()))
		return t
	}
	if field, ok := embedsIndirect(v.Type()); ok {
		d.errs = append(d.errs, fmt.Errorf("it embeds %s by pointer or interface: embed the device type itself", field))
		return t
	}
	d.typ = t.deviceType()
	any(&t).(interface{ setKey(key string) }).setKey(hardware)
	d.capabilities, d.errs = fill(v, hardware, d.errs)
	for _, o := range options {
		if o == nil {
			d.errs = append(d.errs, errors.New("it has a nil option"))
			continue
		}
		o.applyTo(d)
	}
	return t
}

// embedsIndirect finds a field t embeds by pointer or interface, through which a device type
// would be nil.
func embedsIndirect(t reflect.Type) (string, bool) {
	for field := range t.Fields() {
		switch {
		case !field.Anonymous:
		case field.Type.Kind() == reflect.Pointer || field.Type.Kind() == reflect.Interface:
			return field.Name, true
		case field.Type.Kind() == reflect.Struct:
			if name, ok := embedsIndirect(field.Type); ok {
				return name, true
			}
		}
	}
	return "", false
}

// fill makes each of v's capabilities, and its device type's, the hardware's, and says which they
// are, sorted.
func fill(v reflect.Value, key string, errs []error) ([]string, []error) {
	var caps []string
	for field, f := range v.Fields() {
		switch {
		case field.Type == reflect.TypeFor[Hardware]():
			continue
		case field.Anonymous && field.Type.Kind() == reflect.Struct:
			var more []string
			more, errs = fill(f, key, errs)
			caps = append(caps, more...)
			continue
		}
		c, err := capabilityOfField(field)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch h := f.Addr().Interface().(type) {
		case *Bool:
			*h = Bool{Device: key, Capability: Capability[bool](c)}
		case *Number:
			*h = Number{Device: key, Capability: Capability[float64](c)}
		case *Choice:
			*h = Choice{Device: key, Capability: Capability[ClimateMode](c)}
		}
		caps = append(caps, c)
	}
	slices.Sort(caps)
	return slices.Compact(caps), errs
}

// capabilityOfField is the capability a field of a device's struct is: a Bool, Number or Choice
// named as the capability is in Go.
func capabilityOfField(field reflect.StructField) (string, error) {
	kinds := map[reflect.Type]capability.Kind{
		reflect.TypeFor[Bool](): capability.KindBool, reflect.TypeFor[Number](): capability.KindNumber,
		reflect.TypeFor[Choice](): capability.KindName,
	}
	kind, ok := kinds[field.Type]
	if !ok || !field.IsExported() {
		return "", fmt.Errorf("its field %s isn't a capability: a capability is an oikade.Bool, Number or Choice", field.Name)
	}
	for _, name := range capability.Names() {
		spec, _ := capability.Lookup(name)
		switch {
		case spec.Go != field.Name:
		case spec.Kind != kind:
			return "", fmt.Errorf("its field %s is an oikade.%s, and %s is an oikade.%s", field.Name,
				field.Type.Name(), name, kindType(spec.Kind))
		default:
			return name, nil
		}
	}
	return "", fmt.Errorf("its field %s isn't named as a capability is, such as Temperature", field.Name)
}

func kindType(k capability.Kind) string {
	return map[capability.Kind]string{
		capability.KindBool: "Bool", capability.KindNumber: "Number", capability.KindName: "Choice",
	}[k]
}

// checkHardware refuses hardware that isn't a key, as Hardware.Key says.
func checkHardware(key string) error {
	plugin, id, ok := strings.Cut(key, "/")
	if key == "" {
		return errors.New("it needs its hardware: its ref without hardware/, such as hue/0x00178801091863f2")
	}
	if !ok {
		raw, err := url.PathUnescape(key)
		if err != nil || url.PathEscape(raw) != key {
			return fmt.Errorf("its hardware %q isn't an ID, percent-encoded", key)
		}
		return nil
	}
	if err := ident.CheckName(ident.Plugin, plugin); err != nil {
		return fmt.Errorf("its hardware %q: %w", key, err)
	}
	raw, err := url.PathUnescape(id)
	if err != nil || raw == "" {
		return fmt.Errorf("its hardware %q has no ID after its plugin", key)
	}
	if want := ident.HardwareKey(plugin, raw); want != key {
		return fmt.Errorf("its hardware %q is %s, percent-encoded as its ref is", key, want)
	}
	return nil
}

// RoomGroup is a room, from Room, and the devices in it as a Group.
type RoomGroup struct {
	Group
	g *group
}

func (r RoomGroup) applyTo(d *declaredDevice) {
	switch {
	case r.g == nil:
		d.errs = append(d.errs, errors.New("its room isn't one from oikade.Room"))
		return
	case d.room != nil:
		d.errs = append(d.errs, fmt.Errorf("it's placed in %s and %s", d.room.room, r.g.room))
		return
	}
	d.room = r.g
}

// Room is one of your home's rooms, by the name oikade status gives it and room/NAME takes, and the
// devices in it as a Group:
//
//	var Cuisine = oikade.Room("cuisine")
//
//	home.Decide(CuisinePlafond.On).Case("someone-in", true, Cuisine.Occupied.Any()).Default(false)
//
// Device places a device in it. Its devices are the ones the host has there when it's read or
// written: those placed in it, and any your code doesn't declare whose app puts them there. So
// Cuisine.On.To(false) turns off everything in it that has an on, lights, plugs and heaters alike, and
// Home.Room picks some kinds of them. A room is one room however often it's made.
func Room(name string) RoomGroup {
	declared.mu.Lock()
	defer declared.mu.Unlock()
	g, ok := declared.rooms[name]
	if !ok {
		if err := ident.CheckName(ident.Room, name); err != nil {
			declared.mistakes = append(declared.mistakes, err)
		}
		g = &group{name: name, room: name}
		if declared.rooms == nil {
			declared.rooms = map[string]*group{}
		}
		declared.rooms[name] = g
	}
	return RoomGroup{Group: g.fields(), g: g}
}

// declared are the devices and rooms the program declares, with Device and Room, which are its
// home's.
var declared struct {
	mu       sync.Mutex
	devices  []*declaredDevice
	rooms    map[string]*group
	mistakes []error
}

// declaredDevice is a device from Device.
type declaredDevice struct {
	key          string
	typ          devicetype.Type
	capabilities []string
	room         *group
	label        string
	errs         []error
}

func (d *declaredDevice) declare() {
	declared.mu.Lock()
	defer declared.mu.Unlock()
	declared.devices = append(declared.devices, d)
	if d.room != nil {
		d.room.members = append(d.room.members, groupMember{name: d.key, capabilities: d.capabilities})
	}
}

// declaredDevices are the devices from Device, in the order they were declared, and what's wrong
// with them and with the rooms.
func declaredDevices() ([]*declaredDevice, []error) {
	declared.mu.Lock()
	defer declared.mu.Unlock()
	return slices.Clone(declared.devices), slices.Clone(declared.mistakes)
}

// hardwareInventory is the devices from Device, for the host, by their hardware.
func hardwareInventory() []wire.DeviceDeclaration {
	devices, _ := declaredDevices()
	out := make([]wire.DeviceDeclaration, 0, len(devices))
	for _, d := range devices {
		room := ""
		if d.room != nil {
			room = d.room.room
		}
		out = append(out, wire.DeviceDeclaration{
			Hardware: d.key, Room: &room, DeviceType: d.typ, Capabilities: slices.Clone(d.capabilities), Label: d.label,
		})
	}
	return out
}

// checkHardwareInventory refuses a device from Device that's wrong, declared twice, of a plugin the
// home doesn't import, or that Home.Device declares too.
func (h *Home) checkHardwareInventory() error {
	devices, errs := declaredDevices()
	seen := map[string]bool{}
	byName := map[string]string{}
	for _, d := range h.declared {
		if d.id != "" {
			byName[ident.HardwareKey(d.plugin, d.id)] = d.name
		}
	}
	for _, d := range devices {
		bad := func(err error) { errs = append(errs, fmt.Errorf("device %s: %w", d.key, err)) }
		for _, err := range d.errs {
			bad(err)
		}
		imported := func(plugin string) bool {
			return slices.ContainsFunc(h.plugins, func(p *Plugin) bool { return p.name == plugin })
		}
		if plugin, _, ok := strings.Cut(d.key, "/"); ok && !imported(plugin) {
			bad(fmt.Errorf("its hardware is plugin %s's, which this home doesn't import with Plugin", plugin))
		}
		if name, ok := byName[d.key]; ok {
			bad(fmt.Errorf("Home.Device declares it too, as %s: declare it once", name))
		}
		if seen[d.key] {
			bad(errors.New("it's declared twice"))
		}
		seen[d.key] = true
	}
	return errors.Join(errs...)
}
