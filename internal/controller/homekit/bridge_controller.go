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

package homekit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
	homekitv1alpha1 "github.com/harry/oikade/api/homekit/v1alpha1"
	"github.com/harry/oikade/internal/identity"
	"github.com/harry/oikade/internal/ports"
	"github.com/harry/oikade/internal/render"
)

const (
	fieldOwner = "oikade"

	ConditionReady         = "Ready"
	ConditionPortAllocated = "PortAllocated"
)

// BridgeReconciler reconciles a Bridge object
type BridgeReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Image runs the HAP server (the operator's own image).
	Image   string
	MQTTURL string

	// reserved remembers ports handed out before the Bridge's status write is visible in the
	// cache, so two Bridges reconciled back to back can't get the same port.
	mu       sync.Mutex
	reserved map[types.NamespacedName]int32
}

// +kubebuilder:rbac:groups=homekit.oikade.dev,resources=bridges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=homekit.oikade.dev,resources=bridges/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=homekit.oikade.dev,resources=bridges/finalizers,verbs=update
// The operator must hold every permission it grants to HAP Roles.
// +kubebuilder:rbac:groups=home.oikade.dev,resources=devices,verbs=get;list;watch
// +kubebuilder:rbac:groups=zigbee.oikade.dev,resources=zigbee2mqtts,verbs=get;list;watch
// +kubebuilder:rbac:groups=plugins.oikade.dev,resources=plugins,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch

// Reconcile drives a Bridge's identity, port, accessory IDs and HAP Deployment.
func (r *BridgeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b homekitv1alpha1.Bridge
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		if apierrors.IsNotFound(err) {
			r.release(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() {
		// Owned objects are garbage collected; the identity Secret and PVC are retained on purpose.
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

func (r *BridgeReconciler) reconcile(ctx context.Context, b *homekitv1alpha1.Bridge) (ctrl.Result, error) {
	pin, err := readPin(ctx, r.Client, b)
	if err != nil {
		if invalid, ok := errors.AsType[*invalidPinError](err); ok {
			r.setCondition(b, ConditionReady, metav1.ConditionFalse, "InvalidPin", invalid.Error())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	username, err := r.ensureIdentity(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}
	b.Status.Username = username

	if b.Status.Port == 0 {
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

	if err := r.assignAIDs(ctx, b); err != nil {
		if invalid, ok := errors.AsType[*invalidSelectorError](err); ok {
			r.setCondition(b, ConditionReady, metav1.ConditionFalse, "InvalidSelector", invalid.Error())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, render.HAPPVC(b)); client.IgnoreAlreadyExists(err) != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring pvc: %w", err)
	}
	sa, role, rb := render.HAPRBAC(b)
	for _, obj := range []runtime.ApplyConfiguration{sa, role, rb} {
		if err := r.apply(ctx, obj); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying HAP RBAC: %w", err)
		}
	}
	if err := r.apply(ctx, render.HAPDeployment(render.HAPInput{
		Bridge: b, Image: r.Image, MQTTURL: r.MQTTURL,
		ConfigHash: render.HAPConfigHash(b.Status.Port, username, pin),
	})); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying deployment: %w", err)
	}

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Name + "-hap"}, &dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if deploymentAvailable(&dep) {
		r.setCondition(b, ConditionReady, metav1.ConditionTrue, "Available", "HAP server is running")
	} else {
		r.setCondition(b, ConditionReady, metav1.ConditionFalse, "Progressing", "waiting for the HAP pod to become ready")
	}
	return ctrl.Result{}, nil
}

type invalidSelectorError struct{ msg string }

func (e *invalidSelectorError) Error() string { return e.msg }

// assignAIDs gives every selected Device an accessory ID. Entries are only ever added: the Home app
// keys rooms, scenes and favourites to the AID, so a Device that comes back must get its old one.
func (r *BridgeReconciler) assignAIDs(ctx context.Context, b *homekitv1alpha1.Bridge) error {
	selector, err := metav1.LabelSelectorAsSelector(&b.Spec.DeviceSelector)
	if err != nil {
		return &invalidSelectorError{fmt.Sprintf("deviceSelector: %v", err)}
	}
	var list homev1alpha1.DeviceList
	if err := r.List(ctx, &list, client.InNamespace(b.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return err
	}
	slices.SortFunc(list.Items, func(a, c homev1alpha1.Device) int { return strings.Compare(a.Name, c.Name) })

	next := b.Status.NextAID
	for _, a := range b.Status.Accessories {
		next = max(next, a.AID+1)
	}
	next = max(next, homekitv1alpha1.FirstAID)
	for _, d := range list.Items {
		if b.Status.AID(d.Name) == 0 {
			b.Status.Accessories = append(b.Status.Accessories, homekitv1alpha1.Accessory{Device: d.Name, AID: next})
			next++
		}
	}
	b.Status.NextAID = next
	b.Status.Devices = int32(len(list.Items))
	return nil
}

type invalidPinError struct{ msg string }

func (e *invalidPinError) Error() string { return e.msg }

// readPin returns the Bridge's setup code. User-fixable problems are returned as *invalidPinError.
func readPin(ctx context.Context, c client.Client, b *homekitv1alpha1.Bridge) (string, error) {
	ref := b.Spec.PinSecretRef
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: ref.Name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &invalidPinError{fmt.Sprintf("setup code Secret %q not found", ref.Name)}
		}
		return "", err
	}
	pin, ok := s.Data[ref.Key]
	if !ok {
		return "", &invalidPinError{fmt.Sprintf("setup code Secret %q has no key %q", ref.Name, ref.Key)}
	}
	if err := identity.ValidatePin(string(pin)); err != nil {
		return "", &invalidPinError{err.Error()}
	}
	return string(pin), nil
}

// ensureIdentity returns the Bridge's username, creating the identity Secret once.
// An existing username is never replaced: changing it would unpair the bridge.
func (r *BridgeReconciler) ensureIdentity(ctx context.Context, b *homekitv1alpha1.Bridge) (string, error) {
	var s corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: render.IdentitySecretName(b)}, &s)
	if err == nil {
		u := string(s.Data[render.IdentityUsernameKey])
		if !identity.ValidUsername(u) {
			return "", fmt.Errorf("identity secret %s has invalid username %q; fix it by hand", s.Name, u)
		}
		return u, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	// If the Secret was deleted but status remembers the username, restore it rather than re-pair.
	u := b.Status.Username
	if !identity.ValidUsername(u) {
		if u, err = identity.NewUsername(); err != nil {
			return "", err
		}
	}
	if err := r.Create(ctx, render.IdentitySecret(b, u)); err != nil {
		// AlreadyExists means our cache is stale; requeue and read the real one.
		return "", fmt.Errorf("creating identity secret: %w", err)
	}
	return u, nil
}

func (r *BridgeReconciler) allocatePort(ctx context.Context, b *homekitv1alpha1.Bridge) (int32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	self := client.ObjectKeyFromObject(b)
	if port, ok := r.reserved[self]; ok && b.Spec.PortRange.Contains(port) {
		return port, nil
	}

	// Host-networked pods share each node's ports, so ports are unique cluster-wide.
	var all homekitv1alpha1.BridgeList
	if err := r.List(ctx, &all); err != nil {
		return 0, err
	}
	used := map[int32]bool{}
	for _, o := range all.Items {
		if client.ObjectKeyFromObject(&o) != self && o.Status.Port != 0 {
			used[o.Status.Port] = true
		}
	}
	for k, port := range r.reserved {
		if k != self {
			used[port] = true
		}
	}

	port, err := ports.Allocate(b.Spec.PortRange, used)
	if err != nil {
		return 0, err
	}
	if r.reserved == nil {
		r.reserved = map[types.NamespacedName]int32{}
	}
	r.reserved[self] = port
	return port, nil
}

func (r *BridgeReconciler) release(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reserved, key)
}

func (r *BridgeReconciler) apply(ctx context.Context, obj runtime.ApplyConfiguration) error {
	return r.Apply(ctx, obj, client.FieldOwner(fieldOwner), client.ForceOwnership)
}

func (r *BridgeReconciler) setCondition(b *homekitv1alpha1.Bridge, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: b.Generation,
	})
}

func deploymentAvailable(d *appsv1.Deployment) bool {
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas >= 1 &&
		d.Status.AvailableReplicas >= 1
}

// SetupWithManager sets up the controller with the Manager.
func (r *BridgeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&homekitv1alpha1.Bridge{}).
		Owns(&appsv1.Deployment{}).
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(r.bridgesInNamespace)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.bridgesForSecret)).
		Named("homekit-bridge").
		Complete(r)
}

// bridgesInNamespace maps a Device to every Bridge in its namespace; selectors decide the rest.
func (r *BridgeReconciler) bridgesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.bridgeRequests(ctx, obj.GetNamespace(), func(*homekitv1alpha1.Bridge) bool { return true })
}

// bridgesForSecret maps a Secret to Bridges that use it as their setup code or identity.
func (r *BridgeReconciler) bridgesForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	owner := obj.GetLabels()[homekitv1alpha1.LabelBridge]
	return r.bridgeRequests(ctx, obj.GetNamespace(), func(b *homekitv1alpha1.Bridge) bool {
		return b.Name == owner || b.Spec.PinSecretRef.Name == obj.GetName()
	})
}

func (r *BridgeReconciler) bridgeRequests(ctx context.Context, ns string, match func(*homekitv1alpha1.Bridge) bool) []reconcile.Request {
	var list homekitv1alpha1.BridgeList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Bridges")
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if match(&list.Items[i]) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}
