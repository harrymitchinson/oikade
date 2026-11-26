package registry

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/ident"
)

// Declaration is a device a home's code declares: its hardware, and the room, type and label it
// gives it.
type Declaration struct {
	// Hardware is its key, as hue/0x1. Driver and ID are its plugin's, if it has one, for hardware
	// that isn't on record yet.
	Hardware string `json:"hardware"`
	Driver   string `json:"driver,omitempty"`
	ID       string `json:"id,omitempty"`
	// Room is empty for a device in none.
	Room       string          `json:"room,omitempty"`
	DeviceType devicetype.Type `json:"deviceType,omitempty"`
	// Capabilities are those the declaration gives the hardware, which it must report.
	Capabilities []string `json:"capabilities,omitempty"`
	Label        string   `json:"label,omitempty"`
}

// Declared is what declaring did, or would do.
type Declared struct {
	// Changes say what changed, a line each, in the order of the declarations, then of the devices
	// no longer declared.
	Changes []string
	// Unseen are the declared devices whose hardware hasn't reported yet, by their keys.
	Unseen []string
	// Touched are the devices whose room, type or label changed, or that came back, by their keys.
	Touched []string
}

// Declare makes the registry what the home's code declares, in one change. A declared device takes
// its room, type and label, and a device that isn't declared goes back to its app's room, its
// driver's type and no label.
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
// reads a home's devices before it's deployed.
func Declaring(s Snapshot, decls []Declaration) (Snapshot, error) {
	s = clone(s)
	_, err := declare(&s, decls)
	return s, err
}

// CheckDeclarations refuses declarations that disagree among themselves.
func CheckDeclarations(decls []Declaration) error {
	var errs []error
	seen := map[string]bool{}
	for _, d := range decls {
		if d.Room != "" {
			if err := ident.CheckName(ident.Room, d.Room); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", d.Hardware, err))
			}
		}
		if d.DeviceType != "" && !slices.Contains(devicetype.All, d.DeviceType) {
			errs = append(errs, fmt.Errorf("%s is declared a %q, which isn't a device type", d.Hardware, d.DeviceType))
		}
		if seen[d.Hardware] {
			errs = append(errs, fmt.Errorf("%s is declared twice", d.Hardware))
		}
		seen[d.Hardware] = true
	}
	return errors.Join(errs...)
}

// declare changes s to what decls say. It fails, leaving s in any state, if they can't be applied.
func declare(s *Snapshot, decls []Declaration) (Declared, error) {
	if err := CheckDeclarations(decls); err != nil {
		return Declared{}, err
	}
	before := clone(*s)
	var out Declared
	taken := map[int]bool{}
	for _, d := range decls {
		i, err := target(s, d)
		if err != nil {
			return Declared{}, err
		}
		taken[i] = true
		e := &s.Devices[i]
		typ := d.DeviceType
		if !devicetype.Choosable(typ) {
			// The rest follow from its capabilities.
			typ = ""
		}
		e.DeviceType, e.Label, e.Room, e.Removed = typ, d.Label, d.Room, false
		if err := fits(*e, typ); err != nil {
			return Declared{}, err
		}
		if err := reports(*e, d); err != nil {
			return Declared{}, err
		}
		if len(e.Capabilities) == 0 {
			out.Unseen = append(out.Unseen, d.Hardware)
		}
	}
	for i := range s.Devices {
		if e := &s.Devices[i]; !taken[i] && !e.Removed {
			e.Room, e.DeviceType, e.Label = roomOf(e.AppRoom), "", ""
		}
	}
	out.Touched = touched(before, *s)
	out.Changes = changes(before, *s, decls)
	return out, nil
}

// touched are the keys of the devices whose room, type or label differ between before and after,
// or that are new or came back.
func touched(before, after Snapshot) []string {
	var out []string
	for _, a := range after.Devices {
		b, existed := at(before, a.Endpoint)
		if a.Removed {
			continue
		}
		if !existed || b.Removed || b.Room != a.Room || b.DeviceType != a.DeviceType || b.Label != a.Label {
			out = append(out, a.Key())
		}
	}
	return out
}

// at is the device of an endpoint.
func at(s Snapshot, endpoint uint16) (Device, bool) {
	i := slices.IndexFunc(s.Devices, func(d Device) bool { return d.Endpoint == endpoint })
	if i < 0 {
		return Device{}, false
	}
	return s.Devices[i], true
}

// target finds a declaration's entry, or makes one for a plugin's hardware that isn't on record yet.
func target(s *Snapshot, d Declaration) (int, error) {
	if j := slices.IndexFunc(s.Devices, func(e Device) bool { return e.Key() == d.Hardware }); j >= 0 {
		return j, nil
	}
	if !strings.HasPrefix(d.Driver, driver.Plugin+"/") {
		return 0, fmt.Errorf("%s isn't on record: it's no plugin's, so the host only has it once its driver reports it",
			d.Hardware)
	}
	endpoint, err := allocate(s)
	if err != nil {
		return 0, err
	}
	s.Devices = append(s.Devices, Device{ID: d.ID, Driver: d.Driver, Endpoint: endpoint})
	return len(s.Devices) - 1, nil
}

// reports refuses hardware that has reported, and lacks a capability its declaration gives it.
func reports(e Device, d Declaration) error {
	if len(e.Capabilities) == 0 {
		return nil
	}
	var lacks []string
	for _, c := range d.Capabilities {
		if !slices.Contains(e.Capabilities, c) {
			lacks = append(lacks, c)
		}
	}
	if len(lacks) == 0 {
		return nil
	}
	return fmt.Errorf("%s has no %s, which its line in your code gives it: it has %s", e.Key(), strings.Join(lacks, " or "),
		strings.Join(e.Capabilities, ", "))
}

// changes says what changed between two registries, for a deploy to list.
func changes(before, after Snapshot, decls []Declaration) []string {
	var out []string
	for _, d := range decls {
		i := slices.IndexFunc(after.Devices, func(a Device) bool { return a.Key() == d.Hardware })
		a := after.Devices[i]
		ref := ident.DeviceRef(a.Key())
		b, existed := at(before, a.Endpoint)
		switch {
		case !existed:
			out = append(out, fmt.Sprintf("Put %s on record", ref))
		case b.Removed:
			out = append(out, fmt.Sprintf("Brought back %s, which was forgotten", ref))
		}
		switch {
		case existed && b.Room == a.Room:
		case a.Room != "":
			out = append(out, fmt.Sprintf("Moved %s to %s", ref, a.Room))
		case existed:
			out = append(out, fmt.Sprintf("Took %s out of its room", ref))
		}
		if existed && b.DeviceType != a.DeviceType {
			out = append(out, typeLine(a))
		}
		if b.Label != a.Label {
			out = append(out, labelLine(a))
		}
	}
	for _, b := range before.Devices {
		a, _ := at(after, b.Endpoint)
		if !b.Removed && !slices.ContainsFunc(decls, func(d Declaration) bool { return d.Hardware == a.Key() }) &&
			(b.Room != a.Room || b.DeviceType != a.DeviceType || b.Label != a.Label) {
			out = append(out, fmt.Sprintf("Gave %s back its app's room and its driver's type", ident.DeviceRef(a.Key())))
		}
	}
	return out
}

func labelLine(d Device) string {
	if d.Label == "" {
		return fmt.Sprintf("Took %s's label off: Matter shows its app's name", ident.DeviceRef(d.Key()))
	}
	return fmt.Sprintf("Labelled %s %q on Matter", ident.DeviceRef(d.Key()), d.Label)
}

func typeLine(d Device) string {
	if d.DeviceType == "" {
		return fmt.Sprintf("Gave %s back the device type its driver says", ident.DeviceRef(d.Key()))
	}
	return fmt.Sprintf("Set %s's device type to %s", ident.DeviceRef(d.Key()), d.DeviceType)
}

// DescribeHardware names hardware as a person would: its plugin and ID.
func DescribeHardware(d, id string) string {
	return fmt.Sprintf("%s's %s", strings.TrimPrefix(d, driver.Plugin+"/"), id)
}
