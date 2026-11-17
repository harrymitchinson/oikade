// Package registry is the host's list of devices, which it owns and keeps in the store. Git holds a
// copy, from oikade types. Each device has a Matter endpoint ID that nothing else ever gets: Next
// only rises, and a removed device stays on record so that its endpoint comes back with it.
package registry

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/fault"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/store"
)

const (
	// Namespace is the registry's place in the store.
	Namespace = "registry"
	// FirstEndpoint is the first endpoint ID handed to a device. Endpoint 0 is the root node and 1
	// the bridge's aggregator.
	FirstEndpoint = 2
	// LastEndpoint is the last ID Matter allows.
	LastEndpoint = 0xFFFE

	stateKey = "state"
)

var (
	// ErrNoDevice is a name the registry doesn't have.
	ErrNoDevice = errors.New("no such device")
)

// Device is one device. Its JSON is in oikade devices --json, so
// add to it, and don't rename.
type Device struct {
	// Name is what code and the command line call it: what the home declares, or else copied from
	// the device's app once, and never following it.
	Name string `json:"name"`
	// Declared says the home's code declares the device, so its name, room and type are the code's.
	Declared bool `json:"declared"`
	// AppName and AppRoom are what its app called it and where, read once as Name and Room are,
	// which a device the home stops declaring goes back to.
	AppName string `json:"appName,omitempty"`
	AppRoom string `json:"appRoom,omitempty"`
	// ID is the driver's stable ID for the hardware. A name reserved for a device that isn't paired
	// yet has none.
	ID string `json:"id,omitempty"`
	// Driver is the driver instance that reports it.
	Driver       string   `json:"driver,omitempty"`
	Room         string   `json:"room,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// DeviceType is what your home declares it is, which wins over its driver.
	DeviceType devicetype.Type `json:"deviceType,omitempty"`
	// DriverDeviceType is what its driver says it is, if the driver knows.
	DriverDeviceType devicetype.Type `json:"driverDeviceType,omitempty"`
	// Endpoint is its Matter endpoint ID, and follows the name onto replacement hardware.
	Endpoint uint16 `json:"endpoint"`
	// Removed keeps the device on record, holding its endpoint, in case it comes back.
	Removed bool `json:"removed,omitempty"`
	// Control is how it's arbitrated, opaque here. It's on record so that claims hold from the moment
	// the host starts, before a driver has reported.
	Control json.RawMessage `json:"control,omitempty"`
}

// DeviceTypes are what it is: yours, else its driver's, else what its capabilities say.
func (d Device) DeviceTypes() []devicetype.Type {
	return devicetype.Of(d.DeviceType, d.DriverDeviceType, d.Capabilities)
}

// Reserved says the name has no hardware yet.
func (d Device) Reserved() bool { return d.ID == "" && !d.Removed }

// Snapshot is the whole registry, ordered by endpoint.
type Snapshot struct {
	// Next is the next endpoint ID to hand out. It never falls.
	Next    uint16   `json:"next"`
	Devices []Device `json:"devices"`
}

// Report is what a driver says about hardware.
type Report struct {
	ID           string
	Driver       string
	Capabilities []string
	// DeviceType is what the driver says it is, empty if it doesn't know.
	DeviceType devicetype.Type
	// Control is how it's arbitrated. Nil leaves what's on record.
	Control json.RawMessage
	// Name and Room are as named in its app, and are only read the first time.
	Name string
	Room string
}

// Registry is the list of devices. It's safe for concurrent use.
type Registry struct {
	values *store.Namespace

	mu    sync.RWMutex
	state Snapshot
}

// Open reads the registry. An unreadable one is an error, not an empty one, which would hand out
// endpoints again.
func Open(values *store.Namespace) (*Registry, error) {
	r := &Registry{values: values, state: Snapshot{Next: FirstEndpoint}}
	raw, ok, err := values.Get(stateKey)
	if err != nil {
		return nil, fmt.Errorf("reading the device registry: %w", err)
	}
	if !ok {
		return r, nil
	}
	if err := json.Unmarshal(raw, &r.state); err != nil {
		return nil, fmt.Errorf("reading the device registry: %w", err)
	}
	if r.state.Next < FirstEndpoint {
		return nil, fmt.Errorf("reading the device registry: its next endpoint is %d", r.state.Next)
	}
	return r, nil
}

// List returns a copy of the registry.
func (r *Registry) List() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return clone(r.state)
}

// Get returns a live device by name.
func (r *Registry) Get(name string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i := live(r.state, name); i >= 0 {
		return cloneDevice(r.state.Devices[i]), true
	}
	return Device{}, false
}

// ByID returns a live device by its hardware.
func (r *Registry) ByID(driver, id string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i := hardware(r.state, driver, id); i >= 0 && !r.state.Devices[i].Removed {
		return cloneDevice(r.state.Devices[i]), true
	}
	return Device{}, false
}

// change applies f to a copy, and takes it up once it's on disk. The endpoint counter and the
// devices are one value, so one write, and a crash can't leave a device with an endpoint the counter
// hasn't passed.
func (r *Registry) change(f func(*Snapshot) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := clone(r.state)
	if err := f(&next); err != nil {
		return err
	}
	slices.SortFunc(next.Devices, func(a, b Device) int { return int(a.Endpoint) - int(b.Endpoint) })
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := r.values.Put(stateKey, raw); err != nil {
		return fmt.Errorf("saving the device registry: %w", err)
	}
	fault.Hit(fault.RegistrySaved)
	r.state = next
	return nil
}

// Report records hardware a driver sees, adding it if it's new, so it's usable at once. Hardware
// that was removed comes back with its name, room and endpoint, under another name only if someone
// has taken that one. What's named in the device's app is only read when it's new. changed says the
// registry took something up.
func (r *Registry) Report(report Report) (device Device, changed bool, err error) {
	if report.ID == "" || report.Driver == "" {
		return Device{}, false, errors.New("a reported device needs an ID and a driver")
	}
	r.mu.RLock()
	i := hardware(r.state, report.Driver, report.ID)
	same := i >= 0 && !r.state.Devices[i].Removed && slices.Equal(r.state.Devices[i].Capabilities, report.Capabilities) &&
		r.state.Devices[i].DriverDeviceType == report.DeviceType && !newAppName(r.state.Devices[i], report) &&
		(report.Control == nil || bytes.Equal(r.state.Devices[i].Control, report.Control))
	var known Device
	if same {
		known = cloneDevice(r.state.Devices[i])
	}
	r.mu.RUnlock()
	if same {
		return known, false, nil
	}
	var out Device
	err = r.change(func(s *Snapshot) error {
		i := hardware(*s, report.Driver, report.ID)
		if i < 0 {
			endpoint, err := allocate(s)
			if err != nil {
				return err
			}
			s.Devices = append(s.Devices, Device{
				Name: uniqueName(*s, Slug(report.Name, report.ID)), ID: report.ID, Driver: report.Driver,
				Room: roomOf(report.Room), Endpoint: endpoint, AppName: clean(report.Name), AppRoom: clean(report.Room),
			})
			i = len(s.Devices) - 1
		}
		d := &s.Devices[i]
		if d.Removed {
			d.Removed, d.Name = false, uniqueName(*s, d.Name)
		}
		d.Capabilities, d.DriverDeviceType = slices.Clone(report.Capabilities), report.DeviceType
		if newAppName(*d, report) {
			d.AppName, d.AppRoom = clean(report.Name), clean(report.Room)
		}
		if report.Control != nil {
			d.Control = bytes.Clone(report.Control)
		}
		out = cloneDevice(*d)
		return nil
	})
	return out, err == nil, err
}

// newAppName says a report is the first to name the device in its app.
func newAppName(d Device, report Report) bool {
	return d.AppName == "" && clean(report.Name) != ""
}

// fits refuses a device type a device can't be. One that hasn't reported can be any.
func fits(d Device, t devicetype.Type) error {
	if t == "" || len(d.Capabilities) == 0 {
		return nil
	}
	switch {
	case !slices.Contains(d.Capabilities, capability.On):
		return fmt.Errorf("%s isn't on or off, so what it is follows from its capabilities", d.Name)
	case slices.Contains(d.Capabilities, capability.Setpoint):
		return fmt.Errorf("%s has a setpoint, so it's a %s", d.Name, devicetype.ClimateOf(d.Capabilities))
	case devicetype.Dimmable(t) && !slices.Contains(d.Capabilities, capability.Brightness):
		return fmt.Errorf("%s has no brightness, so it can't be %s", d.Name, t)
	}
	return nil
}

// Remove takes a device out of use. It stays on record, keeping its endpoint.
func (r *Registry) Remove(name string) error {
	return r.change(func(s *Snapshot) error {
		i := live(*s, name)
		if i < 0 {
			return fmt.Errorf("%w: %s", ErrNoDevice, name)
		}
		s.Devices[i].Removed = true
		return nil
	})
}

func allocate(s *Snapshot) (uint16, error) {
	if s.Next > LastEndpoint {
		return 0, errors.New("no Matter endpoints are left")
	}
	endpoint := s.Next
	s.Next++
	return endpoint, nil
}

func live(s Snapshot, name string) int {
	return slices.IndexFunc(s.Devices, func(d Device) bool { return d.Name == name && !d.Removed })
}

func hardware(s Snapshot, driver, id string) int {
	return slices.IndexFunc(s.Devices, func(d Device) bool { return d.ID == id && d.Driver == driver })
}

func checkName(name string) error { return ident.CheckName(ident.Device, name) }

// Slug makes a device name from what its app calls it, or its ID if that gives nothing.
func Slug(name, id string) string {
	return cmp.Or(ident.Slug(ident.Device, name), ident.Slug(ident.Device, id), string(ident.Device))
}

// roomOf is the room Oikade puts a device in from what its app calls the room.
func roomOf(appRoom string) string { return ident.Slug(ident.Room, appRoom) }

// uniqueName adds a number if a live device has the name.
func uniqueName(s Snapshot, name string) string {
	if live(s, name) < 0 {
		return name
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		candidate := strings.TrimRight(name[:min(len(name), ident.MaxName-len(suffix))], "-") + suffix
		if live(s, candidate) < 0 {
			return candidate
		}
	}
}

// clean makes a name from outside fit on one line, since it ends up in a generated file.
func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, s))
}

func clone(s Snapshot) Snapshot {
	out := Snapshot{Next: s.Next, Devices: make([]Device, 0, len(s.Devices))}
	for _, d := range s.Devices {
		out.Devices = append(out.Devices, cloneDevice(d))
	}
	return out
}

func cloneDevice(d Device) Device {
	d.Capabilities = slices.Clone(d.Capabilities)
	d.Control = bytes.Clone(d.Control)
	return d
}
