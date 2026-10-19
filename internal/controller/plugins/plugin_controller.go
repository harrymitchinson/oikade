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

package plugins

import (
	"context"
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
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/controller/shared"
	"github.com/harrymitchinson/oikade/internal/render"
)

const ConditionReady = "Ready"

// PluginReconciler reconciles a Plugin object
type PluginReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// MQTTURL is the broker plugins connect to.
	MQTTURL string
}

// +kubebuilder:rbac:groups=plugins.oikade.dev,resources=plugins,verbs=get;list;watch
// +kubebuilder:rbac:groups=plugins.oikade.dev,resources=plugins/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=plugins.oikade.dev,resources=plugins/finalizers,verbs=update
// +kubebuilder:rbac:groups=home.oikade.dev,resources=devices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch

// Reconcile renders a Plugin's declared Devices into its config Secret and runs the plugin.
func (r *PluginReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p pluginsv1alpha1.Plugin
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		// Owned objects are garbage collected; the user's Secret is never ours.
		return ctrl.Result{}, nil
	}

	orig := p.DeepCopy()
	err := r.reconcile(ctx, &p)
	if err == nil {
		// A failed reconcile leaves the conditions describing an older spec.
		p.Status.ObservedGeneration = p.Generation
	}
	if !equality.Semantic.DeepEqual(orig.Status, p.Status) {
		if serr := r.Status().Patch(ctx, &p, client.MergeFrom(orig)); serr != nil && err == nil {
			err = serr
		}
	}
	return ctrl.Result{}, err
}

func (r *PluginReconciler) reconcile(ctx context.Context, p *pluginsv1alpha1.Plugin) error {
	devices, duplicates, err := r.devices(ctx, p)
	if err != nil {
		return err
	}
	p.Status.Devices = int32(len(devices))

	config, err := render.PluginConfig(devices)
	if err != nil {
		return fmt.Errorf("rendering devices.json: %w", err)
	}
	hash := render.Hash(config)

	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Namespace: p.Namespace, Name: name}
	}
	if ref := p.Spec.SecretRef; ref != nil && ref.Name == render.PluginConfigSecretName(p) {
		r.setReady(p, metav1.ConditionFalse, "NameConflict",
			fmt.Sprintf("spec.secretRef names %s, which the operator renders devices.json into", ref.Name))
		return nil
	}
	// Applying with ForceOwnership would silently take over an object someone else owns.
	for _, o := range []struct {
		kind, name string
		obj        client.Object
	}{
		{"Secret", render.PluginConfigSecretName(p), &corev1.Secret{}},
		{"Deployment", render.PluginDeploymentName(p), &appsv1.Deployment{}},
	} {
		err := r.Get(ctx, key(o.name), o.obj)
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		if err == nil && !metav1.IsControlledBy(o.obj, p) {
			r.setReady(p, metav1.ConditionFalse, "NameConflict",
				fmt.Sprintf("%s %s exists and isn't owned by this Plugin", o.kind, o.name))
			return nil
		}
	}

	if err := shared.Apply(ctx, r.Client, render.PluginConfigSecret(p, config)); err != nil {
		return fmt.Errorf("applying config secret: %w", err)
	}

	secretFound, secretVersion := true, ""
	if ref := p.Spec.SecretRef; ref != nil {
		// Only existence and version matter: the operator never reads the user's keys.
		var s corev1.Secret
		err := r.Get(ctx, key(ref.Name), &s)
		switch {
		case apierrors.IsNotFound(err):
			secretFound = false
		case err != nil:
			return fmt.Errorf("getting secret %s: %w", ref.Name, err)
		default:
			secretVersion = s.ResourceVersion
		}
	}

	if err := shared.Apply(ctx, r.Client, render.PluginDeployment(render.PluginInput{
		Plugin: p, MQTTURL: r.MQTTURL, ConfigHash: hash, SecretVersion: secretVersion,
	})); err != nil {
		return fmt.Errorf("applying deployment: %w", err)
	}
	p.Status.ConfigHash = hash

	// The cache may not have the Deployment we just created yet; it then reads as Progressing.
	var dep appsv1.Deployment
	if err := r.Get(ctx, key(render.PluginDeploymentName(p)), &dep); client.IgnoreNotFound(err) != nil {
		return err
	}
	switch {
	case len(duplicates) > 0:
		r.setReady(p, metav1.ConditionFalse, "DuplicateDevice",
			"Device ids claimed by more than one Device (first by name wins): "+strings.Join(duplicates, ", "))
	case !secretFound:
		r.setReady(p, metav1.ConditionFalse, "SecretNotFound",
			fmt.Sprintf("Secret %s named in spec.secretRef doesn't exist", p.Spec.SecretRef.Name))
	case shared.DeploymentAvailable(&dep):
		r.setReady(p, metav1.ConditionTrue, "Available", "The plugin is running")
	default:
		r.setReady(p, metav1.ConditionFalse, "Progressing", "waiting for the plugin pod to become ready")
	}
	return nil
}

// devices returns the Devices declared for this plugin, sorted by id, and any ids claimed by
// more than one Device. The first Device by name wins, as in the hub.
func (r *PluginReconciler) devices(ctx context.Context, p *pluginsv1alpha1.Plugin) ([]contractv1.DeclaredDevice, []string, error) {
	var list homev1alpha1.DeviceList
	if err := r.List(ctx, &list, client.InNamespace(p.Namespace)); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(list.Items, func(a, b homev1alpha1.Device) int { return strings.Compare(a.Name, b.Name) })
	byID := map[string]contractv1.DeclaredDevice{}
	var duplicates []string
	for _, d := range list.Items {
		src := d.Spec.Plugin
		if src == nil || src.PluginRef.Name != p.Name {
			continue
		}
		if _, taken := byID[src.ID]; taken {
			duplicates = append(duplicates, src.ID)
			continue
		}
		dev := contractv1.DeclaredDevice{ID: src.ID, Name: d.Name, DisplayName: d.Title(), Parameters: src.Parameters}
		if d.Spec.RoomRef != nil {
			dev.Room = d.Spec.RoomRef.Name
		}
		byID[src.ID] = dev
	}
	out := make([]contractv1.DeclaredDevice, 0, len(byID))
	for _, d := range byID {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b contractv1.DeclaredDevice) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(duplicates)
	return out, slices.Compact(duplicates), nil
}

func (r *PluginReconciler) setReady(p *pluginsv1alpha1.Plugin, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: status, Reason: reason, Message: msg, ObservedGeneration: p.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *PluginReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pluginsv1alpha1.Plugin{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(pluginForDevice)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.pluginsForSecret)).
		Named("plugins-plugin").
		Complete(r)
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

// pluginsForSecret maps a user Secret to the Plugins whose secretRef names it, so SecretNotFound
// clears when it's created.
func (r *PluginReconciler) pluginsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	var list pluginsv1alpha1.PluginList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Could not list Plugins for Secret", "secret", client.ObjectKeyFromObject(obj))
		return nil
	}
	var out []reconcile.Request
	for _, p := range list.Items {
		if p.Spec.SecretRef != nil && p.Spec.SecretRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
		}
	}
	return out
}
