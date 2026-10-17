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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Device condition types, written by the runtime.
const (
	// ConditionResolved is True once the driver has found the device and reported its capabilities.
	ConditionResolved = "Resolved"
	// ConditionAvailable mirrors the driver's view of whether the device is reachable.
	ConditionAvailable = "Available"
)

// ZigbeeDeviceSource identifies a device paired with a Zigbee2MQTT instance.
type ZigbeeDeviceSource struct {
	// instanceRef names the Zigbee2MQTT object the device is paired with.
	// +required
	InstanceRef corev1.LocalObjectReference `json:"instanceRef"`

	// ieeeAddress is the device's IEEE address. It is used instead of the friendly name
	// because it never changes; the operator sets the friendly name to the Device's name.
	// +kubebuilder:validation:Pattern=`^0x[0-9a-f]{16}$`
	// +required
	IEEEAddress string `json:"ieeeAddress"`
}

// HomeKitService overrides which HomeKit service a device is exposed as.
// +kubebuilder:validation:Enum=Lightbulb;Switch;Outlet;MotionSensor;OccupancySensor
type HomeKitService string

const (
	HomeKitServiceLightbulb       HomeKitService = "Lightbulb"
	HomeKitServiceSwitch          HomeKitService = "Switch"
	HomeKitServiceOutlet          HomeKitService = "Outlet"
	HomeKitServiceMotionSensor    HomeKitService = "MotionSensor"
	HomeKitServiceOccupancySensor HomeKitService = "OccupancySensor"
)

// DeviceHomeKit tunes how a device appears in HomeKit.
type DeviceHomeKit struct {
	// service overrides the default service chosen from the device's capabilities,
	// e.g. Outlet for a smart plug that reports itself as a switch.
	// +optional
	Service HomeKitService `json:"service,omitempty"`
}

// DeviceSpec declares a device that should exist in the home. Exactly one driver source must be set.
// +kubebuilder:validation:XValidation:rule="has(self.zigbee2mqtt)",message="a driver source (zigbee2mqtt) is required"
type DeviceSpec struct {
	// displayName is the human-friendly name shown in HomeKit and Grafana. Defaults to the object name.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// roomRef names the Room the device is in.
	// +optional
	RoomRef *corev1.LocalObjectReference `json:"roomRef,omitempty"`

	// zigbee2mqtt sources the device from a Zigbee2MQTT instance.
	// +optional
	Zigbee2MQTT *ZigbeeDeviceSource `json:"zigbee2mqtt,omitempty"`

	// homekit tunes how the device appears in HomeKit.
	// +optional
	HomeKit *DeviceHomeKit `json:"homekit,omitempty"`
}

// CapabilityKind is the value type of a capability.
// +kubebuilder:validation:Enum=Bool;Number
type CapabilityKind string

const (
	CapabilityKindBool   CapabilityKind = "Bool"
	CapabilityKindNumber CapabilityKind = "Number"
)

// DeviceCapability is one capability a driver reported for the device.
type DeviceCapability struct {
	// name is the canonical capability, e.g. light.on or sensor.temperature.
	// +required
	Name string `json:"name"`

	// kind is the capability's value type.
	// +required
	Kind CapabilityKind `json:"kind"`

	// writable is true when the capability can be set, e.g. light.on but not sensor.temperature.
	// +optional
	Writable bool `json:"writable,omitempty"`
}

// DeviceStatus is the driver's coarse view of the device. Live values are on the MQTT bus, never here.
type DeviceStatus struct {
	// capabilities the driver reported for the device.
	// +listType=map
	// +listMapKey=name
	// +optional
	Capabilities []DeviceCapability `json:"capabilities,omitempty"`

	// manufacturer as reported by the device.
	// +optional
	Manufacturer string `json:"manufacturer,omitempty"`

	// model as reported by the device.
	// +optional
	Model string `json:"model,omitempty"`

	// lastSeen is when the driver last heard from the device (updated at most every few minutes).
	// +optional
	LastSeen metav1.Time `json:"lastSeen,omitzero"`

	// conditions: Resolved and Available.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Room",type=string,JSONPath=`.spec.roomRef.name`
// +kubebuilder:printcolumn:name="Capabilities",type=string,JSONPath=`.status.capabilities[*].name`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Last Seen",type=date,JSONPath=`.status.lastSeen`

// Device is a device declared in Git. Its spec is desired state; its status is written by the runtime.
type Device struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Device
	// +required
	Spec DeviceSpec `json:"spec"`

	// status defines the observed state of Device
	// +optional
	Status DeviceStatus `json:"status,omitzero"`
}

// Title returns the display name, falling back to the object name.
func (d *Device) Title() string {
	if d.Spec.DisplayName != "" {
		return d.Spec.DisplayName
	}
	return d.Name
}

// +kubebuilder:object:root=true

// DeviceList contains a list of Device
type DeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Device `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Device{}, &DeviceList{})
		return nil
	})
}
