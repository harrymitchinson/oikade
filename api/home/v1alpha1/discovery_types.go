package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DiscoveryMatter says whether Devices created for discovered devices are shown on Matter bridges.
// +kubebuilder:validation:Enum=Expose;Hide
type DiscoveryMatter string

const (
	// DiscoveryMatterExpose shows discovered Devices on the bridges that select them.
	DiscoveryMatterExpose DiscoveryMatter = "Expose"
	// DiscoveryMatterHide keeps discovered Devices off every bridge.
	DiscoveryMatterHide DiscoveryMatter = "Hide"
)

// RemovalPolicy says what happens to a discovered Device once its device is removed from its app.
// +kubebuilder:validation:Enum=Retain;Delete
type RemovalPolicy string

const (
	// RemovalPolicyRetain keeps the Device, with Resolved=False, for you to delete.
	RemovalPolicyRetain RemovalPolicy = "Retain"
	// RemovalPolicyDelete deletes the Device once its device has been gone for the grace period.
	RemovalPolicyDelete RemovalPolicy = "Delete"
)

// DefaultRemovalGracePeriod is how long a device must be gone from its app before RemovalPolicyDelete
// deletes its Device, when removalGracePeriod isn't set.
const DefaultRemovalGracePeriod = 24 * time.Hour

// DiscoverySpec configures how a driver instance adopts the devices paired in its app, so they
// appear as Devices without being declared in Git (OIK-123). Offline devices are never removed.
// +kubebuilder:validation:XValidation:rule="!has(self.removalGracePeriod) || self.removalPolicy == 'Delete'",message="removalGracePeriod applies only to removalPolicy Delete"
type DiscoverySpec struct {
	// adopt creates a Device for each device paired in the app that no Device declares.
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// matter says whether the Devices it creates are shown on Matter bridges.
	// +kubebuilder:default=Expose
	// +optional
	Matter DiscoveryMatter `json:"matter,omitempty"`

	// removalPolicy says what happens to a created Device once its device is removed from its app.
	// Retain keeps it with Resolved=False; Delete deletes it after removalGracePeriod.
	// +kubebuilder:default=Retain
	// +optional
	RemovalPolicy RemovalPolicy `json:"removalPolicy,omitempty"`

	// removalGracePeriod is how long a device must be gone from its app before its Device is deleted.
	// Only with removalPolicy Delete. Defaults to 24h.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="removalGracePeriod must be positive"
	// +optional
	RemovalGracePeriod *metav1.Duration `json:"removalGracePeriod,omitempty"`
}
