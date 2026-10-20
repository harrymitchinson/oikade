// Package shared holds helpers every operator controller uses the same way.
package shared

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FieldOwner is the server-side apply field manager of every operator controller.
const FieldOwner = "oikade"

// Apply server-side applies obj, taking ownership of the fields it sets.
func Apply(ctx context.Context, c client.Client, obj runtime.ApplyConfiguration) error {
	return c.Apply(ctx, obj, client.FieldOwner(FieldOwner), client.ForceOwnership)
}

// DeploymentAvailable reports whether the current revision of a one-replica Deployment is serving.
func DeploymentAvailable(d *appsv1.Deployment) bool {
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas >= 1 &&
		d.Status.AvailableReplicas >= 1
}
