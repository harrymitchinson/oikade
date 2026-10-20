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

package home

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/engine"
)

// DeviceReconciler validates what the API server can't check about a Device: that each control
// entry fits its capability. The runtime writes the rest of Device status.
type DeviceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=home.oikade.dev,resources=devices,verbs=get;list;watch
// +kubebuilder:rbac:groups=home.oikade.dev,resources=devices/status,verbs=get;update;patch

// Reconcile sets the Ready condition from compiling the Device's control entries.
func (r *DeviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var d homev1alpha1.Device
	if err := r.Get(ctx, req.NamespacedName, &d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := d.DeepCopy()
	cond := metav1.Condition{
		Type: homev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Valid", ObservedGeneration: d.Generation,
	}
	var errs []error
	for _, c := range d.Spec.Control {
		// The day boundary only matters to the runtime, so UTC does for validating.
		if _, err := engine.ControlSettings(c, nil); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "InvalidControl", err.Error()
	}
	meta.SetStatusCondition(&d.Status.Conditions, cond)
	if equality.Semantic.DeepEqual(orig.Status, d.Status) {
		return ctrl.Result{}, nil
	}
	// Optimistic locking, because the runtime writes Resolved and Available into the same list.
	// It writes often, so a conflict is expected and retried quietly.
	err := r.Status().Patch(ctx, &d, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager. Only spec changes matter, so the
// runtime's status writes don't wake it.
func (r *DeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&homev1alpha1.Device{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("home-device").
		Complete(r)
}
