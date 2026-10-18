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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
// +kubebuilder:rbac:groups=home.oikade.dev,resources=rooms,verbs=get;list;watch

// Reconcile sets the Ready condition from compiling the Automation and checking that the Rooms it
// names exist. A spec that doesn't compile is reported first.
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
	} else {
		missing, err := r.missingRooms(ctx, &a)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(missing) > 0 {
			cond.Status, cond.Reason = metav1.ConditionFalse, "RoomNotFound"
			cond.Message = "Room(s) not found: " + strings.Join(missing, ", ")
		}
	}
	meta.SetStatusCondition(&a.Status.Conditions, cond)
	a.Status.ObservedGeneration = a.Generation
	if equality.Semantic.DeepEqual(orig.Status, a.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Patch(ctx, &a, client.MergeFrom(orig))
}

// missingRooms returns the Rooms named by the Automation's triggers and set actions that don't
// exist in its namespace, sorted.
func (r *AutomationReconciler) missingRooms(ctx context.Context, a *homev1alpha1.Automation) ([]string, error) {
	var named []string
	for _, t := range a.Spec.Triggers {
		if t.Room != "" {
			named = append(named, t.Room)
		}
	}
	for _, act := range a.Spec.Actions {
		if act.Set != nil && act.Set.Room != "" {
			named = append(named, act.Set.Room)
		}
	}
	if len(named) == 0 {
		return nil, nil
	}
	var rooms homev1alpha1.RoomList
	if err := r.List(ctx, &rooms, client.InNamespace(a.Namespace)); err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range named {
		if !slices.ContainsFunc(rooms.Items, func(room homev1alpha1.Room) bool { return room.Name == name }) {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AutomationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&homev1alpha1.Automation{}).
		Watches(&homev1alpha1.Room{}, handler.EnqueueRequestsFromMapFunc(r.automationsInNamespace)).
		Named("home-automation").
		Complete(r)
}

// automationsInNamespace maps a Room to every Automation in its namespace, so creating or deleting
// a Room updates RoomNotFound without the Automation being edited. Automations are few, so this is
// cheaper than an index.
func (r *AutomationReconciler) automationsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list homev1alpha1.AutomationList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Automations")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}
