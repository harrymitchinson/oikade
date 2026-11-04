package home

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/controller/shared"
)

// Event reasons for oikade.dev/replace-with.
const (
	reasonReplaced       = "Replaced"
	reasonReplaceRefused = "ReplaceRefused"
)

// replaceAll acts on each oikade.dev/replace-with annotation on this instance's Devices. It
// reports whether it changed any Device's spec or deleted one, after which the caller's view of
// the Devices is stale.
func (r *AdoptReconciler) replaceAll(ctx context.Context, inst client.Object, devices []homev1alpha1.Device) (bool, error) {
	byName := map[string]*homev1alpha1.Device{}
	for i := range devices {
		byName[devices[i].Name] = &devices[i]
	}
	for i := range devices {
		d := &devices[i]
		if _, ok := r.idOf(d, inst.GetName()); !ok {
			continue
		}
		target, ok := d.Annotations[homev1alpha1.AnnotationReplaceWith]
		if !ok {
			if err := r.clearRefusal(ctx, d); err != nil {
				return false, err
			}
			continue
		}
		done, err := r.replace(ctx, inst, d, byName[strings.TrimSpace(target)], strings.TrimSpace(target))
		if err != nil || done {
			return done, err
		}
	}
	return false, nil
}

// replace moves target's ID onto d and deletes target, or refuses with Replaced=False. It moves
// the ID first: if the delete then fails, the next pass finds two adopted copies of one device
// and keeps the older, d.
func (r *AdoptReconciler) replace(ctx context.Context, inst client.Object, d, target *homev1alpha1.Device, name string) (bool, error) {
	adopted := func(o *homev1alpha1.Device) bool { return metav1.IsControlledBy(o, inst) && !takenOver(o) }
	kind := map[string]string{AdoptPlugin: "Plugin", AdoptZigbee2MQTT: "Zigbee2MQTT"}[r.Driver]
	switch {
	case !adopted(d):
		return false, r.refuse(ctx, d, "NotAdopted",
			"Only an adopted Device takes over another's ID; change the ID in this Device's spec instead")
	case name == "" || name == d.Name:
		return false, r.refuse(ctx, d, "InvalidTarget", fmt.Sprintf("%s must name another Device", homev1alpha1.AnnotationReplaceWith))
	case target == nil:
		return false, r.refuse(ctx, d, "TargetNotFound", fmt.Sprintf("Device %s does not exist", name))
	}
	id, ok := r.idOf(target, inst.GetName())
	switch {
	case !ok:
		return false, r.refuse(ctx, d, "OtherInstance", fmt.Sprintf("Device %s is not a device of %s %s", name, kind, inst.GetName()))
	case !adopted(target):
		return false, r.refuse(ctx, d, "TargetNotAdopted", fmt.Sprintf("Device %s is declared, not adopted from %s %s", name, kind, inst.GetName()))
	}

	orig := d.DeepCopy()
	old := r.setID(d, id)
	delete(d.Annotations, homev1alpha1.AnnotationReplaceWith)
	if err := r.Patch(ctx, d, client.MergeFrom(orig), client.FieldOwner(shared.FieldOwner)); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	orig = d.DeepCopy()
	meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
		Type: homev1alpha1.ConditionReplaced, Status: metav1.ConditionTrue, Reason: "Replaced",
		Message:            fmt.Sprintf("Took over ID %s from Device %s, in place of %s", id, name, old),
		ObservedGeneration: d.Generation,
	})
	if err := r.patchStatus(ctx, d, orig); err != nil {
		return true, err
	}
	if err := r.Delete(ctx, target, client.Preconditions{UID: &target.UID}); client.IgnoreNotFound(err) != nil {
		return true, err
	}
	logf.FromContext(ctx).Info("Replaced Device ID", "device", d.Name, "id", id, "oldID", old, "deleted", name)
	r.event(d, target, corev1.EventTypeNormal, reasonReplaced, "Replace",
		"Took over ID %s from Device %s, which was deleted", id, name)
	return true, nil
}

// setID sets the driver ID a Device declares, and returns the one it replaces.
func (r *AdoptReconciler) setID(d *homev1alpha1.Device, id string) string {
	if r.Driver == AdoptZigbee2MQTT {
		old := d.Spec.Zigbee2MQTT.IEEEAddress
		d.Spec.Zigbee2MQTT.IEEEAddress = id
		return old
	}
	old := d.Spec.Plugin.ID
	d.Spec.Plugin.ID = id
	return old
}

// refuse sets Replaced=False, with an event the first time for each reason.
func (r *AdoptReconciler) refuse(ctx context.Context, d *homev1alpha1.Device, reason, message string) error {
	prev := meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionReplaced)
	if prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == reason && prev.Message == message {
		return nil
	}
	orig := d.DeepCopy()
	meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
		Type: homev1alpha1.ConditionReplaced, Status: metav1.ConditionFalse, Reason: reason,
		Message: message, ObservedGeneration: d.Generation,
	})
	if err := r.patchStatus(ctx, d, orig); err != nil {
		return err
	}
	r.event(d, nil, corev1.EventTypeWarning, reasonReplaceRefused, "Replace", "Refused %s: %s", homev1alpha1.AnnotationReplaceWith, message)
	return nil
}

// clearRefusal drops Replaced=False once its annotation is gone. Replaced=True stays as a record.
func (r *AdoptReconciler) clearRefusal(ctx context.Context, d *homev1alpha1.Device) error {
	c := meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionReplaced)
	if c == nil || c.Status != metav1.ConditionFalse {
		return nil
	}
	orig := d.DeepCopy()
	meta.RemoveStatusCondition(&d.Status.Conditions, homev1alpha1.ConditionReplaced)
	return r.patchStatus(ctx, d, orig)
}
