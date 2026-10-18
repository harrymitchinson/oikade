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
