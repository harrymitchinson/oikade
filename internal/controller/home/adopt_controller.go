package home

import (
	"context"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/controller/shared"
)

// Adoption drivers: the kinds whose discovered devices AdoptReconciler adopts.
const (
	AdoptPlugin      = "plugin"
	AdoptZigbee2MQTT = "zigbee2mqtt"
)

// AdoptReconciler creates a Device for each device a Plugin or Zigbee2MQTT instance discovers in
// its app that no Device declares, when the instance's spec.discovery.adopt is set (OIK-123). It
// writes a Device's spec once, then follows only a plugin device's name in its app. An adopted
// Device has the instance as its controller owner; a Device declared elsewhere with the same ID,
// or a spec written by anything but the operator, takes it over.
type AdoptReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Driver is AdoptPlugin or AdoptZigbee2MQTT.
	Driver string
}

// +kubebuilder:rbac:groups=home.oikade.dev,resources=devices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=home.oikade.dev,resources=rooms,verbs=get;list;watch
// +kubebuilder:rbac:groups=plugins.oikade.dev,resources=plugins,verbs=get;list;watch
// +kubebuilder:rbac:groups=zigbee.oikade.dev,resources=zigbee2mqtts,verbs=get;list;watch

// Reconcile adopts one instance's discovered devices.
func (r *AdoptReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	inst := r.newInstance()
	if err := r.Get(ctx, req.NamespacedName, inst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	discovery, discovered := r.discovery(inst)
	if discovery == nil || !discovery.Adopt || discovered == nil {
		return ctrl.Result{}, nil
	}
	var devices homev1alpha1.DeviceList
	if err := r.List(ctx, &devices, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	var rooms homev1alpha1.RoomList
	if err := r.List(ctx, &rooms, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	taken := map[string]bool{}
	byID := map[string][]*homev1alpha1.Device{}
	for i := range devices.Items {
		d := &devices.Items[i]
		taken[d.Name] = true
		if id, ok := r.idOf(d, inst.GetName()); ok {
			byID[id] = append(byID[id], d)
		}
	}
	names := map[string]string{} // id -> name in the app
	for _, e := range discovered {
		names[e.ID] = e.Name
	}

	for _, ds := range byID {
		// Oldest first, so a second adopted copy, from a stale cache, is the one that goes.
		slices.SortFunc(ds, func(a, b *homev1alpha1.Device) int {
			if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
				return c
			}
			return strings.Compare(a.Name, b.Name)
		})
		kept := slices.ContainsFunc(ds, func(d *homev1alpha1.Device) bool {
			return !metav1.IsControlledBy(d, inst) || takenOver(d)
		})
		for _, d := range ds {
			if !metav1.IsControlledBy(d, inst) {
				continue
			}
			switch {
			case takenOver(d):
				if err := r.release(ctx, d, inst); err != nil {
					return ctrl.Result{}, err
				}
				log.Info("Released adopted Device, its spec is managed elsewhere", "device", d.Name)
			case kept:
				// Another Device declares the same device: it wins, and this copy goes.
				if err := r.Delete(ctx, d); client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, err
				}
				log.Info("Deleted adopted Device, declared by another Device", "device", d.Name)
			case r.Driver == AdoptPlugin:
				if err := r.followName(ctx, d, names); err != nil {
					return ctrl.Result{}, err
				}
			}
			kept = true
		}
	}

	for _, e := range discovered {
		if _, declared := byID[e.ID]; declared {
			continue
		}
		d, err := r.newDevice(inst, e, discovery, rooms.Items, taken)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, d, client.FieldOwner(shared.FieldOwner)); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil // a Device appeared under that name meanwhile
			}
			if apierrors.IsInvalid(err) {
				log.Error(err, "Could not adopt device", "id", e.ID)
				continue
			}
			return ctrl.Result{}, err
		}
		taken[d.Name] = true
		log.Info("Adopted Device", "device", d.Name, "id", e.ID)
	}
	return ctrl.Result{}, nil
}

// newDevice builds the Device for a discovered device.
func (r *AdoptReconciler) newDevice(inst client.Object, e homev1alpha1.DiscoveredDevice,
	discovery *homev1alpha1.DiscoverySpec, rooms []homev1alpha1.Room, taken map[string]bool,
) (*homev1alpha1.Device, error) {
	d := &homev1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: adoptedName(e, taken), Namespace: inst.GetNamespace()},
		Spec: homev1alpha1.DeviceSpec{
			DisplayName: displayName(e.Name),
			RoomRef:     roomFor(e.Room, rooms),
		},
	}
	if discovery.Matter == homev1alpha1.DiscoveryMatterHide {
		d.Annotations = map[string]string{homev1alpha1.AnnotationMatter: homev1alpha1.MatterHide}
	}
	ref := corev1.LocalObjectReference{Name: inst.GetName()}
	switch r.Driver {
	case AdoptPlugin:
		d.Spec.Plugin = &homev1alpha1.PluginDeviceSource{PluginRef: ref, ID: e.ID}
	case AdoptZigbee2MQTT:
		d.Spec.Zigbee2MQTT = &homev1alpha1.ZigbeeDeviceSource{InstanceRef: ref, IEEEAddress: e.ID}
	}
	return d, controllerutil.SetControllerReference(inst, d, r.Scheme)
}

// followName sets an adopted plugin Device's displayName to the device's name in its app.
func (r *AdoptReconciler) followName(ctx context.Context, d *homev1alpha1.Device, names map[string]string) error {
	name, ok := names[d.Spec.Plugin.ID]
	if !ok || name == "" || displayName(name) == d.Spec.DisplayName {
		return nil
	}
	orig := d.DeepCopy()
	d.Spec.DisplayName = displayName(name)
	return client.IgnoreNotFound(r.Patch(ctx, d, client.MergeFrom(orig), client.FieldOwner(shared.FieldOwner)))
}

// release drops the instance's owner reference, so the Device is no longer adopted.
func (r *AdoptReconciler) release(ctx context.Context, d *homev1alpha1.Device, inst client.Object) error {
	orig := d.DeepCopy()
	d.OwnerReferences = slices.DeleteFunc(d.OwnerReferences, func(o metav1.OwnerReference) bool {
		return o.UID == inst.GetUID()
	})
	return client.IgnoreNotFound(r.Patch(ctx, d, client.MergeFrom(orig), client.FieldOwner(shared.FieldOwner)))
}

func (r *AdoptReconciler) newInstance() client.Object {
	if r.Driver == AdoptZigbee2MQTT {
		return &zigbeev1alpha1.Zigbee2MQTT{}
	}
	return &pluginsv1alpha1.Plugin{}
}

func (r *AdoptReconciler) discovery(inst client.Object) (*homev1alpha1.DiscoverySpec, []homev1alpha1.DiscoveredDevice) {
	switch o := inst.(type) {
	case *pluginsv1alpha1.Plugin:
		return o.Spec.Discovery, o.Status.Discovered
	case *zigbeev1alpha1.Zigbee2MQTT:
		return o.Spec.Discovery, o.Status.Discovered
	}
	return nil, nil
}

// idOf returns the ID a Device declares on this driver's instance.
func (r *AdoptReconciler) idOf(d *homev1alpha1.Device, instance string) (string, bool) {
	switch {
	case r.Driver == AdoptPlugin && d.Spec.Plugin != nil && d.Spec.Plugin.PluginRef.Name == instance:
		return d.Spec.Plugin.ID, true
	case r.Driver == AdoptZigbee2MQTT && d.Spec.Zigbee2MQTT != nil && d.Spec.Zigbee2MQTT.InstanceRef.Name == instance:
		return d.Spec.Zigbee2MQTT.IEEEAddress, true
	}
	return "", false
}

// instanceOf maps a Device to the instance of this driver it declares a device on.
func (r *AdoptReconciler) instanceOf(_ context.Context, obj client.Object) []reconcile.Request {
	d, ok := obj.(*homev1alpha1.Device)
	if !ok {
		return nil
	}
	var name string
	switch {
	case r.Driver == AdoptPlugin && d.Spec.Plugin != nil:
		name = d.Spec.Plugin.PluginRef.Name
	case r.Driver == AdoptZigbee2MQTT && d.Spec.Zigbee2MQTT != nil:
		name = d.Spec.Zigbee2MQTT.InstanceRef.Name
	default:
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: d.Namespace, Name: name}}}
}

// SetupWithManager registers the controller. Status changes count: status.discovered is what it adopts.
func (r *AdoptReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(r.newInstance()).
		// Device status changes with every reading; only spec, annotations (a client-side apply's)
		// and creation or deletion matter here.
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(r.instanceOf),
			builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Named(r.Driver + "-adopt").
		Complete(r)
}
