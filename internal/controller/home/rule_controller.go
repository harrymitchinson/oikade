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

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/engine"
)

// RuleReconciler validates Rules. The runtime compiles them again to run them.
type RuleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=home.oikade.dev,resources=rules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=home.oikade.dev,resources=rules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=home.oikade.dev,resources=rules/finalizers,verbs=update
// +kubebuilder:rbac:groups=home.oikade.dev,resources=rooms,verbs=get;list;watch

// Reconcile sets the Ready condition from compiling the Rule and checking that the Rooms it
// names exist. A spec that doesn't compile is reported first.
func (r *RuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var a homev1alpha1.Rule
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
	// Optimistic locking, because the runtime writes the Scheduled condition into the same list.
	return ctrl.Result{}, r.Status().Patch(ctx, &a, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// missingRooms returns the Rooms named by the Rule's triggers and set actions that don't
// exist in its namespace, sorted.
func (r *RuleReconciler) missingRooms(ctx context.Context, a *homev1alpha1.Rule) ([]string, error) {
	var named []string
	for _, t := range a.Spec.Triggers {
		if t.Change != nil && t.Change.Room != "" {
			named = append(named, t.Change.Room)
		}
	}
	for _, act := range a.Spec.Do {
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
func (r *RuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&homev1alpha1.Rule{}).
		Watches(&homev1alpha1.Room{}, handler.EnqueueRequestsFromMapFunc(r.rulesInNamespace)).
		Named("home-rule").
		Complete(r)
}

// rulesInNamespace maps a Room to every Rule in its namespace, so creating or deleting
// a Room updates RoomNotFound without the Rule being edited. Rules are few, so this is
// cheaper than an index.
func (r *RuleReconciler) rulesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list homev1alpha1.RuleList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Rules")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}
