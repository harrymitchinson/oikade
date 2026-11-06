// Package hub holds live device state from drivers. The host, its metrics and the Matter bridge
// share it.
package hub

import (
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
)

// Meta is what the hub knows about a declared Device from its spec.
type Meta struct {
	DisplayName string
	Room        string
	// Instance is the driver instance the device belongs to.
	Instance driver.InstanceKey
	// Control is the Device's control entries: the capabilities it arbitrates, and how.
	Control []control.Entry
}

// Controls reports whether the Device arbitrates the capability.
func (m Meta) Controls(name string) bool {
	return slices.ContainsFunc(m.Control, func(c control.Entry) bool { return c.Capability == name })
}

// Snapshot is a copy of one device's state.
type Snapshot struct {
	Meta
	// Discovered is false until the driver has reported on the device.
	Discovered bool
	Info       driver.Info
	Values     map[string]any
	Changes    map[string]uint64
	// Online is nil until the driver reports availability.
	Online   *bool
	LastSeen time.Time
}

type entry struct {
	Snapshot
}

// Store is the live state of all declared devices. It implements driver.Sink and forwards every
// event to Listeners after updating itself. Events for undeclared devices are dropped.
type Store struct {
	Listeners []driver.Sink
	Log       *slog.Logger

	mu         sync.RWMutex
	devices    map[ident.ID]*entry
	undeclared map[driver.InstanceKey]int
	inventory  map[driver.InstanceKey][]driver.AppDevice
}

var _ driver.Sink = (*Store)(nil)

// NewStore returns an empty store.
func NewStore(log *slog.Logger, listeners ...driver.Sink) *Store {
	return &Store{
		Log: logging.OrDiscard(log), Listeners: listeners,
		devices: map[ident.ID]*entry{}, undeclared: map[driver.InstanceKey]int{},
		inventory: map[driver.InstanceKey][]driver.AppDevice{},
	}
}

// SetDevices replaces the devices that belong to a driver instance. State is kept for devices
// that remain on the same instance; devices no longer declared are dropped, and a device that
// moved from another instance starts afresh.
func (s *Store) SetDevices(instance driver.InstanceKey, devices map[ident.ID]Meta) {
	s.mu.Lock()
	var reassigned, controls []ident.ID
	for key, e := range s.devices {
		if _, keep := devices[key]; e.Instance == instance && !keep {
			delete(s.devices, key)
			reassigned = append(reassigned, key)
			if len(e.Control) > 0 {
				controls = append(controls, key)
			}
		}
	}
	for key, m := range devices {
		e, ok := s.devices[key]
		if !ok || e.Instance != m.Instance {
			e = &entry{Snapshot{Values: map[string]any{}, Changes: map[string]uint64{}}}
			s.devices[key] = e
			reassigned = append(reassigned, key)
		}
		if !slices.EqualFunc(e.Control, m.Control, control.Entry.Equal) {
			controls = append(controls, key)
		}
		e.Meta = m
	}
	s.mu.Unlock()
	for _, l := range s.Listeners {
		if r, ok := l.(Reassigner); ok {
			for _, key := range reassigned {
				r.Reassigned(key)
			}
		}
		if c, ok := l.(ControlWatcher); ok {
			for _, key := range controls {
				c.ControlChanged(key)
			}
		}
	}
}

// ControlWatcher is an optional Listener interface. It hears when a device's control entries
// change, including when it joins or leaves with some.
type ControlWatcher interface {
	ControlChanged(dev ident.ID)
}

// Reassigner is an optional Listener interface. It hears when a device joins, leaves or moves
// between driver instances, which changes its status without any driver event: a plugin that
// hasn't reported yet, for example, sends nothing when the hub claims its device.
type Reassigner interface {
	Reassigned(dev ident.ID)
}

// Move is a device's live state going to another key, with the Meta it has there.
type Move struct {
	To   ident.ID
	Meta Meta
}

// Rename moves devices' live state to other keys, as one change: names can swap, a device replaces
// anything at its new key, and nobody sees one with the wrong Meta. The listeners hear about every
// key involved.
func (s *Store) Rename(moves map[ident.ID]Move) {
	s.mu.Lock()
	taken := map[ident.ID]*entry{}
	for from := range moves {
		if e, ok := s.devices[from]; ok {
			taken[from] = e
			delete(s.devices, from)
		}
	}
	touched := make([]ident.ID, 0, 2*len(moves))
	for from, m := range moves {
		touched = append(touched, from, m.To)
		if e, ok := taken[from]; ok {
			e.Meta = m.Meta
			s.devices[m.To] = e
		}
	}
	s.mu.Unlock()
	for _, l := range s.Listeners {
		for _, key := range touched {
			if r, ok := l.(Reassigner); ok {
				r.Reassigned(key)
			}
			if c, ok := l.(ControlWatcher); ok {
				c.ControlChanged(key)
			}
		}
	}
}

// Forget drops a driver instance that no longer exists: its devices, its undeclared count and its
// inventory.
func (s *Store) Forget(instance driver.InstanceKey) {
	s.SetDevices(instance, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.undeclared, instance)
	delete(s.inventory, instance)
}

// InventoryOf returns a copy of the devices a driver instance sees in its app. reported is false
// until the instance has reported since it started; nil devices with reported true means it
// doesn't list them.
func (s *Store) InventoryOf(instance driver.InstanceKey) (devices []driver.AppDevice, reported bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	devices, reported = s.inventory[instance]
	return cloneApp(devices), reported
}

func cloneApp(devices []driver.AppDevice) []driver.AppDevice {
	if devices == nil {
		return nil
	}
	out := make([]driver.AppDevice, len(devices))
	for i, d := range devices {
		d.Capabilities = slices.Clone(d.Capabilities)
		out[i] = d
	}
	return out
}

// Get returns a copy of a device's state.
func (s *Store) Get(key ident.ID) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.devices[key]
	if !ok {
		return Snapshot{}, false
	}
	return e.copy(), true
}

// List returns a copy of every device's state.
func (s *Store) List() map[ident.ID]Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[ident.ID]Snapshot, len(s.devices))
	for k, e := range s.devices {
		out[k] = e.copy()
	}
	return out
}

// Values returns device name -> capability -> value for one namespace.
func (s *Store) Values(namespace string) map[string]map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]any{}
	for k, e := range s.devices {
		if k.Namespace == namespace {
			out[k.Name] = maps.Clone(e.Values)
		}
	}
	return out
}

// Room returns the declared devices whose room is room in one namespace, sorted by name.
func (s *Store) Room(namespace, room string) []ident.ID {
	if room == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ident.ID
	for k, e := range s.devices {
		if k.Namespace == namespace && e.Room == room {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b ident.ID) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Undeclared returns the number of paired-but-undeclared devices per driver instance.
func (s *Store) UndeclaredCounts() map[driver.InstanceKey]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.undeclared)
}

func (e *entry) copy() Snapshot {
	c := e.Snapshot
	c.Values = maps.Clone(e.Values)
	c.Changes = maps.Clone(e.Changes)
	if e.Online != nil {
		online := *e.Online
		c.Online = &online
	}
	return c
}

// update applies f to a known device and reports whether it was known.
func (s *Store) update(key ident.ID, f func(*entry)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.devices[key]
	if ok {
		f(e)
	}
	return ok
}

// Discovered implements driver.Sink.
func (s *Store) Discovered(dev ident.ID, info driver.Info) {
	if !s.update(dev, func(e *entry) { e.Discovered, e.Info = true, info }) {
		return
	}
	for _, l := range s.Listeners {
		l.Discovered(dev, info)
	}
}

// Changed implements driver.Sink.
func (s *Store) Changed(u driver.Update) {
	if !s.update(u.Device, func(e *entry) {
		e.Values[u.Capability] = u.Value
		e.Changes[u.Capability]++
	}) {
		return
	}
	// Link quality changes on almost every message; logging it would drown the rest.
	if u.Capability != capability.LinkQuality {
		s.Log.Info("Changed capability", "namespace", u.Device.Namespace, "device", u.Device.Name,
			"capability", u.Capability, "value", u.Value, "previous", u.Previous)
	}
	for _, l := range s.Listeners {
		l.Changed(u)
	}
}

// Availability implements driver.Sink.
func (s *Store) Availability(dev ident.ID, online bool) {
	if !s.update(dev, func(e *entry) { e.Online = &online }) {
		return
	}
	for _, l := range s.Listeners {
		l.Availability(dev, online)
	}
}

// Seen implements driver.Sink.
func (s *Store) Seen(dev ident.ID, t time.Time) {
	if !s.update(dev, func(e *entry) { e.LastSeen = t }) {
		return
	}
	for _, l := range s.Listeners {
		l.Seen(dev, t)
	}
}

// Undeclared implements driver.Sink.
func (s *Store) Undeclared(instance driver.InstanceKey, count int) {
	s.mu.Lock()
	prev, had := s.undeclared[instance]
	s.undeclared[instance] = count
	s.mu.Unlock()
	if count > 0 && (!had || prev != count) {
		s.Log.Info("Found paired devices that aren't on record",
			"driver", instance.Driver, "instance", instance.ID, "count", count)
	}
	for _, l := range s.Listeners {
		l.Undeclared(instance, count)
	}
}

// Inventory implements driver.Sink. Listeners get the stored list and must not change it.
func (s *Store) Inventory(instance driver.InstanceKey, devices []driver.AppDevice) {
	devices = cloneApp(devices)
	s.mu.Lock()
	s.inventory[instance] = devices
	s.mu.Unlock()
	for _, l := range s.Listeners {
		l.Inventory(instance, devices)
	}
}

// NopSink ignores every event. Embed it to implement only some of driver.Sink.
type NopSink struct{}

func (NopSink) Discovered(ident.ID, driver.Info)                 {}
func (NopSink) Changed(driver.Update)                            {}
func (NopSink) Availability(ident.ID, bool)                      {}
func (NopSink) Seen(ident.ID, time.Time)                         {}
func (NopSink) Undeclared(driver.InstanceKey, int)               {}
func (NopSink) Inventory(driver.InstanceKey, []driver.AppDevice) {}
