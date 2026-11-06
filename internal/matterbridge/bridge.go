package matterbridge

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
)

// endpoint is a Device on the bridge.
type endpoint struct {
	// key is where its writes go. It's read under the node's lock, where b.mu can't be taken, and
	// changes when a device's name does.
	key       atomic.Pointer[ident.ID]
	signature string
	label     string
	absent    bool
	mapped    *Mapped
	info      *clusters.BridgedInfo
}

func (e *endpoint) device() ident.ID { return *e.key.Load() }

// Bridge keeps a Matter bridge's clusters in step with its devices. It's a driver.Sink for the hub's
// Store, and Apply sets which devices it has.
type Bridge struct {
	hub.NopSink
	// Matter is the node's bridge, to add and remove endpoints on.
	Matter *clusters.Bridge
	Store  *hub.Store
	// Writes takes writes from controllers.
	Writes *Coalescer

	mu        sync.Mutex
	endpoints map[uint16]*endpoint
	byDevice  map[ident.ID]*endpoint
}

// Changed implements driver.Sink.
func (b *Bridge) Changed(u driver.Update) {
	if b.Writes != nil {
		b.Writes.Reported(u.Device, u.Capability)
	}
	b.mu.Lock()
	e := b.byDevice[u.Device]
	b.mu.Unlock()
	if e != nil {
		b.Matter.Node.Update(func() { e.mapped.Update(u.Capability, u.Value) })
	}
}

// Availability implements driver.Sink: it's the bridged device's Reachable.
func (b *Bridge) Availability(dev ident.ID, online bool) {
	b.mu.Lock()
	e := b.byDevice[dev]
	b.mu.Unlock()
	if e != nil {
		b.Matter.Node.Update(func() { e.info.SetReachable(online) })
	}
}

// Restore takes a capability's clusters back after a write that didn't happen: to the device's value,
// or to otherwise if the device hasn't reported one. Nil leaves them.
func (b *Bridge) Restore(dev ident.ID, name string, otherwise any) {
	v := otherwise
	if snap, ok := b.Store.Get(dev); ok {
		if known, has := snap.Values[name]; has {
			v = known
		}
	}
	if v != nil {
		b.Changed(driver.Update{Device: dev, Capability: name, Value: v})
	}
}

// Apply adds, removes and rebuilds endpoints to match want, by endpoint ID. An endpoint whose device
// takes another name keeps its clusters, and its writes go to the new name.
func (b *Bridge) Apply(log logr.Logger, want map[uint16]Device) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.endpoints == nil {
		b.endpoints, b.byDevice = map[uint16]*endpoint{}, map[ident.ID]*endpoint{}
	}
	for id, e := range b.endpoints {
		if d, ok := want[id]; ok && d.Signature() == e.signature {
			if label := Label(d.Name); label != e.label {
				e.label = label
				b.Matter.Node.Update(func() { e.info.SetLabel(label) })
				log.Info("Renamed bridged device", "device", e.device(), "endpoint", id)
			}
			continue
		}
		b.Matter.RemoveDevice(id)
		delete(b.endpoints, id)
		log.Info("Removed bridged device", "device", e.device(), "endpoint", id)
	}
	// Names can swap, so what each endpoint is called now is worked out in one go. Different hardware
	// can have taken a name with no event at all, so each is brought up to date.
	byDevice := map[ident.ID]*endpoint{}
	for id, e := range b.endpoints {
		d := want[id]
		e.key.Store(&d.Key)
		e.absent = d.Absent
		byDevice[d.Key] = e
	}
	b.byDevice = byDevice
	for _, e := range b.endpoints {
		b.refresh(e)
	}
	ids := make([]uint16, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var failed error
	for _, id := range ids {
		if b.endpoints[id] != nil {
			continue
		}
		d := want[id]
		e := &endpoint{signature: d.Signature(), label: Label(d.Name), absent: d.Absent}
		e.key.Store(&d.Key)
		e.mapped = Map(d, func(name string, value any) { b.Writes.Add(e.device(), name, value) })
		info, err := b.Matter.AddDeviceTypes(id, e.mapped.Types, e.label, d.uniqueID(), e.mapped.Clusters...)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("adding %s on endpoint %d: %w", d.Key, id, err))
			continue
		}
		e.info = info
		b.endpoints[id], b.byDevice[d.Key] = e, e
		// Start from the bus's state, then follow it.
		b.refresh(e)
		log.Info("Added bridged device", "device", d.Key, "endpoint", id, "types", len(e.mapped.Types))
	}
	return failed
}

// refresh brings an endpoint's clusters up to date with the device it's for.
func (b *Bridge) refresh(e *endpoint) {
	snap, _ := b.Store.Get(e.device())
	b.Matter.Node.Update(func() {
		if e.absent {
			e.info.SetReachable(false)
			return
		}
		for name, v := range snap.Values {
			e.mapped.Update(name, v)
		}
		e.info.SetReachable(snap.Online == nil || *snap.Online)
	})
}
