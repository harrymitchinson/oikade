package matter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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
	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/controller/shared"
	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/node"
	"github.com/harrymitchinson/oikade/internal/matter/setup"
	"github.com/harrymitchinson/oikade/internal/matterbridge"
	"github.com/harrymitchinson/oikade/internal/ports"
	"github.com/harrymitchinson/oikade/internal/render"
)

const (
	ConditionReady         = "Ready"
	ConditionPortAllocated = "PortAllocated"
	// ConditionDevicesSupported is False when a selected Device has nothing Matter can show. It
	// keeps its endpoint ID, but isn't on the bridge.
	ConditionDevicesSupported = "DevicesSupported"

	reasonInvalidSecret = "InvalidSecret"
)

// BridgeReconciler reconciles a Matter Bridge object
type BridgeReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Image runs the Matter bridge (the operator's own image).
	Image   string
	MQTTURL string

	ports ports.Reserver
}

// +kubebuilder:rbac:groups=matter.oikade.dev,resources=bridges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=matter.oikade.dev,resources=bridges/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=matter.oikade.dev,resources=bridges/finalizers,verbs=update
// The operator must hold every permission it grants to the bridge pod's Role.
// +kubebuilder:rbac:groups=home.oikade.dev,resources=devices,verbs=get;list;watch
// +kubebuilder:rbac:groups=zigbee.oikade.dev,resources=zigbee2mqtts,verbs=get;list;watch
// +kubebuilder:rbac:groups=plugins.oikade.dev,resources=plugins,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch

// Reconcile drives a Bridge's Secret, port, endpoint IDs, PVC and Deployment.
func (r *BridgeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b matterv1alpha1.Bridge
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		if apierrors.IsNotFound(err) {
			r.release(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() {
		// Owned objects are garbage collected; the Secret and PVC are retained on purpose.
		return ctrl.Result{}, nil
	}

	orig := b.DeepCopy()
	res, err := r.reconcile(ctx, &b)
	b.Status.ObservedGeneration = b.Generation
	if !equality.Semantic.DeepEqual(orig.Status, b.Status) {
		if serr := r.Status().Patch(ctx, &b, client.MergeFrom(orig)); serr != nil {
			return ctrl.Result{}, errors.Join(err, serr)
		}
	}
	return res, err
}

// userError is a problem the user fixes in the Bridge, its Secret or its Devices. It's reported
// as Ready=False with its reason, and not retried until something changes.
type userError struct{ reason, msg string }

func (e *userError) Error() string { return e.msg }

func (r *BridgeReconciler) reconcile(ctx context.Context, b *matterv1alpha1.Bridge) (ctrl.Result, error) {
	res, err := r.reconcileObjects(ctx, b)
	if ue, ok := errors.AsType[*userError](err); ok {
		r.setCondition(b, ConditionReady, metav1.ConditionFalse, ue.reason, ue.msg)
		return ctrl.Result{}, nil
	}
	return res, err
}

func (r *BridgeReconciler) reconcileObjects(ctx context.Context, b *matterv1alpha1.Bridge) (ctrl.Result, error) {
	secret, err := r.ensureSecret(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}
	s, err := setupOf(secret)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := setPairing(b, s); err != nil {
		return ctrl.Result{}, err
	}

	if b.Status.Port == 0 || !b.Spec.PortRange.Contains(b.Status.Port) {
		port, err := r.allocatePort(ctx, b)
		if errors.Is(err, ports.ErrExhausted) {
			msg := fmt.Sprintf("no free port in %d-%d", b.Spec.PortRange.Start, b.Spec.PortRange.End)
			r.setCondition(b, ConditionPortAllocated, metav1.ConditionFalse, "PortRangeExhausted", msg)
			r.setCondition(b, ConditionReady, metav1.ConditionFalse, "PortRangeExhausted", msg)
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		b.Status.Port = port
	}
	r.setCondition(b, ConditionPortAllocated, metav1.ConditionTrue, "Allocated", fmt.Sprintf("port %d", b.Status.Port))

	if err := r.assignEndpoints(ctx, b, secret); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, render.MatterPVC(b)); client.IgnoreAlreadyExists(err) != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring pvc: %w", err)
	}
	sa, role, rb := render.MatterRBAC(b)
	for _, obj := range []runtime.ApplyConfiguration{sa, role, rb} {
		if err := shared.Apply(ctx, r.Client, obj); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying RBAC: %w", err)
		}
	}
	if err := shared.Apply(ctx, r.Client, render.MatterDeployment(render.MatterInput{
		Bridge: b, Image: r.Image, MQTTURL: r.MQTTURL,
		ConfigHash: render.MatterConfigHash(secret.Data),
	})); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying deployment: %w", err)
	}

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: render.MatterName(b)}, &dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if shared.DeploymentAvailable(&dep) {
		r.setCondition(b, ConditionReady, metav1.ConditionTrue, "Available", "Matter bridge is running")
	} else {
		r.setCondition(b, ConditionReady, metav1.ConditionFalse, "Progressing", "waiting for the Matter bridge pod to become ready")
	}
	return ctrl.Result{}, nil
}

// ensureSecret returns the Bridge's Secret, creating it once with a new setup code, unique ID and
// the test DAC. A Secret that already exists is never regenerated: its setup code is the one
// printed or scanned, and its endpoint backup must survive.
func (r *BridgeReconciler) ensureSecret(ctx context.Context, b *matterv1alpha1.Bridge) (*corev1.Secret, error) {
	var s corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: render.MatterName(b)}, &s)
	if err == nil {
		return &s, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	passcode, discriminator := setup.Random()
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	dac, key, err := attestation.TestDACPEM()
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{
		render.MatterPasscodeKey:      []byte(strconv.FormatUint(uint64(passcode), 10)),
		render.MatterDiscriminatorKey: []byte(strconv.FormatUint(uint64(discriminator), 10)),
		render.MatterUniqueIDKey:      []byte(strings.ToUpper(hex.EncodeToString(id))),
		render.MatterDACKey:           dac,
		render.MatterDACPrivateKey:    key,
	}
	// If the Secret was deleted but status remembers the endpoints, keep them.
	if len(b.Status.Endpoints) > 0 {
		if data[render.MatterEndpointsKey], err = json.Marshal(b.Status.Endpoints); err != nil {
			return nil, err
		}
	}
	created := render.MatterSecret(b, data)
	if err := r.Create(ctx, created); err != nil {
		// AlreadyExists means our cache is stale; requeue and read the real one.
		return nil, fmt.Errorf("creating secret: %w", err)
	}
	logf.FromContext(ctx).Info("Created Secret with a new setup code", "name", created.Name)
	return created, nil
}

// setupOf reads the setup code from the Bridge's Secret, and checks everything else the pod
// reads from it, as the pod does.
func setupOf(s *corev1.Secret) (node.Setup, error) {
	cfg, err := render.MatterSetup(func(key string) ([]byte, error) {
		if v, ok := s.Data[key]; ok {
			return v, nil
		}
		return nil, fmt.Errorf("no %s", key)
	})
	if err != nil {
		return node.Setup{}, &userError{reasonInvalidSecret, fmt.Sprintf("Secret %s: %v", s.Name, err)}
	}
	return cfg.Setup, nil
}

// setPairing shows the codes a commissioner takes.
func setPairing(b *matterv1alpha1.Bridge, s node.Setup) error {
	p := node.PayloadFor(s)
	qr, err := p.QRCode()
	if err != nil {
		return err
	}
	manual, err := p.ManualCode()
	if err != nil {
		return err
	}
	d := int32(s.Discriminator)
	b.Status.Discriminator, b.Status.QRCode, b.Status.ManualPairingCode = &d, qr, manual
	return nil
}

// assignEndpoints gives every selected Device an endpoint ID. Entries are only ever added:
// controllers key rooms, scenes and automations to the endpoint, so a Device that comes back must
// get its old one. The Secret keeps a copy, which restores them if status is lost, and is written
// before status, so it always holds at least what status does.
func (r *BridgeReconciler) assignEndpoints(ctx context.Context, b *matterv1alpha1.Bridge, secret *corev1.Secret) error {
	selector, err := metav1.LabelSelectorAsSelector(&b.Spec.DeviceSelector)
	if err != nil {
		return &userError{"InvalidSelector", fmt.Sprintf("deviceSelector: %v", err)}
	}
	var list homev1alpha1.DeviceList
	if err := r.List(ctx, &list, client.InNamespace(b.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return err
	}
	var backup []matterv1alpha1.Endpoint
	if raw := secret.Data[render.MatterEndpointsKey]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &backup); err != nil {
			return &userError{reasonInvalidSecret, fmt.Sprintf("Secret %s: %s: %v", secret.Name, render.MatterEndpointsKey, err)}
		}
	}
	endpoints, err := mergeEndpoints(b.Status.Endpoints, backup)
	if err != nil {
		return &userError{"EndpointConflict", fmt.Sprintf("Bridge status and Secret %s disagree: %v. Fix or remove %s in the Secret",
			secret.Name, err, render.MatterEndpointsKey)}
	}
	names := make([]string, 0, len(list.Items))
	for _, d := range list.Items {
		names = append(names, d.Name)
	}
	endpoints, err = allocate(endpoints, names)
	if err != nil {
		return &userError{"EndpointsExhausted", err.Error()}
	}

	if !slices.Equal(endpoints, backup) {
		raw, err := json.Marshal(endpoints)
		if err != nil {
			return err
		}
		patch := client.MergeFrom(secret.DeepCopy())
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[render.MatterEndpointsKey] = raw
		if err := r.Patch(ctx, secret, patch); err != nil {
			return fmt.Errorf("backing up endpoints: %w", err)
		}
	}
	b.Status.Endpoints = endpoints
	b.Status.NextEndpoint = matterv1alpha1.FirstEndpoint + int32(len(endpoints))
	b.Status.Devices = int32(len(list.Items))

	var unsupported []string
	for _, d := range list.Items {
		caps := make([]string, 0, len(d.Status.Capabilities))
		for _, c := range d.Status.Capabilities {
			caps = append(caps, c.Name)
		}
		// A Device with no capabilities yet hasn't been resolved, so nothing is known.
		if len(caps) > 0 && !matterbridge.Supported(caps) {
			unsupported = append(unsupported, d.Name)
		}
	}
	slices.Sort(unsupported)
	if len(unsupported) > 0 {
		r.setCondition(b, ConditionDevicesSupported, metav1.ConditionFalse, "Unsupported",
			"no capability maps to a Matter device type: "+strings.Join(unsupported, ", "))
	} else {
		r.setCondition(b, ConditionDevicesSupported, metav1.ConditionTrue, "Supported", "every Device with capabilities maps to Matter")
	}
	return nil
}

// mergeEndpoints returns status's endpoints with any the backup has that status lost, in endpoint
// order. It fails if the two give a Device or an endpoint ID to different things, or the result
// isn't the dense run from FirstEndpoint that allocation makes.
func mergeEndpoints(status, backup []matterv1alpha1.Endpoint) ([]matterv1alpha1.Endpoint, error) {
	byID := map[int32]string{}
	byDevice := map[string]int32{}
	add := func(e matterv1alpha1.Endpoint) error {
		if d, ok := byID[e.Endpoint]; ok && d != e.Device {
			return fmt.Errorf("endpoint %d is %s and %s", e.Endpoint, d, e.Device)
		}
		if id, ok := byDevice[e.Device]; ok && id != e.Endpoint {
			return fmt.Errorf("device %s is endpoint %d and %d", e.Device, id, e.Endpoint)
		}
		byID[e.Endpoint], byDevice[e.Device] = e.Device, e.Endpoint
		return nil
	}
	for _, e := range slices.Concat(status, backup) {
		if err := add(e); err != nil {
			return nil, err
		}
	}
	out := make([]matterv1alpha1.Endpoint, 0, len(byID))
	for id := int32(matterv1alpha1.FirstEndpoint); id < matterv1alpha1.FirstEndpoint+int32(len(byID)); id++ {
		d, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("endpoint %d is missing", id)
		}
		out = append(out, matterv1alpha1.Endpoint{Device: d, Endpoint: id})
	}
	return out, nil
}

// allocate gives each device without an endpoint the next ID, in name order.
func allocate(endpoints []matterv1alpha1.Endpoint, devices []string) ([]matterv1alpha1.Endpoint, error) {
	have := map[string]bool{}
	for _, e := range endpoints {
		have[e.Device] = true
	}
	slices.Sort(devices)
	for _, d := range devices {
		if have[d] {
			continue
		}
		id := matterv1alpha1.FirstEndpoint + int32(len(endpoints))
		if id > matterv1alpha1.MaxEndpoint {
			return endpoints, fmt.Errorf("no endpoint ID left for %s: every ID up to %d has been used", d, matterv1alpha1.MaxEndpoint)
		}
		endpoints = append(endpoints, matterv1alpha1.Endpoint{Device: d, Endpoint: id})
		have[d] = true
	}
	return endpoints, nil
}

func (r *BridgeReconciler) allocatePort(ctx context.Context, b *matterv1alpha1.Bridge) (int32, error) {
	self := client.ObjectKeyFromObject(b)
	return r.ports.Allocate(self, b.Spec.PortRange.Start, b.Spec.PortRange.End, func() (map[int32]bool, error) {
		// Host-networked pods share each node's ports, so ports are unique cluster-wide.
		var all matterv1alpha1.BridgeList
		if err := r.List(ctx, &all); err != nil {
			return nil, err
		}
		used := map[int32]bool{}
		for _, o := range all.Items {
			if client.ObjectKeyFromObject(&o) != self && o.Status.Port != 0 {
				used[o.Status.Port] = true
			}
		}
		return used, nil
	})
}

func (r *BridgeReconciler) release(key types.NamespacedName) { r.ports.Release(key) }

func (r *BridgeReconciler) setCondition(b *matterv1alpha1.Bridge, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: b.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *BridgeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&matterv1alpha1.Bridge{}).
		Owns(&appsv1.Deployment{}).
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(r.bridgesInNamespace)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.bridgeForSecret)).
		Named("matter-bridge").
		Complete(r)
}

// bridgesInNamespace maps a Device to every Bridge in its namespace; selectors decide the rest.
func (r *BridgeReconciler) bridgesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list matterv1alpha1.BridgeList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Bridges")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// bridgeForSecret maps a Bridge's own Secret to the Bridge.
func (r *BridgeReconciler) bridgeForSecret(_ context.Context, obj client.Object) []reconcile.Request {
	owner := obj.GetLabels()[matterv1alpha1.LabelBridge]
	b := &matterv1alpha1.Bridge{ObjectMeta: metav1.ObjectMeta{Name: owner}}
	if owner == "" || obj.GetName() != render.MatterName(b) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: owner}}}
}
