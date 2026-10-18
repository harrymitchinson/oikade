/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
	zigbeev1alpha1 "github.com/harry/oikade/api/zigbee/v1alpha1"
	"github.com/harry/oikade/internal/capability"
	"github.com/harry/oikade/internal/driver"
	"github.com/harry/oikade/internal/driver/plugin"
	"github.com/harry/oikade/internal/driver/zigbee2mqtt"
	"github.com/harry/oikade/internal/mqtt"
)

// Hub keeps a driver instance running for every driver object, such as a Zigbee2MQTT, feeds the
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

// Reconcile keeps one Zigbee2MQTT instance's driver and devices in sync.
func (h *Hub) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: req.NamespacedName}
	var z zigbeev1alpha1.Zigbee2MQTT
	if err := h.Get(ctx, req.NamespacedName, &z); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		h.stop(key)
		h.Store.SetDevices(key, nil)
		return ctrl.Result{}, nil
	}

	var list homev1alpha1.DeviceList
	if err := h.List(ctx, &list, client.InNamespace(z.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	// Same tie-break as the operator: the first Device by name keeps a duplicated address.
	slices.SortFunc(list.Items, func(a, b homev1alpha1.Device) int { return strings.Compare(a.Name, b.Name) })
	metas := map[types.NamespacedName]Meta{}
	declared := map[string]types.NamespacedName{}
	for _, d := range list.Items {
		src := d.Spec.Zigbee2MQTT
		if src == nil || src.InstanceRef.Name != z.Name {
			continue
		}
		if _, taken := declared[src.IEEEAddress]; taken {
			continue
		}
		dev := client.ObjectKeyFromObject(&d)
		declared[src.IEEEAddress] = dev
		metas[dev] = metaFor(&d, key)
	}

	inst := h.ensure(key, func(i instance) bool {
		return i.(*zigbee2mqtt.Instance).BaseTopic == z.Topic()
	}, func() instance {
		inst := &zigbee2mqtt.Instance{
			Key: req.NamespacedName, BaseTopic: z.Topic(), Bus: h.Bus, Sink: h.Store, Now: h.Now,
			Log: logf.FromContext(h.ctx).WithValues("instance", req.NamespacedName),
		}
		if err := inst.Start(h.ctx); err != nil {
			// The subscription is restored on reconnect; keep the instance.
			logf.FromContext(h.ctx).Error(err, "Failed to subscribe to Zigbee2MQTT topics", "instance", req.NamespacedName)
		}
		return inst
	})
	// The store must know the devices before the driver reports on them.
	h.Store.SetDevices(key, metas)
	inst.SetDevices(declared)
	return ctrl.Result{}, nil
}

func metaFor(d *homev1alpha1.Device, instance driver.InstanceKey) Meta {
	m := Meta{DisplayName: d.Title(), Instance: instance}
	if d.Spec.RoomRef != nil {
		m.Room = d.Spec.RoomRef.Name
	}
	if d.Spec.HomeKit != nil {
		m.HomeKitService = string(d.Spec.HomeKit.Service)
	}
	return m
}

// ensure returns the running instance for key. An instance that current rejects, such as one
// whose topic changed, is stopped and replaced by start.
func (h *Hub) ensure(key driver.InstanceKey, current func(instance) bool, start func() instance) instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inst := h.instances[key]; inst != nil {
		if current(inst) {
			return inst
		}
		inst.Stop()
		delete(h.instances, key)
	}
	inst := start()
	h.instances[key] = inst
	return inst
}

func (h *Hub) stop(key driver.InstanceKey) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inst := h.instances[key]; inst != nil {
		inst.Stop()
		delete(h.instances, key)
	}
}

// SetupWithManager registers the hub as a controller. name must be unique within the manager.
func (h *Hub) SetupWithManager(ctx context.Context, mgr ctrl.Manager, name string) error {
	h.ctx = ctx
	h.instances = map[driver.InstanceKey]instance{}
	if h.Now == nil {
		h.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&zigbeev1alpha1.Zigbee2MQTT{}).
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(instanceForDevice)).
		Named(name).
		Complete(h)
}

func instanceForDevice(_ context.Context, obj client.Object) []reconcile.Request {
	d := obj.(*homev1alpha1.Device)
	if d.Spec.Zigbee2MQTT == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: d.Namespace, Name: d.Spec.Zigbee2MQTT.InstanceRef.Name,
	}}}
}
