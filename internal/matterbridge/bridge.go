package matterbridge

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
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
	// product is what it's said to be, which refresh serves.
	product clusters.Product
	mapped  *Mapped
	info    *clusters.BridgedInfo
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
	// Clock times how long a move takes to settle; nil is the real clock.
	Clock clock.Clock

	mu        sync.Mutex
	endpoints map[uint16]*endpoint
	byDevice  map[ident.ID]*endpoint
}

// Changed implements driver.Sink.
func (b *Bridge) Changed(changes ...driver.Update) {
	for _, u := range changes {
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
	b.mu.Lock()
	e := b.byDevice[dev]
	b.mu.Unlock()
	if v != nil && b.Writes != nil {
		b.Writes.Reported(dev, name)
	}
	if e == nil {
		return
	}
	// One update, so a subscriber never sees the move cancelled without its value back.
	b.Matter.Node.Update(func() {
		// A blind that hasn't reported has no position to show: Current comes only from reports.
		if e.mapped.Cancel(name) && v == otherwise {
			return
		}
		if v != nil {
			e.mapped.Update(name, v)
		}
	})
}

// after runs f under the node's lock once d has passed on the bridge's clock, unless stop is called
// first.
func (b *Bridge) after(d time.Duration, f func()) (stop func()) {
	c := b.Clock
	if c == nil {
		c = clock.Real{}
	}
	return clock.AfterFunc(c, d, func() { b.Matter.Node.Update(f) }).Stop
}

// Apply adds, removes and rebuilds endpoints to match want, by endpoint ID. An endpoint whose device
// takes another name keeps its clusters, and its writes go to the new name.
func (b *Bridge) Apply(log *slog.Logger, want map[uint16]Device) error {
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
		e.absent, e.product = d.Absent, productOf(d.Product)
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
		e := &endpoint{
			signature: d.Signature(), label: Label(d.Name), absent: d.Absent, product: productOf(d.Product),
		}
		e.key.Store(&d.Key)
		d.After = b.after
		// So the log says what a controller asked for, as well as what was written (OIK-337).
		d.Command = func(command string, args ...any) {
			log.Info("Took Matter command", append([]any{"device", e.device(), "command", command}, args...)...)
		}
		d.Send = func() { b.Writes.Send(e.device()) }
		d.Gathering = func(name string) bool { return b.Writes.Gathering(e.device(), name) }
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

// productOf is what a driver says a device is, as Matter has it.
func productOf(p driver.Product) clusters.Product {
	return clusters.Product{
		VendorName: p.Manufacturer, ProductName: p.Model, SerialNumber: p.SerialNumber,
		SoftwareVersion: p.FirmwareVersion, HardwareVersion: p.HardwareVersion,
	}
}

// refresh brings an endpoint's clusters up to date with the device it's for.
func (b *Bridge) refresh(e *endpoint) {
	snap, _ := b.Store.Get(e.device())
	b.Matter.Node.Update(func() {
		// What a device is, it is while it's away.
		e.info.SetProduct(e.product)
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
