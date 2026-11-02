package hub

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/driver/plugin"
	"github.com/harrymitchinson/oikade/internal/driver/zigbee2mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// Hub keeps a driver instance running for every driver object, a Zigbee2MQTT or a Plugin, feeds the
// Store, and routes Set calls to the right instance.
type Hub struct {
	client.Client
	Bus   mqtt.Bus
	Store *Store
	Now   func() time.Time

	// ctx outlives individual reconciles; driver subscriptions are bound to it.
	ctx       context.Context
	mu        sync.Mutex
	instances map[driver.InstanceKey]instance
}

// instance is a running driver instance.
type instance interface {
	Start(ctx context.Context) error
	Set(ctx context.Context, dev types.NamespacedName, name string, value any) error
	// SetDevices replaces the declared devices, keyed by the driver's device id.
	SetDevices(declared map[string]types.NamespacedName)
	Stop()
}

var (
	_ driver.Setter = (*Hub)(nil)
	_ instance      = (*zigbee2mqtt.Instance)(nil)
	_ instance      = (*plugin.Instance)(nil)
)

// Set validates a command against the capability registry and sends it to the device's driver.
func (h *Hub) Set(ctx context.Context, dev types.NamespacedName, name string, value any) error {
	spec, ok := capability.Lookup(name)
	if !ok {
		return fmt.Errorf("unknown capability %q", name)
	}
	if !spec.Writable {
		return fmt.Errorf("capability %s is read-only", name)
	}
	v, err := capability.Validate(name, value)
	if err != nil {
		return err
	}
	snap, ok := h.Store.Get(dev)
	if !ok {
		return fmt.Errorf("device %s is not declared", dev)
	}
	h.mu.Lock()
	inst := h.instances[snap.Instance]
	h.mu.Unlock()
	if inst == nil {
		return fmt.Errorf("%s instance %s for device %s is not running",
			snap.Instance.Driver, snap.Instance.NamespacedName, dev)
	}
	return inst.Set(ctx, dev, name, v)
}

// reconcileZigbee2MQTT keeps one Zigbee2MQTT instance's driver and devices in sync.
func (h *Hub) reconcileZigbee2MQTT(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: req.NamespacedName}
	var z zigbeev1alpha1.Zigbee2MQTT
	if found, err := h.get(ctx, key, &z); !found {
		return ctrl.Result{}, err
	}
	metas, declared, err := h.declared(ctx, key, func(d *homev1alpha1.Device) string {
		if src := d.Spec.Zigbee2MQTT; src != nil && src.InstanceRef.Name == z.Name {
			return src.IEEEAddress
		}
		return ""
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	// The store must know the devices before the driver reports on them.
	h.Store.SetDevices(key, metas)
	h.ensure(key, declared, func(i instance) bool {
		// The key carries the driver, so the instance is a Zigbee2MQTT one.
		return i.(*zigbee2mqtt.Instance).BaseTopic == z.Topic()
	}, func() instance {
		return &zigbee2mqtt.Instance{
			Key: req.NamespacedName, BaseTopic: z.Topic(), Bus: h.Bus, Sink: h.Store, Now: h.Now,
			Log: logf.FromContext(h.ctx).WithValues("instance", req.NamespacedName),
		}
	})
	return ctrl.Result{}, nil
}

// reconcilePlugin keeps one Plugin's decoder and devices in sync.
func (h *Hub) reconcilePlugin(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := driver.InstanceKey{Driver: driver.Plugin, NamespacedName: req.NamespacedName}
	var p pluginsv1alpha1.Plugin
	if found, err := h.get(ctx, key, &p); !found {
		return ctrl.Result{}, err
	}
	metas, declared, err := h.declared(ctx, key, func(d *homev1alpha1.Device) string {
		if src := d.Spec.Plugin; src != nil && src.PluginRef.Name == p.Name {
			return src.ID
		}
		return ""
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	h.Store.SetDevices(key, metas)
	// The topic root follows from the key, so a running instance is always current.
	h.ensure(key, declared, nil, func() instance {
		// The decoder logs the plugin key itself.
		return &plugin.Instance{
			Key: req.NamespacedName, Root: contractv1.Root(p.Topic()), Bus: h.Bus, Sink: h.Store, Now: h.Now,
			Log: logf.FromContext(h.ctx),
		}
	})
	return ctrl.Result{}, nil
}

// get fetches a driver object. When it is gone, its instance is stopped and its devices dropped,
// and found is false.
func (h *Hub) get(ctx context.Context, key driver.InstanceKey, obj client.Object) (found bool, err error) {
	if err := h.Get(ctx, key.NamespacedName, obj); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return false, err
		}
		h.stop(key)
		h.Store.Forget(key)
		return false, nil
	}
	return true, nil
}

// declared lists the Devices a driver instance serves. id returns a Device's driver-local id, or
// "" if the Device belongs elsewhere.
func (h *Hub) declared(ctx context.Context, key driver.InstanceKey, id func(*homev1alpha1.Device) string) (
	map[types.NamespacedName]Meta, map[string]types.NamespacedName, error,
) {
	var list homev1alpha1.DeviceList
	if err := h.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
		return nil, nil, err
	}
	// Same tie-break as the operator: the first Device by name keeps a duplicated id.
	slices.SortFunc(list.Items, func(a, b homev1alpha1.Device) int { return strings.Compare(a.Name, b.Name) })
	metas := map[types.NamespacedName]Meta{}
	declared := map[string]types.NamespacedName{}
	for _, d := range list.Items {
		k := id(&d)
		if k == "" {
			continue
		}
		if _, taken := declared[k]; taken {
			continue
		}
		dev := client.ObjectKeyFromObject(&d)
		declared[k] = dev
		metas[dev] = metaFor(&d, key)
	}
	return metas, declared, nil
}

func metaFor(d *homev1alpha1.Device, instance driver.InstanceKey) Meta {
	m := Meta{DisplayName: d.Title(), Instance: instance}
	if d.Spec.RoomRef != nil {
		m.Room = d.Spec.RoomRef.Name
	}
	m.Control = d.Spec.Control
	return m
}

// subscribeTimeout bounds the wait for the broker to acknowledge a driver's subscription.
const subscribeTimeout = 10 * time.Second

// ensure keeps an instance running for key with the declared devices. An instance that current
// rejects, such as one whose topic changed, is stopped and replaced by a new one from create; a
// nil current keeps any running instance.
//
// Start and Stop wait on the broker, so they run outside h.mu, which Set and PluginStats take. A
// new instance gets its devices before Set can reach it. Reconciles of one key never overlap, so
// nothing else touches key's slot meanwhile.
func (h *Hub) ensure(key driver.InstanceKey, declared map[string]types.NamespacedName,
	current func(instance) bool, create func() instance,
) {
	h.mu.Lock()
	old := h.instances[key]
	if old != nil && (current == nil || current(old)) {
		h.mu.Unlock()
		old.SetDevices(declared)
		return
	}
	inst := create()
	inst.SetDevices(declared)
	h.instances[key] = inst
	h.mu.Unlock()

	if old != nil {
		old.Stop()
	}
	ctx, cancel := context.WithTimeout(h.ctx, subscribeTimeout)
	defer cancel()
	if err := inst.Start(ctx); err != nil {
		// The handler stays registered and the subscription is restored on reconnect; keep the instance.
		logf.FromContext(h.ctx).Error(err, "Failed to subscribe to driver topics",
			"driver", key.Driver, "instance", key.NamespacedName)
	}
}

// stop stops and removes key's instance. Like ensure, it waits on the broker outside h.mu.
func (h *Hub) stop(key driver.InstanceKey) {
	h.mu.Lock()
	inst := h.instances[key]
	delete(h.instances, key)
	h.mu.Unlock()
	if inst != nil {
		inst.Stop()
	}
}

// PluginStats returns the decoder's view of every running Plugin.
func (h *Hub) PluginStats() map[types.NamespacedName]plugin.Stats {
	type running struct {
		key  types.NamespacedName
		inst *plugin.Instance
	}
	h.mu.Lock()
	var plugins []running
	for key, inst := range h.instances {
		if p, ok := inst.(*plugin.Instance); ok {
			plugins = append(plugins, running{key.NamespacedName, p})
		}
	}
	h.mu.Unlock()
	out := make(map[types.NamespacedName]plugin.Stats, len(plugins))
	for _, p := range plugins {
		out[p.key] = p.inst.Stats()
	}
	return out
}

// SetupWithManager registers the hub's controllers, one per driver kind. name must be unique
// within the manager; the Plugin controller is named <name>-plugin.
func (h *Hub) SetupWithManager(ctx context.Context, mgr ctrl.Manager, name string) error {
	h.ctx = ctx
	h.instances = map[driver.InstanceKey]instance{}
	if h.Now == nil {
		h.Now = time.Now
	}
	// Status writes, the hub's own included, change no generation and need no reconcile.
	// GenerationChangedPredicate passes creates and deletes.
	specChanged := builder.WithPredicates(predicate.GenerationChangedPredicate{})
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&zigbeev1alpha1.Zigbee2MQTT{}, specChanged).
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(zigbee2mqttForDevice), specChanged).
		Named(name).
		Complete(reconcile.Func(h.reconcileZigbee2MQTT)); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&pluginsv1alpha1.Plugin{}, specChanged).
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(pluginForDevice), specChanged).
		Named(name + "-plugin").
		Complete(reconcile.Func(h.reconcilePlugin))
}

func zigbee2mqttForDevice(_ context.Context, obj client.Object) []reconcile.Request {
	d := obj.(*homev1alpha1.Device)
	if d.Spec.Zigbee2MQTT == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: d.Namespace, Name: d.Spec.Zigbee2MQTT.InstanceRef.Name,
	}}}
}

func pluginForDevice(_ context.Context, obj client.Object) []reconcile.Request {
	d := obj.(*homev1alpha1.Device)
	if d.Spec.Plugin == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: d.Namespace, Name: d.Spec.Plugin.PluginRef.Name,
	}}}
}
