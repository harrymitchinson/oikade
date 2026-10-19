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

// Package status writes the driver's coarse view of each device to Device status.
package status

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
)

// Writer is a driver.Sink that queues Devices for a status update, and the controller that writes
// them. Live values never reach the API server; only capabilities, availability and a throttled
// lastSeen do.
type Writer struct {
	hub.NopSink
	client.Client
	Store *hub.Store
	// SeenInterval is how stale status.lastSeen may get before it is refreshed.
	SeenInterval time.Duration

	events chan event.GenericEvent
}

// The runtime's permissions are in config/runtime/role.yaml, not the operator's generated role.

// Discovered implements driver.Sink.
func (w *Writer) Discovered(dev types.NamespacedName, _ driver.Info) { w.enqueue(dev) }

// Availability implements driver.Sink.
func (w *Writer) Availability(dev types.NamespacedName, _ bool) { w.enqueue(dev) }

// Seen implements driver.Sink.
func (w *Writer) Seen(dev types.NamespacedName, _ time.Time) { w.enqueue(dev) }

// Reassigned implements hub.Reassigner: whether a driver instance claims the Device is in its status.
func (w *Writer) Reassigned(dev types.NamespacedName) { w.enqueue(dev) }

var _ hub.Reassigner = (*Writer)(nil)

// enqueue never blocks the driver. A dropped event is caught by the periodic requeue.
func (w *Writer) enqueue(dev types.NamespacedName) {
	obj := &homev1alpha1.Device{}
	obj.Name, obj.Namespace = dev.Name, dev.Namespace
	select {
	case w.events <- event.GenericEvent{Object: obj}:
	default:
	}
}

// Reconcile writes one Device's status from the store.
func (w *Writer) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var d homev1alpha1.Device
	if err := w.Get(ctx, req.NamespacedName, &d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := d.DeepCopy()
	snap, ok := w.Store.Get(req.NamespacedName)
	compute(&d, snap, ok, w.SeenInterval)
	if !equality.Semantic.DeepEqual(orig.Status, d.Status) {
		if err := w.Status().Patch(ctx, &d, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: w.SeenInterval}, nil
}

// compute derives Device status from a store snapshot. known is false when no running driver
// instance claims the device.
func compute(d *homev1alpha1.Device, snap hub.Snapshot, known bool, seenInterval time.Duration) {
	resolved := metav1.Condition{Type: homev1alpha1.ConditionResolved, ObservedGeneration: d.Generation}
	switch {
	case !known:
		resolved.Status, resolved.Reason, resolved.Message = metav1.ConditionFalse, "NoDriver", noDriverMessage(d)
	case !snap.Discovered:
		resolved.Status, resolved.Reason, resolved.Message = metav1.ConditionUnknown, "Waiting", "Waiting for the driver"
	case snap.Info.Error != "":
		resolved.Status, resolved.Reason, resolved.Message = metav1.ConditionFalse, "PluginError", truncate(snap.Info.Error, maxErrorLen)
	case !snap.Info.Found:
		resolved.Status, resolved.Reason = metav1.ConditionFalse, "NotPaired"
		resolved.Message = fmt.Sprintf("The device is not paired with %s", snap.Instance.Name)
		if d.Spec.Plugin != nil {
			resolved.Message = fmt.Sprintf("Plugin %s has not found the device", snap.Instance.Name)
		}
	default:
		resolved.Status, resolved.Reason = metav1.ConditionTrue, "Paired"
		resolved.Message = fmt.Sprintf("%d capabilities", len(snap.Info.Capabilities))
	}
	meta.SetStatusCondition(&d.Status.Conditions, resolved)

	if known && snap.Discovered {
		d.Status.Capabilities = nil
		for _, name := range snap.Info.Capabilities {
			if c, ok := capability.Status(name); ok {
				d.Status.Capabilities = append(d.Status.Capabilities, c)
			}
		}
		d.Status.Manufacturer, d.Status.Model = snap.Info.Manufacturer, snap.Info.Model
	}

	available := metav1.Condition{Type: homev1alpha1.ConditionAvailable, ObservedGeneration: d.Generation}
	switch {
	case !known || snap.Online == nil:
		available.Status, available.Reason, available.Message = metav1.ConditionUnknown, "NoReport", "No availability report yet"
	case *snap.Online:
		available.Status, available.Reason = metav1.ConditionTrue, "Online"
	default:
		available.Status, available.Reason = metav1.ConditionFalse, "Offline"
	}
	meta.SetStatusCondition(&d.Status.Conditions, available)

	if known && !snap.LastSeen.IsZero() && snap.LastSeen.Sub(d.Status.LastSeen.Time) >= seenInterval {
		d.Status.LastSeen = metav1.NewTime(snap.LastSeen.Truncate(time.Second))
	}
}

func noDriverMessage(d *homev1alpha1.Device) string {
	if d.Spec.Plugin != nil {
		return "No running driver instance claims this device; check spec.plugin.pluginRef " +
			"and that no other Device uses the same plugin id"
	}
	return "No running driver instance claims this device; check spec.zigbee2mqtt.instanceRef " +
		"and that no other Device uses the same IEEE address"
}

// maxErrorLen bounds a driver-reported error in a condition message.
const maxErrorLen = 256

// truncate shortens s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ellipsis = "…"
	cut := n - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// SetupWithManager registers the writer.
func (w *Writer) SetupWithManager(mgr ctrl.Manager) error {
	w.events = make(chan event.GenericEvent, 1024)
	return ctrl.NewControllerManagedBy(mgr).
		// Only spec changes matter; our own status patches would otherwise requeue the Device.
		For(&homev1alpha1.Device{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(w.events, &handler.EnqueueRequestForObject{})).
		Named("device-status").
		Complete(w)
}
