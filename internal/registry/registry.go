// Package registry is the host's list of devices, which it owns and keeps in the store. Git holds a
// copy, from oikade types. Each device has a Matter endpoint ID that nothing else ever gets: Next
// only rises, and a removed device stays on record so that its endpoint comes back with it.
package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
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
	maxName  = 63
)

var (
	// ErrNoDevice is a name the registry doesn't have.
	ErrNoDevice = errors.New("no such device")
	// ErrDisagrees is a copy that would change a registry already in use.
	ErrDisagrees = errors.New("the file disagrees with the registry")

	validName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
)

// Device is one device. Its JSON is in oikade devices --json and in the file oikade types writes, so
// add to it, and don't rename.
type Device struct {
	// Name is what code and the command line call it. It's copied from the device's app once, and
	// never follows it.
	Name string `json:"name"`
	// ID is the driver's stable ID for the hardware. A name reserved for a device that isn't paired
	// yet has none.
	ID string `json:"id,omitempty"`
	// Driver is the driver instance that reports it.
	Driver       string   `json:"driver,omitempty"`
	Room         string   `json:"room,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// DeviceType is what you say it is, from oikade devices type, which wins over its driver. It
	// stays with the name, as a room does.
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
		r.state.Devices[i].DriverDeviceType == report.DeviceType &&
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
				Room: clean(report.Room), Endpoint: endpoint,
			})
			i = len(s.Devices) - 1
		}
		d := &s.Devices[i]
		if d.Removed {
			d.Removed, d.Name = false, uniqueName(*s, d.Name)
		}
		d.Capabilities, d.DriverDeviceType = slices.Clone(report.Capabilities), report.DeviceType
		if report.Control != nil {
			d.Control = bytes.Clone(report.Control)
		}
		out = cloneDevice(*d)
		return nil
	})
	return out, err == nil, err
}

// Reserve holds a name for a device that isn't paired yet, so code can name it. Replace gives it its
// hardware.
func (r *Registry) Reserve(name, room string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return r.change(func(s *Snapshot) error {
		if live(*s, name) >= 0 {
			return fmt.Errorf("%s already exists", name)
		}
		endpoint, err := allocate(s)
		if err != nil {
			return err
		}
		s.Devices = append(s.Devices, Device{Name: name, Room: clean(room), Endpoint: endpoint})
		return nil
	})
}

// Rename gives a device a new name. Its endpoint stays.
func (r *Registry) Rename(name, to string) error {
	if err := checkName(to); err != nil {
		return err
	}
	return r.change(func(s *Snapshot) error {
		i := live(*s, name)
		if i < 0 {
			return fmt.Errorf("%w: %s", ErrNoDevice, name)
		}
		if live(*s, to) >= 0 {
			return fmt.Errorf("%s already exists", to)
		}
		s.Devices[i].Name = to
		return nil
	})
}

// Move puts a device in a room. An empty room takes it out.
func (r *Registry) Move(name, room string) error {
	return r.change(func(s *Snapshot) error {
		i := live(*s, name)
		if i < 0 {
			return fmt.Errorf("%w: %s", ErrNoDevice, name)
		}
		s.Devices[i].Room = clean(room)
		return nil
	})
}

// SetDeviceType says what a device is, one of devicetype.OnOff. An empty type goes back to what its
// driver says. A dimmable type needs a brightness, unless the device isn't paired yet.
func (r *Registry) SetDeviceType(name string, t devicetype.Type) error {
	if t != "" && !devicetype.Choosable(t) {
		return fmt.Errorf("%q isn't a device type: use one of %s", t, devicetype.Join(devicetype.OnOff))
	}
	return r.change(func(s *Snapshot) error {
		i := live(*s, name)
		if i < 0 {
			return fmt.Errorf("%w: %s", ErrNoDevice, name)
		}
		d := &s.Devices[i]
		if t != "" && !d.Reserved() {
			if !slices.Contains(d.Capabilities, capability.On) {
				return fmt.Errorf("%s isn't on or off, so what it is follows from its capabilities", name)
			}
			if devicetype.Dimmable(t) && !slices.Contains(d.Capabilities, capability.Brightness) {
				return fmt.Errorf("%s has no brightness, so it can't be %s", name, t)
			}
		}
		d.DeviceType = t
		return nil
	})
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

// Replace moves name onto the hardware that with reports, so code and the Matter endpoint keep
// working, and how it's arbitrated stays with the name. with goes, and its endpoint stays retired. name's old hardware is forgotten, so if it's
// ever paired again it's a new device.
func (r *Registry) Replace(name, with string) error {
	if name == with {
		return errors.New("a device can't replace itself")
	}
	return r.change(func(s *Snapshot) error {
		i, j := live(*s, name), live(*s, with)
		if i < 0 {
			return fmt.Errorf("%w: %s", ErrNoDevice, name)
		}
		if j < 0 {
			return fmt.Errorf("%w: %s", ErrNoDevice, with)
		}
		source := &s.Devices[j]
		if source.ID == "" {
			return fmt.Errorf("%s has no hardware to move", with)
		}
		d := &s.Devices[i]
		d.ID, d.Driver, d.Capabilities = source.ID, source.Driver, slices.Clone(source.Capabilities)
		d.DriverDeviceType = source.DriverDeviceType
		if len(d.Control) == 0 {
			d.Control = bytes.Clone(source.Control)
		}
		source.ID, source.Removed = "", true
		return nil
	})
}

// Restore rebuilds the registry from a copy. The copy decides everything it names, matched by
// hardware, so a device that re-entered after a loss gets its old name and endpoint back. Anything
// else stays, on a new endpoint if the copy gives its one to something else, or a new name if the
// copy gives its one to something else, and moved lists the live devices that changed so. Next never
// falls, so nothing allocated since the copy is handed out again while the registry still
// remembers it.
//
// A registry in use is the authority, and a copy can be old, so unless force is set Restore refuses
// one that disagrees with it, with ErrDisagrees.
func (r *Registry) Restore(from Snapshot, force bool) (moved []string, err error) {
	if err := check(from); err != nil {
		return nil, err
	}
	err = r.change(func(s *Snapshot) error {
		moved = nil
		if why := disagreement(*s, from); why != "" && !force {
			return fmt.Errorf("%w: %s", ErrDisagrees, why)
		}
		out := clone(from)
		out.Next = max(from.Next, s.Next)
		for _, d := range s.Devices {
			if named(from, d) {
				continue
			}
			was := d.Name
			collides := slices.ContainsFunc(from.Devices, func(f Device) bool { return f.Endpoint == d.Endpoint })
			if collides && d.Removed && d.ID == "" {
				continue // A retired endpoint with nothing that could come back to it.
			}
			if collides {
				endpoint, err := allocate(&out)
				if err != nil {
					return err
				}
				d.Endpoint = endpoint
			}
			if !d.Removed {
				d.Name = uniqueName(out, d.Name)
				if collides || d.Name != was {
					moved = append(moved, was)
				}
			}
			out.Devices = append(out.Devices, cloneDevice(d))
		}
		*s = out
		return nil
	})
	return moved, err
}

// disagreement says how a copy differs from a registry in use, on the hardware and names it has. A
// registry with nothing in it has nothing to lose.
func disagreement(s, from Snapshot) string {
	if len(s.Devices) == 0 {
		return ""
	}
	for _, f := range from.Devices {
		if i := hardware(s, f.Driver, f.ID); f.ID != "" && i >= 0 {
			if d := s.Devices[i]; d.Name != f.Name || d.Endpoint != f.Endpoint || d.Removed != f.Removed {
				return fmt.Sprintf("the file has %s as %s, and the registry as %s", f.ID, f.Name, d.Name)
			}
		}
		if i := live(s, f.Name); !f.Removed && i >= 0 {
			if d := s.Devices[i]; d.ID != f.ID || d.Driver != f.Driver || d.Endpoint != f.Endpoint {
				return fmt.Sprintf("%s is different hardware or a different endpoint in the file", f.Name)
			}
		}
	}
	return ""
}

// named reports whether the copy already has this device.
func named(from Snapshot, d Device) bool {
	return slices.ContainsFunc(from.Devices, func(f Device) bool {
		if d.ID != "" {
			return f.ID == d.ID && f.Driver == d.Driver
		}
		return f.ID == "" && f.Endpoint == d.Endpoint && f.Name == d.Name
	})
}

// check refuses a copy that couldn't have come from a registry.
func check(s Snapshot) error {
	if s.Next < FirstEndpoint {
		return fmt.Errorf("its next endpoint is %d", s.Next)
	}
	endpoints, names, ids := map[uint16]bool{}, map[string]bool{}, map[string]bool{}
	for _, d := range s.Devices {
		if d.Endpoint < FirstEndpoint || d.Endpoint >= s.Next {
			return fmt.Errorf("%s has endpoint %d, outside %d to %d", d.Name, d.Endpoint, FirstEndpoint, s.Next-1)
		}
		if endpoints[d.Endpoint] {
			return fmt.Errorf("endpoint %d is used twice", d.Endpoint)
		}
		endpoints[d.Endpoint] = true
		if err := checkName(d.Name); err != nil {
			return err
		}
		for _, t := range []devicetype.Type{d.DeviceType, d.DriverDeviceType} {
			if t != "" && !devicetype.Choosable(t) {
				return fmt.Errorf("%s has device type %q, which isn't one of %s", d.Name, t, devicetype.Join(devicetype.OnOff))
			}
		}
		if !d.Removed {
			if names[d.Name] {
				return fmt.Errorf("%s is used twice", d.Name)
			}
			names[d.Name] = true
		}
		if d.ID != "" {
			hw := d.Driver + "\x00" + d.ID
			if ids[hw] {
				return fmt.Errorf("the hardware %s is used twice", d.ID)
			}
			ids[hw] = true
		}
	}
	return nil
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

func checkName(name string) error {
	if len(name) > maxName || !validName.MatchString(name) {
		return fmt.Errorf("%q isn't a device name: use lowercase letters, digits and single dashes, starting with a letter", name)
	}
	return nil
}

// Slug makes a device name from what its app calls it, or its ID if that gives nothing.
func Slug(name, id string) string {
	slug := dashed(name)
	if slug == "" {
		slug = dashed(id)
	}
	if slug == "" || !unicode.IsLetter(rune(slug[0])) {
		slug = strings.TrimSuffix("device-"+slug, "-")
	}
	return strings.TrimRight(slug[:min(len(slug), maxName)], "-")
}

func dashed(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// uniqueName adds a number if a live device has the name.
func uniqueName(s Snapshot, name string) string {
	if live(s, name) < 0 {
		return name
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		candidate := strings.TrimRight(name[:min(len(name), maxName-len(suffix))], "-") + suffix
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
