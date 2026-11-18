package registry

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// Declaration is a device a home's code declares: its name, and the hardware, room and type it gives
// it. One with no ID reserves the name for hardware that isn't paired yet.
type Declaration struct {
	Name string `json:"name"`
	// Driver and ID are the hardware's, as a Device has them.
	Driver     string          `json:"driver,omitempty"`
	ID         string          `json:"id,omitempty"`
	Room       string          `json:"room,omitempty"`
	DeviceType devicetype.Type `json:"deviceType,omitempty"`
}

// Declared is what declaring did, or would do.
type Declared struct {
	// Renamed are the live devices whose names change, from their old names to their new ones. Their
	// claims go with them.
	Renamed map[string]string
	// Absorbed are hardware whose own entry a declared name takes over, from the hardware's name to
	// the declared one: its live values come across, and the name keeps its claims.
	Absorbed map[string]string
	// Changes say what changed, a line each, in the order of the declarations, then of the devices
	// no longer declared.
	Changes []string
	// Unseen are declared names whose hardware hasn't reported yet.
	Unseen []string
	// Removed are reserved names no longer declared, which go.
	Removed []string
	// Fresh are names whose hardware changed with nothing on record to bring its values, so their
	// live state starts again.
	Fresh []string
	// Touched are the live devices whose name, room, type or hardware changed, by their names now.
	Touched []string
}

// Declare makes the registry what the home's code declares, in one change. A declared device takes
// its name, room and type, matched by its hardware, and keeps its endpoint. A name that moves onto
// other hardware keeps its endpoint, and a name with no hardware is reserved. A
// device that was declared and no longer is goes back to what its app calls it, and a reserved name
// that no longer is declared is removed.
func (r *Registry) Declare(decls []Declaration) (Declared, error) {
	var out Declared
	err := r.change(func(s *Snapshot) error {
		var err error
		out, err = declare(s, decls)
		return err
	})
	return out, err
}

// Plan says what Declare would do, and changes nothing.
func (r *Registry) Plan(decls []Declaration) (Declared, error) {
	s := r.List()
	return declare(&s, decls)
}

// Declaring is s as decls would make it once a version with them attaches, for oikade types, which
// writes a home's handles before it's deployed.
func Declaring(s Snapshot, decls []Declaration) (Snapshot, error) {
	s = clone(s)
	_, err := declare(&s, decls)
	return s, err
}

// CheckDeclarations refuses declarations that disagree among themselves.
func CheckDeclarations(decls []Declaration) error {
	var errs []error
	names, hardware := map[string]bool{}, map[string]string{}
	for _, d := range decls {
		if err := checkName(d.Name); err != nil {
			errs = append(errs, err)
		}
		if d.Room != "" {
			if err := ident.CheckName(ident.Room, d.Room); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", d.Name, err))
			}
		}
		if names[d.Name] {
			errs = append(errs, fmt.Errorf("%s is declared twice", d.Name))
		}
		names[d.Name] = true
		if d.DeviceType != "" && !devicetype.Choosable(d.DeviceType) {
			errs = append(errs, fmt.Errorf("%s is declared a %q, which isn't one of %s", d.Name, d.DeviceType,
				devicetype.Join(devicetype.OnOff)))
		}
		if (d.ID == "") != (d.Driver == "") {
			errs = append(errs, fmt.Errorf("%s is declared with half its hardware: it needs a plugin and an ID", d.Name))
		}
		if d.ID == "" {
			continue
		}
		hw := d.Driver + "\x00" + d.ID
		if other, ok := hardware[hw]; ok {
			errs = append(errs, fmt.Errorf("%s and %s are both declared as %s", other, d.Name,
				describeDriver(Device{Driver: d.Driver, ID: d.ID})))
		}
		hardware[hw] = d.Name
	}
	return errors.Join(errs...)
}

// declare changes s to what decls say. It fails, leaving s in any state, if they can't be applied.
func declare(s *Snapshot, decls []Declaration) (Declared, error) {
	if err := CheckDeclarations(decls); err != nil {
		return Declared{}, err
	}
	before := clone(*s)
	out := Declared{Renamed: map[string]string{}, Absorbed: map[string]string{}}
	// Each declaration's entry, found before any names change.
	targets := make([]int, len(decls))
	taken := map[int]string{}
	for k, d := range decls {
		i, err := target(s, d, decls, &out)
		if err != nil {
			return Declared{}, err
		}
		if other, ok := taken[i]; ok {
			return Declared{}, fmt.Errorf("%s and %s are declared as the same device", other, d.Name)
		}
		taken[i], targets[k] = d.Name, i
	}
	for k, d := range decls {
		e := &s.Devices[targets[k]]
		e.Name, e.Room, e.DeviceType, e.Declared, e.Removed = d.Name, d.Room, d.DeviceType, true, false
		if err := fits(*e, d.DeviceType); err != nil {
			return Declared{}, err
		}
		if e.ID != "" && len(e.Capabilities) == 0 {
			out.Unseen = append(out.Unseen, d.Name)
		}
	}
	// What's no longer declared goes back to its app's name, once every declared name is placed.
	var undeclared []int
	for i := range s.Devices {
		e := &s.Devices[i]
		if _, ok := taken[i]; ok || !e.Declared || e.Removed {
			continue
		}
		e.Declared, e.Room, e.DeviceType = false, roomOf(e.AppRoom), ""
		if e.ID == "" {
			e.Removed = true
			out.Removed = append(out.Removed, e.Name)
			continue
		}
		e.Name = ""
		undeclared = append(undeclared, i)
	}
	for _, i := range undeclared {
		e := &s.Devices[i]
		e.Name = uniqueName(*s, Slug(e.AppName, e.ID))
	}
	if err := clash(*s); err != nil {
		return Declared{}, err
	}
	compare(before, *s, &out)
	out.Changes = append(out.Changes, changes(before, *s, decls)...)
	return out, nil
}

// compare records in out what changed between before and after, entry by entry.
func compare(before, after Snapshot, out *Declared) {
	for _, b := range before.Devices {
		a := after.Devices[slices.IndexFunc(after.Devices, func(d Device) bool { return d.Endpoint == b.Endpoint })]
		if a.Removed {
			continue
		}
		if !b.Removed && b.ID != "" && a.Name != b.Name {
			out.Renamed[b.Name] = a.Name
		}
		sameHardware := b.ID == a.ID && b.Driver == a.Driver
		if !b.Removed && b.ID != "" && !sameHardware && !slices.Contains(slices.Collect(maps.Values(out.Absorbed)), a.Name) {
			out.Fresh = append(out.Fresh, a.Name)
		}
		if b.Removed || b.Name != a.Name || b.Room != a.Room || b.DeviceType != a.DeviceType || !sameHardware {
			out.Touched = append(out.Touched, a.Name)
		}
	}
	for _, a := range after.Devices[len(before.Devices):] {
		out.Touched = append(out.Touched, a.Name)
	}
}

// target finds a declaration's entry, or makes one. Hardware decides before names do: an entry
// whose hardware is declared is that declaration's, whatever it's called now, so the order of decls
// doesn't matter. Hardware the name moves onto leaves its own entry, which it retires, and records in
// out as renamed.
func target(s *Snapshot, d Declaration, decls []Declaration, out *Declared) (int, error) {
	j := -1
	if d.ID != "" {
		j = hardware(*s, d.Driver, d.ID)
	}
	i := live(*s, d.Name)
	if i >= 0 && i != j && hardwareDeclared(s.Devices[i], decls) {
		i = -1 // Its hardware is another declaration's, so this name is free to take.
	}
	switch {
	case j >= 0 && (i < 0 || i == j):
		return j, nil
	case i >= 0 && !s.Devices[i].Declared && s.Devices[i].ID != "":
		return 0, fmt.Errorf("%s is already the name of %s, which isn't declared: declare that too, "+
			"under another name, or call this something else", d.Name, describeDriver(s.Devices[i]))
	case i >= 0 && j >= 0:
		// The name moves onto hardware that's on record: the name's entry takes it, keeping its
		// endpoint, and the hardware's own is retired.
		e, from := &s.Devices[i], &s.Devices[j]
		e.ID, e.Driver, e.Capabilities, e.DriverDeviceType = from.ID, from.Driver, slices.Clone(from.Capabilities), from.DriverDeviceType
		e.AppName, e.AppRoom = from.AppName, from.AppRoom
		e.Manufacturer, e.Model = from.Manufacturer, from.Model
		if len(e.Control) == 0 {
			e.Control = bytes.Clone(from.Control)
		}
		if !from.Removed {
			out.Absorbed[from.Name] = d.Name
		}
		from.ID, from.Removed, from.Declared = "", true, false
		return i, nil
	case i >= 0:
		// Onto hardware not on record, or none: the old hardware is forgotten.
		e := &s.Devices[i]
		if e.ID != d.ID || e.Driver != d.Driver {
			e.ID, e.Driver, e.Capabilities, e.DriverDeviceType, e.AppName, e.AppRoom = d.ID, d.Driver, nil, "", "", ""
			e.Manufacturer, e.Model = "", ""
		}
		return i, nil
	default:
		endpoint, err := allocate(s)
		if err != nil {
			return 0, err
		}
		s.Devices = append(s.Devices, Device{Name: d.Name, ID: d.ID, Driver: d.Driver, Endpoint: endpoint})
		return len(s.Devices) - 1, nil
	}
}

// hardwareDeclared says a declaration names e's hardware.
func hardwareDeclared(e Device, decls []Declaration) bool {
	return e.ID != "" && slices.ContainsFunc(decls, func(d Declaration) bool { return d.ID == e.ID && d.Driver == e.Driver })
}

// clash refuses two live devices of one name, which an undeclared device's app name can make.
func clash(s Snapshot) error {
	seen := map[string]bool{}
	for _, d := range s.Devices {
		if d.Removed {
			continue
		}
		if seen[d.Name] {
			return fmt.Errorf("%s would be two devices' name", d.Name)
		}
		seen[d.Name] = true
	}
	return nil
}

// changes says what changed between two registries, for a deploy to list.
func changes(before, after Snapshot, decls []Declaration) []string {
	var out []string
	at := func(s Snapshot, endpoint uint16) (Device, bool) {
		i := slices.IndexFunc(s.Devices, func(d Device) bool { return d.Endpoint == endpoint })
		if i < 0 {
			return Device{}, false
		}
		return s.Devices[i], true
	}
	describe := func(a Device) []string {
		b, existed := at(before, a.Endpoint)
		var lines []string
		switch {
		case !existed || b.Removed && a.ID == "":
			lines = append(lines, fmt.Sprintf("Reserved %s", a.Name))
			if a.ID != "" {
				lines[0] = fmt.Sprintf("Declared %s, for %s", a.Name, describeDriver(a))
			}
		case b.Removed:
			lines = append(lines, fmt.Sprintf("Brought back %s, which was removed", a.Name))
		case b.Name != a.Name:
			lines = append(lines, fmt.Sprintf("Renamed %s to %s", b.Name, a.Name))
		}
		if existed && (b.ID != a.ID || b.Driver != a.Driver) && a.ID != "" {
			lines = append(lines, fmt.Sprintf("Moved %s onto %s", a.Name, describeDriver(a)))
		}
		if existed && b.Room != a.Room {
			lines = append(lines, roomLine(a))
		}
		if existed && b.DeviceType != a.DeviceType {
			lines = append(lines, typeLine(a))
		}
		return lines
	}
	for _, d := range decls {
		if i := live(after, d.Name); i >= 0 {
			out = append(out, describe(after.Devices[i])...)
		}
	}
	for _, b := range before.Devices {
		a, _ := at(after, b.Endpoint)
		switch {
		case b.Removed || !b.Declared || a.Declared:
		case a.Removed:
			out = append(out, fmt.Sprintf("Removed %s, which is no longer declared", b.Name))
		default:
			out = append(out, fmt.Sprintf("Gave %s back its app's name, %s: it's no longer declared", b.Name, a.Name))
		}
	}
	return out
}

func roomLine(d Device) string {
	if d.Room == "" {
		return fmt.Sprintf("Took %s out of its room", d.Name)
	}
	return fmt.Sprintf("Moved %s to %s", d.Name, d.Room)
}

func typeLine(d Device) string {
	if d.DeviceType == "" {
		return fmt.Sprintf("Gave %s back the device type its driver says", d.Name)
	}
	return fmt.Sprintf("Set %s's device type to %s", d.Name, d.DeviceType)
}

// describeDriver names a device's hardware as a person would: its plugin and ID.
func describeDriver(d Device) string {
	return DescribeHardware(d.Driver, d.ID)
}

// HardwareRef is the ref of a device's hardware, hardware/<plugin>/<id>, or empty for one that has
// no plugin's hardware, as with a reserved or simulated device.
func HardwareRef(d Device) ident.Ref {
	plugin, ok := pluginOf(d)
	if !ok {
		return ""
	}
	return ident.HardwareRef(plugin, d.ID)
}

// pluginOf is the name a home imports a device's plugin by, or false for a device no plugin's
// hardware is, as with a reserved or simulated one.
func pluginOf(d Device) (string, bool) {
	plugin, ok := strings.CutPrefix(d.Driver, driver.Plugin+"/")
	return plugin, ok && d.ID != ""
}

// Undeclared says the line that would declare a device your home's code doesn't, as it is now, to
// paste into it, or false for one it declares, or can't, as with a removed or simulated device.
func Undeclared(d Device) (string, bool) {
	plugin, ok := pluginOf(d)
	if d.Declared || d.Removed || !ok {
		return "", false
	}
	line := fmt.Sprintf("home.Device(%q).Hardware(%q, %q)", d.Name, plugin, d.ID)
	if d.Room != "" {
		line += fmt.Sprintf(".Room(%q)", d.Room)
	}
	if d.DeviceType != "" {
		line += fmt.Sprintf(".Is(oikade.%s)", d.DeviceType)
	}
	return line, true
}

// DescribeHardware names hardware as a person would: its plugin and ID.
func DescribeHardware(d, id string) string {
	return fmt.Sprintf("%s's %s", strings.TrimPrefix(d, driver.Plugin+"/"), id)
}
