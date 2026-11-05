package zigbee

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/controller/shared"
	"github.com/harrymitchinson/oikade/internal/render"
)

const (
	ConditionReady = "Ready"
)

// Zigbee2MQTTReconciler reconciles a Zigbee2MQTT object
type Zigbee2MQTTReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Image is the default Zigbee2MQTT image.
	Image string
	// MQTTURL is the broker Zigbee2MQTT connects to.
	MQTTURL string
}

// +kubebuilder:rbac:groups=zigbee.oikade.dev,resources=zigbee2mqtts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zigbee.oikade.dev,resources=zigbee2mqtts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zigbee.oikade.dev,resources=zigbee2mqtts/finalizers,verbs=update
// +kubebuilder:rbac:groups=home.oikade.dev,resources=devices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch

// Reconcile drives a Zigbee2MQTT instance's network identity, configuration and Deployment.
func (r *Zigbee2MQTTReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var z zigbeev1alpha1.Zigbee2MQTT
	if err := r.Get(ctx, req.NamespacedName, &z); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !z.DeletionTimestamp.IsZero() {
		// Owned objects are garbage collected; the network Secret and PVC are retained on purpose.
		return ctrl.Result{}, nil
	}

	orig := z.DeepCopy()
	err := r.reconcile(ctx, &z)
	z.Status.ObservedGeneration = z.Generation
	if !equality.Semantic.DeepEqual(orig.Status, z.Status) {
		if serr := r.Status().Patch(ctx, &z, client.MergeFrom(orig)); serr != nil && err == nil {
			err = serr
		}
	}
	return ctrl.Result{}, err
}

func (r *Zigbee2MQTTReconciler) reconcile(ctx context.Context, z *zigbeev1alpha1.Zigbee2MQTT) error {
	network, err := r.ensureNetwork(ctx, z)
	if err != nil {
		if invalid, ok := errors.AsType[*invalidNetworkError](err); ok {
			r.setReady(z, metav1.ConditionFalse, "InvalidNetworkSecret", invalid.Error())
			return nil
		}
		return err
	}
	z.Status.PanID = int32(network.PanID)

	devices, duplicates, err := r.devices(ctx, z)
	if err != nil {
		return err
	}
	z.Status.Devices = int32(len(devices))

	config, err := render.ZigbeeConfig(render.ZigbeeConfigInput{Instance: z, Network: network, MQTTURL: r.MQTTURL})
	if err != nil {
		return fmt.Errorf("rendering configuration.yaml: %w", err)
	}
	// The seed is read only when the PVC has no devices.yaml, so it stays out of the hash:
	// declaring a Device doesn't restart Zigbee2MQTT.
	seed, err := render.ZigbeeDevicesSeed(devices)
	if err != nil {
		return fmt.Errorf("rendering devices.yaml: %w", err)
	}
	hash := render.Hash(config)

	if err := r.apply(ctx, render.ZigbeeConfigSecret(z, config, seed)); err != nil {
		return fmt.Errorf("applying config secret: %w", err)
	}
	if err := r.ensurePVC(ctx, z); err != nil {
		return err
	}
	image := z.Spec.Image
	if image == "" {
		image = r.Image
	}
	if err := r.apply(ctx, render.ZigbeeDeployment(z, image, hash)); err != nil {
		return fmt.Errorf("applying deployment: %w", err)
	}
	if err := r.reconcileService(ctx, z); err != nil {
		return err
	}
	z.Status.ConfigHash = hash

	var dep appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(z), &dep); err != nil {
		return client.IgnoreNotFound(err)
	}
	switch {
	case len(duplicates) > 0:
		r.setReady(z, metav1.ConditionFalse, "DuplicateDevice",
			"IEEE addresses claimed by more than one Device (first by name wins): "+strings.Join(duplicates, ", "))
	case shared.DeploymentAvailable(&dep):
		r.setReady(z, metav1.ConditionTrue, "Available", "Zigbee2MQTT is running")
	default:
		r.setReady(z, metav1.ConditionFalse, "Progressing", "waiting for the Zigbee2MQTT pod to become ready")
	}
	return nil
}

type invalidNetworkError struct{ msg string }

func (e *invalidNetworkError) Error() string { return e.msg }

// ensureNetwork returns the instance's Zigbee network identity, generating it once.
// An existing Secret is never replaced: a new network key would mean re-pairing every device.
func (r *Zigbee2MQTTReconciler) ensureNetwork(ctx context.Context, z *zigbeev1alpha1.Zigbee2MQTT) (render.ZigbeeNetwork, error) {
	var s corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: render.ZigbeeNetworkSecretName(z)}, &s)
	if err == nil {
		n, err := render.ParseZigbeeNetwork(s.Data)
		if err != nil {
			return n, &invalidNetworkError{fmt.Sprintf("Secret %s: %v; fix it by hand", s.Name, err)}
		}
		return n, nil
	}
	if !apierrors.IsNotFound(err) {
		return render.ZigbeeNetwork{}, err
	}
	n, err := render.NewZigbeeNetwork()
	if err != nil {
		return n, err
	}
	if err := r.Create(ctx, render.ZigbeeNetworkSecret(z, n)); err != nil {
		// AlreadyExists means our cache is stale; requeue and read the real one.
		return n, fmt.Errorf("creating network secret: %w", err)
	}
	logf.FromContext(ctx).Info("Generated Zigbee network", "secret", render.ZigbeeNetworkSecretName(z), "panID", n.PanID)
	return n, nil
}

// devices returns the Devices declared for this instance, sorted by IEEE address, and any
// addresses claimed by more than one Device.
func (r *Zigbee2MQTTReconciler) devices(ctx context.Context, z *zigbeev1alpha1.Zigbee2MQTT) ([]render.ZigbeeDevice, []string, error) {
	var list homev1alpha1.DeviceList
	if err := r.List(ctx, &list, client.InNamespace(z.Namespace)); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(list.Items, func(a, b homev1alpha1.Device) int { return strings.Compare(a.Name, b.Name) })
	byAddr := map[string]string{}
	var duplicates []string
	for _, d := range list.Items {
		src := d.Spec.Zigbee2MQTT
		if src == nil || src.InstanceRef.Name != z.Name {
			continue
		}
		if _, taken := byAddr[src.IEEEAddress]; taken {
			duplicates = append(duplicates, src.IEEEAddress)
			continue
		}
		byAddr[src.IEEEAddress] = d.Name
	}
	out := make([]render.ZigbeeDevice, 0, len(byAddr))
	for addr, name := range byAddr {
		out = append(out, render.ZigbeeDevice{IEEEAddress: addr, FriendlyName: name})
	}
	slices.SortFunc(out, func(a, b render.ZigbeeDevice) int { return strings.Compare(a.IEEEAddress, b.IEEEAddress) })
	return out, slices.Compact(duplicates), nil
}

func (r *Zigbee2MQTTReconciler) ensurePVC(ctx context.Context, z *zigbeev1alpha1.Zigbee2MQTT) error {
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: render.ZigbeePVCName(z)}, &pvc)
	if apierrors.IsNotFound(err) {
		err = r.Create(ctx, render.ZigbeePVC(z))
	}
	if err != nil {
		return fmt.Errorf("ensuring pvc: %w", err)
	}
	return nil
}

func (r *Zigbee2MQTTReconciler) reconcileService(ctx context.Context, z *zigbeev1alpha1.Zigbee2MQTT) error {
	if z.Spec.Frontend {
		if err := r.apply(ctx, render.ZigbeeService(z)); err != nil {
			return fmt.Errorf("applying service: %w", err)
		}
		return nil
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: z.Name, Namespace: z.Namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(svc), svc); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(svc, z) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, svc))
}

func (r *Zigbee2MQTTReconciler) apply(ctx context.Context, obj runtime.ApplyConfiguration) error {
	return shared.Apply(ctx, r.Client, obj)
}

func (r *Zigbee2MQTTReconciler) setReady(z *zigbeev1alpha1.Zigbee2MQTT, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&z.Status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: status, Reason: reason, Message: msg, ObservedGeneration: z.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *Zigbee2MQTTReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&zigbeev1alpha1.Zigbee2MQTT{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(instanceForDevice)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(instanceForLabel)).
		Named("zigbee-zigbee2mqtt").
		Complete(r)
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

// instanceForLabel maps retained objects (the network Secret) back to their instance.
func instanceForLabel(_ context.Context, obj client.Object) []reconcile.Request {
	name, ok := obj.GetLabels()[zigbeev1alpha1.LabelInstance]
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}
