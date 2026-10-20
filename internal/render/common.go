// Package render builds the Kubernetes objects the operator manages.
package render

import (
	"crypto/sha256"
	"encoding/hex"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
)

const (
	// AnnotationConfigHash on a pod template rolls the pod when its rendered config changes.
	AnnotationConfigHash = "oikade.dev/config-hash"
	// ManagedBy is the app.kubernetes.io/managed-by value.
	ManagedBy = "oikade"
)

// Hash returns a short, stable hash of b.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// labels returns the app.kubernetes.io labels plus the owning kind's own label.
func labels(app, instance, ownLabel string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app,
		"app.kubernetes.io/instance":   instance,
		"app.kubernetes.io/managed-by": ManagedBy,
		ownLabel:                       instance,
	}
}

func ownerRef(gvk schema.GroupVersionKind, obj metav1.Object) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(obj.GetName()).
		WithUID(obj.GetUID()).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

// pvc returns a claim with no owner reference: it holds state that must outlive its owner.
func pvc(name, namespace string, labels map[string]string, class *string, size resource.Quantity) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
}

func containerSecurityContext() *corev1ac.SecurityContextApplyConfiguration {
	return corev1ac.SecurityContext().
		WithAllowPrivilegeEscalation(false).
		WithCapabilities(corev1ac.Capabilities().WithDrop("ALL"))
}

func podSecurityContext(id int64) *corev1ac.PodSecurityContextApplyConfiguration {
	return corev1ac.PodSecurityContext().
		WithRunAsUser(id).WithRunAsGroup(id).WithFSGroup(id).WithRunAsNonRoot(true).
		WithSeccompProfile(corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault))
}

func resources(r corev1.ResourceRequirements) *corev1ac.ResourceRequirementsApplyConfiguration {
	ac := corev1ac.ResourceRequirements()
	if len(r.Requests) > 0 {
		ac.WithRequests(r.Requests)
	}
	if len(r.Limits) > 0 {
		ac.WithLimits(r.Limits)
	}
	return ac
}

func toleration(t corev1.Toleration) *corev1ac.TolerationApplyConfiguration {
	ac := corev1ac.Toleration().WithKey(t.Key).WithOperator(t.Operator).WithValue(t.Value).WithEffect(t.Effect)
	if t.TolerationSeconds != nil {
		ac.WithTolerationSeconds(*t.TolerationSeconds)
	}
	return ac
}

func schedule(pod *corev1ac.PodSpecApplyConfiguration, nodeSelector map[string]string, tolerations []corev1.Toleration) {
	if len(nodeSelector) > 0 {
		pod.WithNodeSelector(nodeSelector)
	}
	for _, t := range tolerations {
		pod.WithTolerations(toleration(t))
	}
}
