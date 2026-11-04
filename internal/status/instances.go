package status

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
)

// Discovered is a driver.Sink that writes each driver instance's inventory, the devices it sees in
// its app, to status.discovered on its Plugin or Zigbee2MQTT, where the operator can adopt them
// (OIK-123). The list changes only when devices are paired, removed or renamed, and is written
// only when it differs, so it stays coarse.
type Discovered struct {
	hub.NopSink
	client.Client
	Store *hub.Store

	plugins chan event.GenericEvent
	zigbee  chan event.GenericEvent
}

// Inventory implements driver.Sink.
func (w *Discovered) Inventory(instance driver.InstanceKey, _ []driver.AppDevice) {
	var obj client.Object
	events := w.plugins
	switch instance.Driver {
	case driver.Plugin:
		obj = &pluginsv1alpha1.Plugin{}
	case driver.Zigbee2MQTT:
		obj, events = &zigbeev1alpha1.Zigbee2MQTT{}, w.zigbee
	default:
		return
	}
	obj.SetName(instance.Name)
	obj.SetNamespace(instance.Namespace)
	// Never block the driver. A dropped event is caught by the next inventory or the resync.
	select {
	case events <- event.GenericEvent{Object: obj}:
	default:
	}
}

// discovered converts a reported inventory to status: nil devices (the instance doesn't list
// them) clears the field, and an empty inventory is an empty list.
func discovered(devices []driver.AppDevice) []homev1alpha1.DiscoveredDevice {
	if devices == nil {
		return nil
	}
	out := make([]homev1alpha1.DiscoveredDevice, 0, len(devices))
	for _, d := range devices {
		out = append(out, homev1alpha1.DiscoveredDevice{
			ID: d.ID, Name: truncate(d.Name, maxNameLen), Room: truncate(d.Room, maxNameLen),
			Manufacturer: truncate(d.Manufacturer, maxNameLen), Model: truncate(d.Model, maxNameLen),
			Capabilities: d.Capabilities,
		})
	}
	return out
}

// maxNameLen bounds a name from an app, as the CRD does.
const maxNameLen = 128

// resync rewrites the list now and then, in case an inventory event was dropped.
const resync = 10 * time.Minute

// reconcile writes one instance's status.discovered. field points into obj's status.
func (w *Discovered) reconcile(ctx context.Context, req ctrl.Request, drv string, obj client.Object,
	field *[]homev1alpha1.DiscoveredDevice,
) (ctrl.Result, error) {
	if err := w.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	devices, reported := w.Store.InventoryOf(driver.InstanceKey{Driver: drv, NamespacedName: req.NamespacedName})
	if !reported {
		// The driver hasn't reported since the runtime started: keep what's there, rather than
		// clear it on every restart.
		return ctrl.Result{RequeueAfter: resync}, nil
	}
	want := discovered(devices)
	if same(*field, want) {
		return ctrl.Result{RequeueAfter: resync}, nil
	}
	orig, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return ctrl.Result{}, fmt.Errorf("cannot copy %T", obj)
	}
	*field = want
	if err := w.Status().Patch(ctx, obj, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: resync}, nil
}

// same is equality.Semantic, except that nil (no list) and empty (an app with no devices) differ.
func same(a, b []homev1alpha1.DiscoveredDevice) bool {
	return (a == nil) == (b == nil) && equality.Semantic.DeepEqual(a, b)
}

func (w *Discovered) reconcilePlugin(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p pluginsv1alpha1.Plugin
	return w.reconcile(ctx, req, driver.Plugin, &p, &p.Status.Discovered)
}

func (w *Discovered) reconcileZigbee2MQTT(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var z zigbeev1alpha1.Zigbee2MQTT
	return w.reconcile(ctx, req, driver.Zigbee2MQTT, &z, &z.Status.Discovered)
}

// SetupWithManager registers a controller for each kind.
func (w *Discovered) SetupWithManager(mgr ctrl.Manager) error {
	w.plugins, w.zigbee = make(chan event.GenericEvent, 64), make(chan event.GenericEvent, 64)
	if err := ctrl.NewControllerManagedBy(mgr).
		// Only spec changes and creation matter; our own status patches would otherwise requeue.
		For(&pluginsv1alpha1.Plugin{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(w.plugins, &handler.EnqueueRequestForObject{})).
		Named("plugin-discovered").
		Complete(reconcile.Func(w.reconcilePlugin)); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&zigbeev1alpha1.Zigbee2MQTT{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(w.zigbee, &handler.EnqueueRequestForObject{})).
		Named("zigbee2mqtt-discovered").
		Complete(reconcile.Func(w.reconcileZigbee2MQTT))
}
