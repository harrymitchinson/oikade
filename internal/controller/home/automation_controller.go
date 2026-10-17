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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
	"github.com/harry/oikade/internal/engine"
)

// AutomationReconciler validates Automations. The runtime compiles them again to run them.
type AutomationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=home.oikade.dev,resources=automations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=home.oikade.dev,resources=automations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=home.oikade.dev,resources=automations/finalizers,verbs=update

// Reconcile sets the Ready condition from compiling the Automation.
func (r *AutomationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var a homev1alpha1.Automation
	if err := r.Get(ctx, req.NamespacedName, &a); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := a.DeepCopy()
	cond := metav1.Condition{
		Type: homev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Valid", ObservedGeneration: a.Generation,
	}
	if _, err := engine.Compile(&a); err != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Invalid", err.Error()
	}
	meta.SetStatusCondition(&a.Status.Conditions, cond)
	a.Status.ObservedGeneration = a.Generation
	if equality.Semantic.DeepEqual(orig.Status, a.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Patch(ctx, &a, client.MergeFrom(orig))
}

// SetupWithManager sets up the controller with the Manager.
func (r *AutomationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&homev1alpha1.Automation{}).
		Named("home-automation").
		Complete(r)
}
