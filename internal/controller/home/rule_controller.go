package home

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/schedule"
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
// +kubebuilder:rbac:groups=home.oikade.dev,resources=devices,verbs=get;list;watch

// Reasons for Ready=False, after Invalid and RoomNotFound.
const (
	// ReasonUnendedClaim: a set on an arbitrated capability of a device has no for or until.
	ReasonUnendedClaim = "UnendedClaim"
	// ReasonConflictingHolds: two holds keep one capability that has no control entry.
	ReasonConflictingHolds = "ConflictingHolds"
	// ReasonInvalidRelease: a release names a Rule that doesn't exist or can't be released.
	ReasonInvalidRelease = "InvalidRelease"
	// ReasonNoControl: a budget's target Device doesn't exist, or has no control entry for the
	// capability, which the budget's constraints come from.
	ReasonNoControl = "NoControl"
	// ReasonInvalidBudget: a budget doesn't fit its target's control entry, or is malformed in a
	// way the CRD can't check, such as bands out of order.
	ReasonInvalidBudget = "InvalidBudget"
)

// Reconcile sets the Ready condition from compiling the Rule, then checking it against the
// Rooms, Devices and other Rules in its namespace. The first problem found is reported, in the
// order of the reasons above.
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
		} else {
			reason, msg, err := r.checkClaims(ctx, &a)
			if err != nil {
				return ctrl.Result{}, err
			}
			if reason == "" {
				if reason, msg, err = r.checkBudget(ctx, &a); err != nil {
					return ctrl.Result{}, err
				}
			}
			if reason != "" {
				cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, reason, msg
			}
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
	if a.Spec.Hold != nil && a.Spec.Hold.Room != "" {
		named = append(named, a.Spec.Hold.Room)
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

// checkClaims checks the Rule against arbitration (OIK-64): a set on an arbitrated capability of a
// device must end, two holds mustn't keep one capability that isn't arbitrated, and a release
// must name a Rule that can be released. It returns an empty reason when all is well.
func (r *RuleReconciler) checkClaims(ctx context.Context, a *homev1alpha1.Rule) (string, string, error) {
	var devices homev1alpha1.DeviceList
	if err := r.List(ctx, &devices, client.InNamespace(a.Namespace)); err != nil {
		return "", "", err
	}
	byName := map[string]*homev1alpha1.Device{}
	for i := range devices.Items {
		byName[devices.Items[i].Name] = &devices.Items[i]
	}

	var unended []string
	for i, act := range a.Spec.Do {
		s := act.Set
		if s == nil || s.Device == "" || s.For != nil || s.Until != "" {
			continue
		}
		if d, ok := byName[s.Device]; ok && arbitrated(d, s.Capability) {
			unended = append(unended, fmt.Sprintf("do[%d] sets %s on %s", i, s.Capability, s.Device))
		}
	}
	if len(unended) > 0 {
		return ReasonUnendedClaim, strings.Join(unended, "; ") +
			", which is arbitrated, so the set needs for or until", nil
	}

	var rules homev1alpha1.RuleList
	needRules := a.Spec.Hold != nil || slices.ContainsFunc(a.Spec.Do, func(act homev1alpha1.Action) bool { return act.Release != nil })
	if needRules {
		if err := r.List(ctx, &rules, client.InNamespace(a.Namespace)); err != nil {
			return "", "", err
		}
	}

	if a.Spec.Hold != nil {
		mine := heldTargets(a.Spec.Hold, devices.Items)
		var conflicts []string
		for i := range rules.Items {
			other := &rules.Items[i]
			if other.Name == a.Name || other.Spec.Hold == nil {
				continue
			}
			for _, t := range heldTargets(other.Spec.Hold, devices.Items) {
				if !slices.Contains(mine, t) {
					continue
				}
				// A Device that doesn't exist yet isn't held by anything, so it can't conflict.
				if d, ok := byName[t.device]; !ok || arbitrated(d, t.capability) {
					continue
				}
				conflicts = append(conflicts, fmt.Sprintf("%s on %s with Rule %s", t.capability, t.device, other.Name))
			}
		}
		if len(conflicts) > 0 {
			slices.Sort(conflicts)
			return ReasonConflictingHolds, "Holds " + strings.Join(slices.Compact(conflicts), "; ") +
				", which has no control entry to arbitrate them", nil
		}
	}

	var bad []string
	for _, act := range a.Spec.Do {
		if act.Release == nil {
			continue
		}
		i := slices.IndexFunc(rules.Items, func(o homev1alpha1.Rule) bool { return o.Name == act.Release.Rule })
		switch {
		case i < 0:
			bad = append(bad, fmt.Sprintf("Rule %s doesn't exist", act.Release.Rule))
		case !releasable(&rules.Items[i]):
			bad = append(bad, fmt.Sprintf("Rule %s has no latched hold or until: Released set", act.Release.Rule))
		}
	}
	if len(bad) > 0 {
		return ReasonInvalidRelease, "Can't release: " + strings.Join(bad, "; "), nil
	}
	return "", "", nil
}

// checkBudget compiles a budget hold against its target's control entry, which must exist. It
// returns an empty reason when all is well, or when the Rule has no budget.
func (r *RuleReconciler) checkBudget(ctx context.Context, a *homev1alpha1.Rule) (string, string, error) {
	if a.Spec.While == nil || a.Spec.While.Budget == nil {
		return "", "", nil
	}
	h := a.Spec.Hold
	var d homev1alpha1.Device
	if err := r.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: h.Device}, &d); err != nil {
		if apierrors.IsNotFound(err) {
			return ReasonNoControl, fmt.Sprintf("Device %s not found", h.Device), nil
		}
		return "", "", err
	}
	i := slices.IndexFunc(d.Spec.Control, func(c homev1alpha1.CapabilityControl) bool { return c.Capability == h.Capability })
	if i < 0 {
		return ReasonNoControl, fmt.Sprintf("Device %s has no control entry for %s, which a budget needs", h.Device, h.Capability), nil
	}
	entry := control.FromAPI(d.Spec.Control[i])
	if _, err := schedule.Compile(a, &entry); err != nil {
		return ReasonInvalidBudget, err.Error(), nil
	}
	return "", "", nil
}

// target is one capability of one device.
type target struct{ device, capability string }

// heldTargets returns what a hold keeps: its device, or the Devices in its Room that report the
// capability, as the runtime would.
func heldTargets(h *homev1alpha1.Hold, devices []homev1alpha1.Device) []target {
	if h.Device != "" {
		return []target{{h.Device, h.Capability}}
	}
	var out []target
	for _, d := range devices {
		if d.Spec.RoomRef == nil || d.Spec.RoomRef.Name != h.Room {
			continue
		}
		if slices.ContainsFunc(d.Status.Capabilities, func(c homev1alpha1.DeviceCapability) bool { return c.Name == h.Capability }) {
			out = append(out, target{d.Name, h.Capability})
		}
	}
	return out
}

// arbitrated reports whether the Device has a control entry for the capability.
func arbitrated(d *homev1alpha1.Device, capability string) bool {
	return slices.ContainsFunc(d.Spec.Control, func(c homev1alpha1.CapabilityControl) bool { return c.Capability == capability })
}

// releasable reports whether a release action can name the Rule: it has a latched hold, or a
// set that lasts until released.
func releasable(a *homev1alpha1.Rule) bool {
	if a.Spec.While != nil && a.Spec.While.Latch {
		return true
	}
	return slices.ContainsFunc(a.Spec.Do, func(act homev1alpha1.Action) bool {
		return act.Set != nil && act.Set.Until == homev1alpha1.ClaimUntilReleased
	})
}

// SetupWithManager sets up the controller with the Manager. Rooms, Devices and other Rules feed
// the checks, so a change to any of them re-checks every Rule in the namespace.
func (r *RuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&homev1alpha1.Rule{}).
		Watches(&homev1alpha1.Room{}, handler.EnqueueRequestsFromMapFunc(r.rulesInNamespace)).
		// A Device's control entries are spec, and Room members are held by what they report.
		Watches(&homev1alpha1.Device{}, handler.EnqueueRequestsFromMapFunc(r.rulesInNamespace),
			builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, capabilitiesChanged))).
		// Status writes by the runtime don't change what other Rules check against.
		Watches(&homev1alpha1.Rule{}, handler.EnqueueRequestsFromMapFunc(r.rulesInNamespace),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("home-rule").
		Complete(r)
}

// capabilitiesChanged passes Device updates that change the capabilities it reports.
var capabilitiesChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		old, okOld := e.ObjectOld.(*homev1alpha1.Device)
		cur, okNew := e.ObjectNew.(*homev1alpha1.Device)
		return okOld && okNew && !equality.Semantic.DeepEqual(old.Status.Capabilities, cur.Status.Capabilities)
	},
}

// rulesInNamespace maps a Room, Device or Rule to every Rule in its namespace, so a change to
// one updates the others' Ready without them being edited. Rules are few, so this is cheaper
// than an index.
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
