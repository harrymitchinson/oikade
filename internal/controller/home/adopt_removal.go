package home

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

// Event reasons for the removal of adopted Devices.
const (
	reasonRemovalPending = "RemovalPending"
	reasonRemoved        = "Removed"
)

// removal applies the instance's removal policy to one adopted Device. gone is true when its
// device is missing from the instance's reported list of devices in its app: an offline device
// stays in that list, so it's never gone. Under Delete, a gone Device gets RemovalPending and is
// deleted once it has been gone for the grace period. It returns how long until the Device is
// due for deletion, or 0 when nothing is pending.
func (r *AdoptReconciler) removal(ctx context.Context, inst client.Object, d *homev1alpha1.Device,
	discovery *homev1alpha1.DiscoverySpec, gone bool,
) (time.Duration, error) {
	pending := meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionRemovalPending)
	if !gone || discovery.RemovalPolicy != homev1alpha1.RemovalPolicyDelete || d.RetainedOnRemoval() {
		// Retain needs nothing here: the runtime already reports the Device Resolved=False.
		return 0, r.clearRemoval(ctx, d)
	}
	grace := homev1alpha1.DefaultRemovalGracePeriod
	if discovery.RemovalGracePeriod != nil {
		grace = discovery.RemovalGracePeriod.Duration
	}
	now := r.now()
	if pending == nil || pending.Status != metav1.ConditionTrue {
		orig := d.DeepCopy()
		meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
			Type:               homev1alpha1.ConditionRemovalPending,
			Status:             metav1.ConditionTrue,
			Reason:             "RemovedFromApp",
			Message:            fmt.Sprintf("The device was removed from %s's app; this Device is deleted after %s unless it returns", inst.GetName(), grace),
			ObservedGeneration: d.Generation,
			LastTransitionTime: metav1.NewTime(now),
		})
		if err := r.patchStatus(ctx, d, orig); err != nil {
			return 0, err
		}
		r.event(d, nil, corev1.EventTypeWarning, reasonRemovalPending, "Delete",
			"Device removed from %s's app; deleting this Device after %s unless it returns", inst.GetName(), grace)
		return grace, nil
	}
	if left := pending.LastTransitionTime.Add(grace).Sub(now); left > 0 {
		return left, nil
	}
	// The resourceVersion precondition keeps a Device that changed meanwhile, such as one just
	// taken over in Git; the next pass looks at it again.
	err := r.Delete(ctx, d, client.Preconditions{UID: &d.UID, ResourceVersion: &d.ResourceVersion})
	switch {
	case apierrors.IsConflict(err):
		return time.Second, nil
	case client.IgnoreNotFound(err) != nil:
		return 0, err
	}
	logf.FromContext(ctx).Info("Deleted adopted Device, its device was removed from its app", "device", d.Name, "gracePeriod", grace)
	r.event(inst, d, corev1.EventTypeNormal, reasonRemoved, "Delete",
		"Deleted Device %s, its device was removed from the app %s ago", d.Name, grace)
	return 0, nil
}

// clearRemoval drops a RemovalPending condition, once the device is back or the Device is kept.
func (r *AdoptReconciler) clearRemoval(ctx context.Context, d *homev1alpha1.Device) error {
	if meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionRemovalPending) == nil {
		return nil
	}
	orig := d.DeepCopy()
	meta.RemoveStatusCondition(&d.Status.Conditions, homev1alpha1.ConditionRemovalPending)
	return r.patchStatus(ctx, d, orig)
}

// patchStatus patches a Device's status with optimistic locking, because the runtime and the
// Device controller write into the same conditions list.
func (r *AdoptReconciler) patchStatus(ctx context.Context, d, orig *homev1alpha1.Device) error {
	return client.IgnoreNotFound(r.Status().Patch(ctx, d, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})))
}

func (r *AdoptReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock.Now()
}

func (r *AdoptReconciler) event(regarding, related client.Object, eventtype, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	if related == nil {
		r.Recorder.Eventf(regarding, nil, eventtype, reason, action, note, args...)
		return
	}
	r.Recorder.Eventf(regarding, related, eventtype, reason, action, note, args...)
}
