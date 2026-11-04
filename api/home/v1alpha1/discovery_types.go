package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AnnotationMatter overrides whether a Device is shown on Matter bridges: hide keeps it off every
// bridge, and expose, or no annotation, leaves it to each bridge's deviceSelector. An adopted Device
// gets hide when its instance's discovery.matter is Hide.
const AnnotationMatter = "oikade.dev/matter"

// The values of AnnotationMatter.
const (
	MatterHide   = "hide"
	MatterExpose = "expose"
)

// HiddenFromMatter reports whether a Device's annotation keeps it off every Matter bridge.
func (d *Device) HiddenFromMatter() bool {
	return d.Annotations[AnnotationMatter] == MatterHide
}

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

// DiscoveredDevice is a device a driver instance sees in its app, declared or not, as the runtime
// last saw it.
type DiscoveredDevice struct {
	// id is the driver's ID for the device: an IEEE address, or a plugin's discovered ID.
	// +kubebuilder:validation:MaxLength=63
	// +required
	ID string `json:"id"`

	// name is the device's name in its app, if it has one.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Name string `json:"name,omitempty"`

	// room is the room the device is in, in its app, if it has one.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Room string `json:"room,omitempty"`

	// manufacturer as reported by the app.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Manufacturer string `json:"manufacturer,omitempty"`

	// model as reported by the app.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Model string `json:"model,omitempty"`

	// capabilities the device has, by canonical name.
	// +listType=atomic
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`
}
