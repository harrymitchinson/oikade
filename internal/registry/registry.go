// Package registry is the host's list of devices, which it owns and keeps in the store. Each device
// has a Matter endpoint ID that nothing else ever gets: Next only rises, and a removed device stays
// on record so that its endpoint comes back with it. A device is its Key, its hardware, and has no
// name but its app's: what your code calls it, only your code sees.
package registry

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/fault"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/store"
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
	// ErrNoDevice is a key the registry doesn't have.
	ErrNoDevice = errors.New("no such device")
)

// Device is one device. Its JSON is what the store keeps and oikade list devices --json prints, so
// add to it, and don't rename.
type Device struct {
	// AppName and AppRoom are what its app called it and where, read once, and never following it.
	AppName string `json:"appName,omitempty"`
	AppRoom string `json:"appRoom,omitempty"`
	// ID is the driver's stable ID for the hardware.
	ID string `json:"id,omitempty"`
	// Driver is the driver instance that reports it.
	Driver string `json:"driver,omitempty"`
	// Room is the one your code places it in, which may be none, or, for a device your code doesn't
	// declare, its app's.
	Room         string   `json:"room,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// DeviceType is what your home declares it is, which wins over its driver.
	DeviceType devicetype.Type `json:"deviceType,omitempty"`
	// DriverDeviceType is what its driver says it is, if the driver knows.
	DriverDeviceType devicetype.Type `json:"driverDeviceType,omitempty"`
	// Label is what your home's code says Matter controllers show for it, in place of its app's name.
	Label string `json:"label,omitempty"`
	// Product is what its driver last said it is, each field kept while the driver says nothing of it.
	driver.Product
	// Endpoint is its Matter endpoint ID. Other hardware in its place is another device, with another.
	Endpoint uint16 `json:"endpoint"`
	// Removed keeps the device on record, holding its endpoint, in case it comes back.
	Removed bool `json:"removed,omitempty"`
	// Control is how it's arbitrated, opaque here. It's on record so that claims hold from the moment
	// the host starts, before a driver has reported.
	Control json.RawMessage `json:"control,omitempty"`
}

// Key is how the host and your process know the device: its hardware, as hue/0x00178801091863f2,
// which is its plugin's name and the ID the plugin gives it, percent-encoded. A simulated device's is
// simulator/ and its ID. A key has no space, so a case's key can hold one.
func (d Device) Key() string { return KeyOf(d.Driver, d.ID) }

// KeyOf is the key of a driver instance's device with an ID. A device is its plugin's, or, for a
// driver that isn't one, its instance's, as simulator/porch-light, so every device's ref has the same
// shape.
func KeyOf(instance, id string) string {
	_, source, _ := strings.Cut(instance, "/")
	return ident.HardwareKey(cmp.Or(source, instance), id)
}

// DeviceTypes are what it is: yours, else its driver's, else what its capabilities say.
func (d Device) DeviceTypes() []devicetype.Type {
	return devicetype.Of(d.DeviceType, d.DriverDeviceType, d.Capabilities)
}

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
	// Product is what the driver says it is. An empty field leaves what's on record.
	driver.Product
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
	// A name reserved for hardware before devices were their hardware has none, so it goes.
	r.state.Devices = slices.DeleteFunc(r.state.Devices, func(d Device) bool { return d.ID == "" })
	// A room copied from an app before rooms had names of their own is made into one.
	for i, d := range r.state.Devices {
		if d.Room != "" && !ident.ValidName(d.Room) {
			r.state.Devices[i].Room = roomOf(d.Room)
		}
	}
	return r, nil
}

// Names are the names a registry saved before devices were their hardware, by which a host kept
// what it saved for them, for the host to start that again. A name reserved for hardware that wasn't
// paired was its own key, so it isn't one.
func Names(values *store.Namespace) ([]string, error) {
	raw, ok, err := values.Get(stateKey)
	if err != nil || !ok {
		return nil, err
	}
	var saved struct {
		Devices []struct{ Name, ID, Driver string } `json:"devices"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, fmt.Errorf("reading the device registry: %w", err)
	}
	var out []string
	for _, d := range saved.Devices {
		if d.Name != "" && d.ID != "" && d.Name != KeyOf(d.Driver, d.ID) {
			out = append(out, d.Name)
		}
	}
	return out, nil
}

// List returns a copy of the registry.
func (r *Registry) List() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return clone(r.state)
}

// ByKey returns a live device by its Key.
func (r *Registry) ByKey(key string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i := slices.IndexFunc(r.state.Devices, func(d Device) bool { return !d.Removed && d.Key() == key }); i >= 0 {
		return cloneDevice(r.state.Devices[i]), true
	}
	return Device{}, false
}

// ByID returns a live device by its hardware.
func (r *Registry) ByID(instance, id string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i := hardware(r.state, instance, id); i >= 0 && !r.state.Devices[i].Removed {
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
// that was removed comes back with its room and endpoint. What's named in the device's app is only
// read when it's new. changed says the registry took something up.
func (r *Registry) Report(report Report) (device Device, changed bool, err error) {
	if report.ID == "" || report.Driver == "" {
		return Device{}, false, errors.New("a reported device needs an ID and a driver")
	}
	r.mu.RLock()
	i := hardware(r.state, report.Driver, report.ID)
	same := i >= 0 && !r.state.Devices[i].Removed && slices.Equal(r.state.Devices[i].Capabilities, report.Capabilities) &&
		r.state.Devices[i].DriverDeviceType == report.DeviceType && !newAppName(r.state.Devices[i], report) &&
		!newAppRoom(r.state.Devices[i], report) &&
		!newProduct(r.state.Devices[i], report) &&
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
				ID: report.ID, Driver: report.Driver, Room: roomOf(report.Room), Endpoint: endpoint,
				AppName: clean(report.Name), AppRoom: clean(report.Room),
			})
			i = len(s.Devices) - 1
		}
		d := &s.Devices[i]
		d.Removed = false
		d.Capabilities, d.DriverDeviceType = slices.Clone(report.Capabilities), report.DeviceType
		switch {
		case newAppName(*d, report):
			d.AppName, d.AppRoom = clean(report.Name), clean(report.Room)
		case newAppRoom(*d, report):
			d.AppRoom = clean(report.Room)
		}
		if report.Control != nil {
			d.Control = bytes.Clone(report.Control)
		}
		d.Product = report.Product.Clean().Or(d.Product)
		out = cloneDevice(*d)
		return nil
	})
	return out, err == nil, err
}

// newAppName says a report is the first to name the device in its app.
func newAppName(d Device, report Report) bool {
	return d.AppName == "" && clean(report.Name) != ""
}

// newAppRoom says a report puts a device its app hasn't named yet in another room.
func newAppRoom(d Device, report Report) bool {
	room := clean(report.Room)
	return d.AppName == "" && room != "" && room != d.AppRoom
}

// newProduct says a report says anew what the device is.
func newProduct(d Device, report Report) bool {
	return report.Product.Clean().Or(d.Product) != d.Product
}

// fits refuses a device type a device can't be. One that hasn't reported can be any.
func fits(d Device, t devicetype.Type) error {
	if t == "" || len(d.Capabilities) == 0 {
		return nil
	}
	switch {
	case !slices.Contains(d.Capabilities, capability.On):
		return fmt.Errorf("%s isn't on or off, so what it is follows from its capabilities", d.Key())
	case slices.Contains(d.Capabilities, capability.Setpoint):
		return fmt.Errorf("%s has a setpoint, so it's a %s", d.Key(), devicetype.ClimateOf(d.Capabilities))
	case devicetype.Dimmable(t) && !slices.Contains(d.Capabilities, capability.Brightness):
		return fmt.Errorf("%s has no brightness, so it can't be %s", d.Key(), t)
	}
	return nil
}

// Remove takes a device out of use, by its key. It stays on record, keeping its endpoint.
func (r *Registry) Remove(key string) error {
	return r.change(func(s *Snapshot) error {
		i := slices.IndexFunc(s.Devices, func(d Device) bool { return !d.Removed && d.Key() == key })
		if i < 0 {
			return fmt.Errorf("%w: %s", ErrNoDevice, key)
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

func hardware(s Snapshot, instance, id string) int {
	return slices.IndexFunc(s.Devices, func(d Device) bool { return d.ID == id && d.Driver == instance })
}

// roomOf is the room Oikade puts a device in from what its app calls the room.
func roomOf(appRoom string) string { return ident.Slug(ident.Room, appRoom) }

// clean makes a name from outside fit on one line, since oikade types makes a Go name of it.
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
