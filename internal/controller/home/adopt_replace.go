package home

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
// reports whether it deleted a Device or changed one's spec, after which the caller's list of
// Devices is stale.
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
		value, ok := d.Annotations[homev1alpha1.AnnotationReplaceWith]
		if !ok {
			if err := r.dropCondition(ctx, d, homev1alpha1.ConditionReplaced, metav1.ConditionFalse); err != nil {
				return false, err
			}
			continue
		}
		if done, err := r.replace(ctx, inst, d, value, byName); err != nil || done {
			return done, err
		}
	}
	return false, nil
}

// replace moves the target's ID onto d and deletes the target, or refuses with Replaced=False.
// The target goes first, so two Devices never declare one ID, which the duplicate rule in manage
// would settle by age, not by what was asked. If moving the ID then fails, the annotation stays:
// the next pass adopts the ID again under a new Device, and this replaces it.
func (r *AdoptReconciler) replace(ctx context.Context, inst client.Object, d *homev1alpha1.Device,
	value string, byName map[string]*homev1alpha1.Device,
) (bool, error) {
	name := strings.TrimSpace(value)
	target := byName[name]
	adopted := func(o *homev1alpha1.Device) bool { return metav1.IsControlledBy(o, inst) && !takenOver(o) }
	kind := "Plugin"
	if r.Driver == AdoptZigbee2MQTT {
		kind = "Zigbee2MQTT"
	}
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

	// The preconditions keep a target that changed meanwhile, such as one just taken over in Git.
	if err := r.Delete(ctx, target, client.Preconditions{UID: &target.UID, ResourceVersion: &target.ResourceVersion}); client.IgnoreNotFound(err) != nil {
		return false, err
	}
	old, err := r.moveID(ctx, d, value, id)
	if err != nil {
		return true, err
	}
	if err := r.markReplaced(ctx, d, fmt.Sprintf("Took over ID %s from Device %s, in place of %s", id, name, old)); err != nil {
		return true, err
	}
	logf.FromContext(ctx).Info("Replaced Device ID", "device", d.Name, "id", id, "oldID", old, "deleted", name)
	r.event(d, target, corev1.EventTypeNormal, reasonReplaced, "Replace",
		"Took over ID %s from Device %s, which was deleted", id, name)
	return true, nil
}

// moveID sets d's ID and removes its annotation in one JSON patch, which applies only while the
// annotation still has the value acted on. It returns the ID it replaced.
func (r *AdoptReconciler) moveID(ctx context.Context, d *homev1alpha1.Device, value, id string) (string, error) {
	path, old := "/spec/plugin/id", ""
	if r.Driver == AdoptZigbee2MQTT {
		path, old = "/spec/zigbee2mqtt/ieeeAddress", d.Spec.Zigbee2MQTT.IEEEAddress
	} else {
		old = d.Spec.Plugin.ID
	}
	annotation := "/metadata/annotations/" + strings.ReplaceAll(homev1alpha1.AnnotationReplaceWith, "/", "~1")
	type op struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value,omitempty"`
	}
	ops, err := json.Marshal([]op{
		{Op: "test", Path: annotation, Value: value},
		{Op: "replace", Path: path, Value: id},
		{Op: "remove", Path: annotation},
	})
	if err != nil {
		return "", err
	}
	return old, client.IgnoreNotFound(r.Patch(ctx, d, client.RawPatch(types.JSONPatchType, ops), client.FieldOwner(shared.FieldOwner)))
}

// markReplaced sets Replaced=True, re-reading the Device on a conflict: the runtime writes its
// status as soon as the new ID resolves, and the annotation that would retry this is gone.
func (r *AdoptReconciler) markReplaced(ctx context.Context, d *homev1alpha1.Device, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
			return client.IgnoreNotFound(err)
		}
		orig := d.DeepCopy()
		meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
			Type: homev1alpha1.ConditionReplaced, Status: metav1.ConditionTrue, Reason: "Replaced",
			Message: message, ObservedGeneration: d.Generation,
		})
		return r.patchStatus(ctx, d, orig)
	})
}

// refuse sets Replaced=False, with an event the first time for each refusal.
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
