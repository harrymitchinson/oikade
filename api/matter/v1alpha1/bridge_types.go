package v1alpha1

import (
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// LabelBridge is set on objects that belong to a Bridge, including retained ones (its Secret and PVC).
const LabelBridge = "matter.oikade.dev/bridge"

// FirstEndpoint is the first endpoint ID handed to a Device. Endpoint 0 is the root node and
// endpoint 1 the aggregator.
const FirstEndpoint = 2

// MaxEndpoint is the highest endpoint ID. 0xFFFF is reserved.
const MaxEndpoint = 0xFFFE

// MaxLabelBytes is the longest NodeLabel Matter allows, in bytes of UTF-8.
const MaxLabelBytes = 32

// NetworkMode selects how the bridge pod reaches the LAN for mDNS and IPv6.
// +kubebuilder:validation:Enum=HostNetwork
type NetworkMode string

const (
	NetworkModeHostNetwork NetworkMode = "HostNetwork"
)

// NetworkSpec configures the bridge pod's networking.
type NetworkSpec struct {
	// mode is how the pod attaches to the LAN. Only HostNetwork is supported in v1alpha1.
	// +kubebuilder:default=HostNetwork
	// +optional
	Mode NetworkMode `json:"mode,omitempty"`
}

// PortRange is an inclusive range of UDP ports.
// +kubebuilder:validation:XValidation:rule="self.start <= self.end",message="start must be <= end"
type PortRange struct {
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	Start int32 `json:"start"`
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	End int32 `json:"end"`
}

// Contains reports whether port lies within the range.
func (r PortRange) Contains(port int32) bool {
	return port >= r.Start && port <= r.End
}

// StorageSpec configures the PersistentVolumeClaim that holds the bridge's fabrics.
type StorageSpec struct {
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// +kubebuilder:default="100Mi"
	// +optional
	Size resource.Quantity `json:"size,omitzero"`
}

// BridgeSpec defines a Matter bridge that exposes a set of Devices, one bridged endpoint each.
type BridgeSpec struct {
	// displayName is the bridge's name (its NodeLabel) shown while commissioning, at most 32 bytes
	// of UTF-8. Defaults to the object name, cut to 32 bytes.
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="bytes(self).size() <= 32",message="displayName must be at most 32 bytes of UTF-8"
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// deviceSelector picks the Devices in this namespace to expose. Empty selects every Device.
	// +optional
	DeviceSelector metav1.LabelSelector `json:"deviceSelector,omitzero"`

	// network configures how the bridge pod reaches the LAN.
	// +kubebuilder:default={mode: HostNetwork}
	// +optional
	Network NetworkSpec `json:"network,omitzero"`

	// portRange is the pool the Matter UDP port is allocated from. Ports are unique across the
	// whole cluster because host-networked pods share each node's port space.
	// +kubebuilder:default={start: 5540, end: 5640}
	// +optional
	PortRange PortRange `json:"portRange,omitzero"`

	// storage configures the PersistentVolumeClaim that holds the bridge's fabrics.
	// +kubebuilder:default={size: "100Mi"}
	// +optional
	Storage StorageSpec `json:"storage,omitzero"`

	// nodeSelector pins the bridge pod, e.g. to a node on the same L2 network as your controllers.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations for the bridge pod.
	// +listType=atomic
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// Endpoint records the endpoint ID handed to a Device. Controllers key rooms, scenes and
// automations by it, so it is never reused or reordered, and survives the Device being deleted.
// +kubebuilder:validation:XValidation:rule="self.device == oldSelf.device",message="an endpoint's Device never changes"
type Endpoint struct {
	// device is the Device name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Device string `json:"device"`

	// endpoint is the Matter endpoint ID.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=65534
	// +required
	Endpoint int32 `json:"endpoint"`
}

// Fabric is a fabric the bridge has been commissioned into: one controller ecosystem.
type Fabric struct {
	// fabricIndex is the bridge's local index for the fabric.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=254
	// +required
	FabricIndex int32 `json:"fabricIndex"`

	// fabricID is the fabric ID, as 16 upper-case hex digits.
	// +kubebuilder:validation:Pattern=`^[0-9A-F]{16}$`
	// +required
	FabricID string `json:"fabricID"`

	// nodeID is the bridge's node ID in the fabric, as 16 upper-case hex digits.
	// +kubebuilder:validation:Pattern=`^[0-9A-F]{16}$`
	// +required
	NodeID string `json:"nodeID"`

	// vendorID is the vendor ID of the controller that commissioned the fabric.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	// +required
	VendorID int32 `json:"vendorID"`

	// label is the label the controller gave the fabric, if any.
	// +kubebuilder:validation:MaxLength=32
	// +optional
	Label string `json:"label,omitempty"`
}

// BridgeStatus defines the observed state of Bridge.
//
// Endpoints are keyed by ID, so IDs are unique. They are allocated densely from FirstEndpoint, each
// ID's Device never changes, and nextEndpoint never decreases, so no entry can be removed, changed
// or handed to another Device. A Device with two entries isn't rejected: the n² check is over the
// CEL cost budget, and the controller only allocates for a Device that has no entry.
// +kubebuilder:validation:XValidation:rule="(has(self.endpoints) ? self.endpoints.size() : 0) + 2 == (has(self.nextEndpoint) ? self.nextEndpoint : 2)",message="endpoints are allocated densely: nextEndpoint must be 2 plus the number of endpoints"
// +kubebuilder:validation:XValidation:rule="!has(self.endpoints) || self.endpoints.all(e, e.endpoint < (has(self.nextEndpoint) ? self.nextEndpoint : 2))",message="every endpoint must be below nextEndpoint"
// +kubebuilder:validation:XValidation:rule="(has(self.nextEndpoint) ? self.nextEndpoint : 2) >= (has(oldSelf.nextEndpoint) ? oldSelf.nextEndpoint : 2)",message="endpoints are append-only: nextEndpoint never decreases"
type BridgeStatus struct {
	// port is the allocated Matter UDP port.
	// +optional
	Port int32 `json:"port,omitempty"`

	// discriminator is the 12-bit discriminator commissioners find the bridge by.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4095
	// +optional
	Discriminator *int32 `json:"discriminator,omitempty"`

	// fabrics lists the fabrics the bridge has been commissioned into.
	// +listType=map
	// +listMapKey=fabricIndex
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Fabrics []Fabric `json:"fabrics,omitempty"`

	// endpoints maps every Device ever exposed by this Bridge to its endpoint ID. Entries are
	// never removed or changed.
	// +listType=map
	// +listMapKey=endpoint
	// +kubebuilder:validation:MaxItems=10000
	// +optional
	Endpoints []Endpoint `json:"endpoints,omitempty"`

	// nextEndpoint is the next endpoint ID to hand out.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=65535
	// +optional
	NextEndpoint int32 `json:"nextEndpoint,omitempty"`

	// devices is the number of Devices currently exposed.
	// +optional
	Devices int32 `json:"devices,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Endpoint returns the endpoint ID recorded for device, or 0.
func (s *BridgeStatus) Endpoint(device string) int32 {
	for _, e := range s.Endpoints {
		if e.Device == device {
			return e.Endpoint
		}
	}
	return 0
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=matterbridge;matterbridges
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.status.port`
// +kubebuilder:printcolumn:name="Devices",type=integer,JSONPath=`.status.devices`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Bridge is a Matter bridge that exposes Devices to Matter controllers such as Apple Home.
type Bridge struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Bridge
	// +required
	Spec BridgeSpec `json:"spec"`

	// status defines the observed state of Bridge
	// +optional
	Status BridgeStatus `json:"status,omitzero"`
}

// Title returns the display name, falling back to the object name, cut to MaxLabelBytes.
func (b *Bridge) Title() string {
	t := b.Spec.DisplayName
	if t == "" {
		t = b.Name
	}
	for len(t) > MaxLabelBytes {
		_, size := utf8.DecodeLastRuneInString(t)
		t = t[:len(t)-size]
	}
	return t
}

// +kubebuilder:object:root=true

// BridgeList contains a list of Bridge
type BridgeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Bridge `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Bridge{}, &BridgeList{})
		return nil
	})
}
