package status

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
	// Never block the driver. A dropped event is caught by the next inventory or a resync.
	select {
	case events <- event.GenericEvent{Object: obj}:
	default:
	}
}

// discovered converts an inventory to status. ok false means none, so the field is cleared.
func discovered(devices []driver.AppDevice, ok bool) []homev1alpha1.DiscoveredDevice {
	if !ok || len(devices) == 0 {
		// An empty list and none both clear the field: omitempty can't keep an empty list.
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

func (w *Discovered) reconcilePlugin(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p pluginsv1alpha1.Plugin
	if err := w.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	want := discovered(w.Store.InventoryOf(key(driver.Plugin, req.NamespacedName)))
	if equality.Semantic.DeepEqual(p.Status.Discovered, want) {
		return ctrl.Result{}, nil
	}
	orig := p.DeepCopy()
	p.Status.Discovered = want
	return ctrl.Result{}, client.IgnoreNotFound(w.Status().Patch(ctx, &p, client.MergeFrom(orig)))
}

func (w *Discovered) reconcileZigbee2MQTT(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var z zigbeev1alpha1.Zigbee2MQTT
	if err := w.Get(ctx, req.NamespacedName, &z); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	want := discovered(w.Store.InventoryOf(key(driver.Zigbee2MQTT, req.NamespacedName)))
	if equality.Semantic.DeepEqual(z.Status.Discovered, want) {
		return ctrl.Result{}, nil
	}
	orig := z.DeepCopy()
	z.Status.Discovered = want
	return ctrl.Result{}, client.IgnoreNotFound(w.Status().Patch(ctx, &z, client.MergeFrom(orig)))
}

func key(drv string, nn types.NamespacedName) driver.InstanceKey {
	return driver.InstanceKey{Driver: drv, NamespacedName: nn}
}

// SetupWithManager registers a controller for each kind.
func (w *Discovered) SetupWithManager(mgr ctrl.Manager) error {
	w.plugins, w.zigbee = make(chan event.GenericEvent, 64), make(chan event.GenericEvent, 64)
	if err := ctrl.NewControllerManagedBy(mgr).
		// Only spec changes and creation matter; our own status patches would otherwise requeue.
		For(&pluginsv1alpha1.Plugin{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(w.plugins, &handler.EnqueueRequestForObject{})).
		Named("plugin-discovered").
		Complete(reconcile(w.reconcilePlugin)); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&zigbeev1alpha1.Zigbee2MQTT{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(w.zigbee, &handler.EnqueueRequestForObject{})).
		Named("zigbee2mqtt-discovered").
		Complete(reconcile(w.reconcileZigbee2MQTT))
}

type reconcile func(context.Context, ctrl.Request) (ctrl.Result, error)

func (f reconcile) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return f(ctx, req)
}
