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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ConditionReady is True when an Automation's condition compiles and its actions are valid.
const ConditionReady = "Ready"

// Trigger fires the Automation when a capability of a device changes value.
type Trigger struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Device string `json:"device"`

	// capability is the canonical capability to watch, e.g. sensor.occupancy-detected.
	// +kubebuilder:validation:MinLength=1
	// +required
	Capability string `json:"capability"`
}

// SetAction sets a capability on a device.
type SetAction struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Device string `json:"device"`

	// capability is the writable capability to set, e.g. light.level.
	// +kubebuilder:validation:MinLength=1
	// +required
	Capability string `json:"capability"`

	// value is a bool or number matching the capability's kind.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=""
	// +kubebuilder:pruning:PreserveUnknownFields
	// +required
	Value apiextensionsv1.JSON `json:"value"`
}

// Action is one step of an Automation. Exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="has(self.set) != has(self.delay)",message="exactly one of set or delay must be specified"
type Action struct {
	// set sets a capability on a device.
	// +optional
	Set *SetAction `json:"set,omitempty"`

	// delay waits before the next action, e.g. 5m.
	// +optional
	Delay *metav1.Duration `json:"delay,omitempty"`
}

// AutomationMode decides what happens when a trigger fires while a run is in progress.
// +kubebuilder:validation:Enum=Single;Restart
type AutomationMode string

const (
	// AutomationModeSingle ignores triggers while a run is in progress.
	AutomationModeSingle AutomationMode = "Single"
	// AutomationModeRestart cancels the run in progress and starts again, e.g. to keep a
	// light on until five minutes after the last motion.
	AutomationModeRestart AutomationMode = "Restart"
)

// AutomationSpec is an event-driven rule: when a trigger fires and the condition holds, run the actions.
type AutomationSpec struct {
	// triggers are the capability changes that start the Automation.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +required
	Triggers []Trigger `json:"triggers"`

	// condition is a CEL expression that must evaluate to true for the actions to run.
	// Variables: devices (map of device name to map of capability to value),
	// trigger (device, capability, value, previous) and now (timestamp).
	// For example: trigger.value && (now.getHours() >= 18 || now.getHours() < 7)
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Condition string `json:"condition,omitempty"`

	// actions run in order.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +required
	Actions []Action `json:"actions"`

	// mode decides what happens when a trigger fires while a run is in progress.
	// +kubebuilder:default=Single
	// +optional
	Mode AutomationMode `json:"mode,omitempty"`
}

// AutomationStatus defines the observed state of Automation.
type AutomationStatus struct {
	// lastTriggered is when the actions last started (updated at most every minute).
	// +optional
	LastTriggered metav1.Time `json:"lastTriggered,omitzero"`

	// observedGeneration is the generation the Ready condition was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last Triggered",type=date,JSONPath=`.status.lastTriggered`

// Automation is a rule run by the Oikade runtime.
type Automation struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Automation
	// +required
	Spec AutomationSpec `json:"spec"`

	// status defines the observed state of Automation
	// +optional
	Status AutomationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AutomationList contains a list of Automation
type AutomationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Automation `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Automation{}, &AutomationList{})
		return nil
	})
}
