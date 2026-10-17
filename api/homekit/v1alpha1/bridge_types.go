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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// LabelBridge is set on objects that belong to a Bridge, including retained ones (identity Secret, PVC).
const LabelBridge = "homekit.oikade.dev/bridge"

// FirstAID is the first accessory ID handed to a Device. AID 1 is the bridge itself.
const FirstAID = 2

// NetworkMode selects how the HAP pod reaches the LAN for mDNS.
// +kubebuilder:validation:Enum=HostNetwork
type NetworkMode string

const (
	NetworkModeHostNetwork NetworkMode = "HostNetwork"
)

// NetworkSpec configures the HAP pod's networking.
type NetworkSpec struct {
	// mode is how the pod attaches to the LAN. Only HostNetwork is supported in v1alpha1.
	// +kubebuilder:default=HostNetwork
	// +optional
	Mode NetworkMode `json:"mode,omitempty"`
}

// PortRange is an inclusive range of TCP ports.
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

// SecretKeyRef selects a key of a Secret in the same namespace.
type SecretKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// StorageSpec configures the PersistentVolumeClaim that holds pairing state.
type StorageSpec struct {
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// +kubebuilder:default="100Mi"
	// +optional
	Size resource.Quantity `json:"size,omitzero"`
}

// BridgeSpec defines a HomeKit bridge that exposes a set of Devices.
type BridgeSpec struct {
	// displayName is the bridge name shown while pairing. Defaults to the object name.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// deviceSelector picks the Devices in this namespace to expose. Empty selects every Device.
	// +optional
	DeviceSelector metav1.LabelSelector `json:"deviceSelector,omitzero"`

	// network configures how the HAP pod reaches the LAN.
	// +kubebuilder:default={mode: HostNetwork}
	// +optional
	Network NetworkSpec `json:"network,omitzero"`

	// portRange is the pool the HAP port is allocated from. Ports are unique across the whole cluster
	// because host-networked pods share each node's port space.
	// +kubebuilder:default={start: 51830, end: 51930}
	// +optional
	PortRange PortRange `json:"portRange,omitzero"`

	// pinSecretRef points at the HomeKit setup code (format XXX-XX-XXX).
	// +required
	PinSecretRef SecretKeyRef `json:"pinSecretRef"`

	// storage configures the PersistentVolumeClaim that holds pairing state.
	// +kubebuilder:default={size: "100Mi"}
	// +optional
	Storage StorageSpec `json:"storage,omitzero"`

	// nodeSelector pins the HAP pod, e.g. to a node on the same L2 network as your Apple devices.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations for the HAP pod.
	// +listType=atomic
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// Accessory records the AID handed to a Device. AIDs key rooms, scenes and favourites in the
// Home app, so they are never reused or reordered, and survive the Device being deleted.
type Accessory struct {
	// device is the Device name.
	// +required
	Device string `json:"device"`

	// aid is the HomeKit accessory ID.
	// +kubebuilder:validation:Minimum=2
	// +required
	AID uint64 `json:"aid"`
}

// BridgeStatus defines the observed state of Bridge.
type BridgeStatus struct {
	// username is the bridge's HomeKit identity (MAC-style ID). It never changes once set.
	// +optional
	Username string `json:"username,omitempty"`

	// port is the allocated HAP port.
	// +optional
	Port int32 `json:"port,omitempty"`

	// accessories maps every Device ever exposed by this Bridge to its AID.
	// +listType=map
	// +listMapKey=device
	// +optional
	Accessories []Accessory `json:"accessories,omitempty"`

	// nextAID is the next AID to hand out.
	// +optional
	NextAID uint64 `json:"nextAID,omitempty"`

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

// AID returns the accessory ID recorded for device, or 0.
func (s *BridgeStatus) AID(device string) uint64 {
	for _, a := range s.Accessories {
		if a.Device == device {
			return a.AID
		}
	}
	return 0
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.status.port`
// +kubebuilder:printcolumn:name="Devices",type=integer,JSONPath=`.status.devices`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Bridge is a HomeKit bridge that exposes Devices to the Home app.
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

// Title returns the display name, falling back to the object name.
func (b *Bridge) Title() string {
	if b.Spec.DisplayName != "" {
		return b.Spec.DisplayName
	}
	return b.Name
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
