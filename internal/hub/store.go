// Package hub holds live device state from drivers and routes commands back to them. It is shared
// by the runtime (metrics, status, rules) and the HomeKit bridge.
package hub

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
)

// Meta is what the hub knows about a declared Device from its spec.
type Meta struct {
	DisplayName string
	Room        string
	// Instance is the driver instance the device belongs to.
	Instance driver.InstanceKey
	// HomeKitService is the optional HomeKit service override.
	HomeKitService string
	// Control is the Device's control entries: the capabilities it arbitrates, and how.
	Control []homev1alpha1.CapabilityControl
}

// Controls reports whether the Device arbitrates the capability.
func (m Meta) Controls(name string) bool {
	return slices.ContainsFunc(m.Control, func(c homev1alpha1.CapabilityControl) bool { return c.Capability == name })
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
	Log       logr.Logger

	mu         sync.RWMutex
	devices    map[types.NamespacedName]*entry
	undeclared map[driver.InstanceKey]int
}

var _ driver.Sink = (*Store)(nil)

// NewStore returns an empty store.
func NewStore(log logr.Logger, listeners ...driver.Sink) *Store {
	return &Store{
		Log: log, Listeners: listeners,
		devices: map[types.NamespacedName]*entry{}, undeclared: map[driver.InstanceKey]int{},
	}
}

// SetDevices replaces the devices that belong to a driver instance. State is kept for devices
// that remain on the same instance; devices no longer declared are dropped, and a device that
// moved from another instance starts afresh.
func (s *Store) SetDevices(instance driver.InstanceKey, devices map[types.NamespacedName]Meta) {
	s.mu.Lock()
	var reassigned, controls []types.NamespacedName
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
		if !equality.Semantic.DeepEqual(e.Control, m.Control) {
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
	ControlChanged(dev types.NamespacedName)
}

// Reassigner is an optional Listener interface. It hears when a device joins, leaves or moves
// between driver instances, which changes its status without any driver event: a plugin that
// hasn't reported yet, for example, sends nothing when the hub claims its device.
type Reassigner interface {
	Reassigned(dev types.NamespacedName)
}

// Forget drops a driver instance that no longer exists: its devices and its undeclared count.
func (s *Store) Forget(instance driver.InstanceKey) {
	s.SetDevices(instance, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.undeclared, instance)
}

// Get returns a copy of a device's state.
func (s *Store) Get(key types.NamespacedName) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.devices[key]
	if !ok {
		return Snapshot{}, false
	}
	return e.copy(), true
}

// List returns a copy of every device's state.
func (s *Store) List() map[types.NamespacedName]Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[types.NamespacedName]Snapshot, len(s.devices))
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
func (s *Store) Room(namespace, room string) []types.NamespacedName {
	if room == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []types.NamespacedName
	for k, e := range s.devices {
		if k.Namespace == namespace && e.Room == room {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b types.NamespacedName) int { return strings.Compare(a.Name, b.Name) })
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
func (s *Store) update(key types.NamespacedName, f func(*entry)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.devices[key]
	if ok {
		f(e)
	}
	return ok
}

// Discovered implements driver.Sink.
func (s *Store) Discovered(dev types.NamespacedName, info driver.Info) {
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
func (s *Store) Availability(dev types.NamespacedName, online bool) {
	if !s.update(dev, func(e *entry) { e.Online = &online }) {
		return
	}
	for _, l := range s.Listeners {
		l.Availability(dev, online)
	}
}

// Seen implements driver.Sink.
func (s *Store) Seen(dev types.NamespacedName, t time.Time) {
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
		s.Log.Info("Found paired devices without a Device object",
			"driver", instance.Driver, "instance", instance.NamespacedName, "count", count)
	}
	for _, l := range s.Listeners {
		l.Undeclared(instance, count)
	}
}

// NopSink ignores every event. Embed it to implement only some of driver.Sink.
type NopSink struct{}

func (NopSink) Discovered(types.NamespacedName, driver.Info) {}
func (NopSink) Changed(driver.Update)                        {}
func (NopSink) Availability(types.NamespacedName, bool)      {}
func (NopSink) Seen(types.NamespacedName, time.Time)         {}
func (NopSink) Undeclared(driver.InstanceKey, int)           {}
