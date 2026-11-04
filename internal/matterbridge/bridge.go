package matterbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/fabric"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// endpoint is a Device on the bridge.
type endpoint struct {
	key       types.NamespacedName
	signature string
	mapped    *Mapped
	info      *clusters.BridgedInfo
}

// Bridge keeps a Matter bridge's endpoints in step with its Bridge object and Devices, and their
// clusters in step with the bus. It's a driver.Sink for the hub's Store, and a controller.
type Bridge struct {
	hub.NopSink
	client.Client
	Key types.NamespacedName
	// Matter is the node's bridge, to add and remove endpoints on.
	Matter *clusters.Bridge
	Store  *hub.Store
	// Writes takes writes from controllers.
	Writes *Coalescer

	mu        sync.Mutex
	endpoints map[uint16]*endpoint
	byDevice  map[types.NamespacedName]*endpoint
}

// Changed implements driver.Sink.
func (b *Bridge) Changed(u driver.Update) {
	b.mu.Lock()
	e := b.byDevice[u.Device]
	b.mu.Unlock()
	if e != nil {
		b.Matter.Node.Update(func() { e.mapped.Update(u.Capability, u.Value) })
	}
}

// Availability implements driver.Sink: it's the bridged device's Reachable.
func (b *Bridge) Availability(dev types.NamespacedName, online bool) {
	b.mu.Lock()
	e := b.byDevice[dev]
	b.mu.Unlock()
	if e != nil {
		b.Matter.Node.Update(func() { e.info.SetReachable(online) })
	}
}

// Reconcile adds, removes and rebuilds endpoints to match the selected Devices that have an
// endpoint ID and something Matter can show.
func (b *Bridge) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var br matterv1alpha1.Bridge
	if err := b.Get(ctx, b.Key, &br); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	want, err := b.devices(ctx, &br)
	if err != nil {
		return ctrl.Result{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.endpoints == nil {
		b.endpoints, b.byDevice = map[uint16]*endpoint{}, map[types.NamespacedName]*endpoint{}
	}
	for id, e := range b.endpoints {
		if d, ok := want[id]; ok && d.Signature() == e.signature {
			continue
		}
		b.Matter.RemoveDevice(id)
		delete(b.endpoints, id)
		delete(b.byDevice, e.key)
		log.Info("Removed bridged device", "device", e.key, "endpoint", id)
	}
	ids := make([]uint16, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if b.endpoints[id] != nil {
			continue
		}
		d := want[id]
		key := d.Key
		m := Map(d, func(name string, value any) { b.Writes.Add(key, name, value) })
		info, err := b.Matter.AddDeviceTypes(id, m.Types, Label(d.Name), UniqueID(key), m.Clusters...)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("adding %s on endpoint %d: %w", key, id, err)
		}
		e := &endpoint{key: key, signature: d.Signature(), mapped: m, info: info}
		b.endpoints[id], b.byDevice[key] = e, e
		// Start from the bus's state, then follow it.
		snap, ok := b.Store.Get(key)
		b.Matter.Node.Update(func() {
			if !ok {
				return
			}
			for name, v := range snap.Values {
				m.Update(name, v)
			}
			if snap.Online != nil {
				info.SetReachable(*snap.Online)
			}
		})
		log.Info("Added bridged device", "device", key, "endpoint", id, "types", len(m.Types))
	}
	return ctrl.Result{}, nil
}

// devices returns the selected Devices that have an endpoint ID and map to Matter, by endpoint.
func (b *Bridge) devices(ctx context.Context, br *matterv1alpha1.Bridge) (map[uint16]Device, error) {
	selector, err := metav1.LabelSelectorAsSelector(&br.Spec.DeviceSelector)
	if err != nil {
		return nil, err
	}
	var list homev1alpha1.DeviceList
	if err := b.List(ctx, &list, client.InNamespace(br.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	out := map[uint16]Device{}
	for _, d := range list.Items {
		if d.HiddenFromMatter() {
			continue
		}
		id := br.Status.Endpoint(d.Name)
		if id == 0 {
			continue // the operator hasn't assigned one yet
		}
		dev := Device{Key: client.ObjectKeyFromObject(&d), Endpoint: uint16(id), Name: d.Title()}
		// Capabilities come from status, not the live store, so the endpoints are the same from the
		// moment the pod starts and controllers don't see devices vanish and reappear.
		for _, c := range d.Status.Capabilities {
			dev.Capabilities = append(dev.Capabilities, c.Name)
		}
		if d.Spec.Matter != nil {
			dev.Type = d.Spec.Matter.DeviceType
		}
		if Supported(dev.Capabilities) {
			out[dev.Endpoint] = dev
		}
	}
	return out, nil
}

// SetupWithManager registers the controller. Every request maps to the one Bridge.
func (b *Bridge) SetupWithManager(mgr ctrl.Manager) error {
	toBridge := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: b.Key}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("matter-bridge").
		Watches(&matterv1alpha1.Bridge{}, toBridge).
		Watches(&homev1alpha1.Device{}, toBridge, builder.WithPredicates(deviceShapeChanged)).
		Complete(b)
}

// deviceShapeChanged passes Device events that can change the bridge's endpoints: not the status
// updates that every reading makes, which reach the bridge through the Store.
var deviceShapeChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*homev1alpha1.Device)
		n, ok2 := e.ObjectNew.(*homev1alpha1.Device)
		if !ok1 || !ok2 {
			return true
		}
		return o.Generation != n.Generation || !maps.Equal(o.Labels, n.Labels) ||
			!slices.Equal(capabilityNames(o), capabilityNames(n))
	},
}

func capabilityNames(d *homev1alpha1.Device) []string {
	names := make([]string, 0, len(d.Status.Capabilities))
	for _, c := range d.Status.Capabilities {
		names = append(names, c.Name)
	}
	slices.Sort(names)
	return names
}

// Fabrics converts the fabric table to what status shows.
func Fabrics(t *fabric.Table) []matterv1alpha1.Fabric {
	all := t.Fabrics()
	out := make([]matterv1alpha1.Fabric, 0, len(all))
	for _, f := range all {
		out = append(out, matterv1alpha1.Fabric{
			FabricIndex: int32(f.Index), FabricID: fmt.Sprintf("%016X", f.FabricID), NodeID: fmt.Sprintf("%016X", f.NodeID),
			VendorID: int32(f.VendorID), Label: f.Label,
		})
	}
	slices.SortFunc(out, func(a, b matterv1alpha1.Fabric) int { return int(a.FabricIndex - b.FabricIndex) })
	return out
}

// FabricsPublisher publishes a bridge's fabrics, retained, whenever they change, for the runtime to
// put in the Bridge's status.
type FabricsPublisher struct {
	Bus    mqtt.Bus
	Bridge types.NamespacedName
	Table  *fabric.Table

	mu      sync.Mutex
	last    string
	changed chan struct{}
}

// Changed asks Run to publish soon. It doesn't block, so the node can call it under its lock.
func (p *FabricsPublisher) Changed() {
	p.mu.Lock()
	if p.changed == nil {
		p.changed = make(chan struct{}, 1)
	}
	c := p.changed
	p.mu.Unlock()
	select {
	case c <- struct{}{}:
	default:
	}
}

// Publish publishes the fabrics if they changed since the last publish.
func (p *FabricsPublisher) Publish(ctx context.Context) {
	raw, err := json.Marshal(Fabrics(p.Table))
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if string(raw) == p.last {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Bus.Publish(ctx, FabricsTopic(p.Bridge), raw, true); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to publish fabrics", "bridge", p.Bridge)
		return
	}
	p.last = string(raw)
}

// Run publishes now, after each Changed, and every interval, which catches label changes that
// have no hook.
func (p *FabricsPublisher) Run(ctx context.Context, interval time.Duration) {
	p.mu.Lock()
	if p.changed == nil {
		p.changed = make(chan struct{}, 1)
	}
	changed := p.changed
	p.mu.Unlock()
	p.Publish(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-t.C:
		}
		p.Publish(ctx)
	}
}

// FabricsTopic is where a bridge publishes its fabrics.
func FabricsTopic(bridge types.NamespacedName) string {
	return Root + bridge.Namespace + "/" + bridge.Name + "/" + fabricsTopic
}

// SubscribeFabrics calls f with each bridge's fabrics as they're published, retained ones first.
func SubscribeFabrics(ctx context.Context, bus mqtt.Bus, f func(bridge types.NamespacedName, fabrics []matterv1alpha1.Fabric)) (func(), error) {
	log := logf.FromContext(ctx)
	return bus.Subscribe(ctx, Root+"+/+/"+fabricsTopic, func(m mqtt.Message) {
		parts := strings.Split(strings.TrimPrefix(m.Topic, Root), "/")
		var fabrics []matterv1alpha1.Fabric
		if len(parts) != 3 || json.Unmarshal(m.Payload, &fabrics) != nil {
			log.V(1).Info("Ignored malformed Matter fabrics", "topic", m.Topic)
			return
		}
		f(types.NamespacedName{Namespace: parts[0], Name: parts[1]}, fabrics)
	})
}

// FabricStatus writes the fabrics bridges publish into their Bridges' status. The runtime runs it,
// since the bridge pods can't write to the API.
type FabricStatus struct {
	client.Client

	mu      sync.Mutex
	pending map[types.NamespacedName][]matterv1alpha1.Fabric
	wake    chan struct{}
}

// Record takes a bridge's fabrics. It doesn't block.
func (s *FabricStatus) Record(bridge types.NamespacedName, fabrics []matterv1alpha1.Fabric) {
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[types.NamespacedName][]matterv1alpha1.Fabric{}
	}
	s.pending[bridge] = fabrics
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start writes the fabrics until ctx is done. A write that fails is retried with the next change,
// or a minute later.
func (s *FabricStatus) Start(ctx context.Context) error {
	log := logf.FromContext(ctx)
	retry := time.NewTicker(time.Minute)
	defer retry.Stop()
	for {
		s.mu.Lock()
		pending := s.pending
		s.pending = nil
		s.mu.Unlock()
		for key, fabrics := range pending {
			if err := s.write(ctx, key, fabrics); err != nil {
				log.Error(err, "Failed to write fabrics to Bridge status", "bridge", key)
				s.mu.Lock()
				if _, newer := s.pending[key]; !newer {
					if s.pending == nil {
						s.pending = map[types.NamespacedName][]matterv1alpha1.Fabric{}
					}
					s.pending[key] = fabrics
				}
				s.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-retry.C:
		}
	}
}

func (s *FabricStatus) write(ctx context.Context, key types.NamespacedName, fabrics []matterv1alpha1.Fabric) error {
	var b matterv1alpha1.Bridge
	if err := s.Get(ctx, key, &b); err != nil {
		return client.IgnoreNotFound(err)
	}
	if equality.Semantic.DeepEqual(b.Status.Fabrics, fabrics) || (len(b.Status.Fabrics) == 0 && len(fabrics) == 0) {
		return nil
	}
	orig := b.DeepCopy()
	b.Status.Fabrics = fabrics
	return s.Status().Patch(ctx, &b, client.MergeFrom(orig))
}

// NewFabricStatus returns a FabricStatus ready to Start.
func NewFabricStatus(c client.Client) *FabricStatus {
	return &FabricStatus{Client: c, wake: make(chan struct{}, 1)}
}
