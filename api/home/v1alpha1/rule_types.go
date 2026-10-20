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

// ConditionReady is True when a Rule's condition compiles and its actions are valid.
const ConditionReady = "Ready"

// ConditionScheduled is set by the runtime on a Rule with time or sun triggers. It is False when
// the Rule can't be scheduled, such as a sun trigger on a runtime without a location.
const ConditionScheduled = "Scheduled"

// Trigger is a moment that fires the Rule. Exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="[has(self.change), has(self.at), has(self.sun)].exists_one(x, x)",message="exactly one of change, at or sun must be specified"
type Trigger struct {
	// change fires when a capability changes value.
	// +optional
	Change *ChangeTrigger `json:"change,omitempty"`

	// at fires at a local time of day.
	// +optional
	At *AtTrigger `json:"at,omitempty"`

	// sun fires at sunrise or sunset, from the runtime's --latitude and --longitude.
	// +optional
	Sun *SunTrigger `json:"sun,omitempty"`
}

// Weekday is a day of the week.
// +kubebuilder:validation:Enum=Mon;Tue;Wed;Thu;Fri;Sat;Sun
type Weekday string

// AtTrigger fires the Rule at a time of day in the runtime's --timezone.
type AtTrigger struct {
	// time is the local time, HH:MM. A time skipped by a daylight saving change fires when the
	// clocks go forward, and a time that happens twice fires the first time.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +required
	Time string `json:"time"`

	// days limits the trigger to these days. A day is the calendar day the trigger fires on, so
	// 00:30 on Friday and Saturday nights is days [Sat, Sun]. Empty means every day.
	// +kubebuilder:validation:MaxItems=7
	// +listType=set
	// +optional
	Days []Weekday `json:"days,omitempty"`
}

// SunEvent is a sun event.
// +kubebuilder:validation:Enum=Sunrise;Sunset
type SunEvent string

const (
	SunEventSunrise SunEvent = "Sunrise"
	SunEventSunset  SunEvent = "Sunset"
)

// SunTrigger fires the Rule at sunrise or sunset, moved by an offset.
// +kubebuilder:validation:XValidation:rule="!has(self.offset) || (duration(self.offset) >= duration('-6h') && duration(self.offset) <= duration('6h'))",message="offset must be between -6h and 6h"
type SunTrigger struct {
	// event is Sunrise or Sunset.
	// +required
	Event SunEvent `json:"event"`

	// offset moves the trigger from the event, e.g. -15m for 15 minutes before.
	// +optional
	Offset *metav1.Duration `json:"offset,omitempty"`

	// days limits the trigger to these days. A day is the calendar day the trigger fires on.
	// Empty means every day.
	// +kubebuilder:validation:MaxItems=7
	// +listType=set
	// +optional
	Days []Weekday `json:"days,omitempty"`
}

// ChangeTrigger fires the Rule when a capability of a device, or of any device in a room, changes value.
// +kubebuilder:validation:XValidation:rule="has(self.device) != has(self.room)",message="exactly one of device or room must be specified"
type ChangeTrigger struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Device string `json:"device,omitempty"`

	// room names a Room in the same namespace. The trigger matches a change from any Device whose
	// roomRef is this room at the time of the change.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Room string `json:"room,omitempty"`

	// capability is the canonical capability to watch, e.g. sensor.occupancy-detected.
	// +kubebuilder:validation:MinLength=1
	// +required
	Capability string `json:"capability"`
}

// SetAction sets a capability on a device, or on every device in a room that has the capability.
// +kubebuilder:validation:XValidation:rule="has(self.device) != has(self.room)",message="exactly one of device or room must be specified"
type SetAction struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Device string `json:"device,omitempty"`

	// room names a Room in the same namespace. Every discovered Device in it that reports the capability is set,
	// in name order.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Room string `json:"room,omitempty"`

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

// Action is one step of a Rule. Exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="has(self.set) != has(self.delay)",message="exactly one of set or delay must be specified"
type Action struct {
	// set sets a capability on a device.
	// +optional
	Set *SetAction `json:"set,omitempty"`

	// delay waits before the next action, e.g. 5m.
	// +optional
	Delay *metav1.Duration `json:"delay,omitempty"`
}

// RuleMode decides what happens when a trigger fires while a run is in progress.
// +kubebuilder:validation:Enum=Single;Restart
type RuleMode string

const (
	// RuleModeSingle ignores triggers while a run is in progress.
	RuleModeSingle RuleMode = "Single"
	// RuleModeRestart cancels the run in progress and starts again, e.g. to keep a
	// light on until five minutes after the last motion.
	RuleModeRestart RuleMode = "Restart"
)

// RuleSpec is a moment rule: when a trigger fires and the condition holds, run the actions.
type RuleSpec struct {
	// triggers are the moments that start the Rule.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +required
	Triggers []Trigger `json:"triggers"`

	// condition is a CEL expression that must evaluate to true for the actions to run.
	// Variables: devices (map of device name to map of capability to value),
	// trigger (type, which is change, at or sun, and for a change device, capability, value and
	// previous), now (timestamp) and tz (the runtime's timezone). Timestamp accessors default
	// to UTC, so pass tz for local time.
	// For example: trigger.value && (now.getHours(tz) >= 18 || now.getHours(tz) < 7)
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Condition string `json:"condition,omitempty"`

	// do are the actions, run in order.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +required
	Do []Action `json:"do"`

	// mode decides what happens when a trigger fires while a run is in progress.
	// +kubebuilder:default=Single
	// +optional
	Mode RuleMode `json:"mode,omitempty"`

	// startingDeadline is how late an at or sun trigger may still fire after the runtime was
	// down at its time. A later one is skipped and counted as missed. Unset skips every missed
	// time.
	// +optional
	StartingDeadline *metav1.Duration `json:"startingDeadline,omitempty"`
}

// RuleStatus defines the observed state of Rule.
type RuleStatus struct {
	// lastTriggered is when the actions last started (updated at most every minute).
	// +optional
	LastTriggered metav1.Time `json:"lastTriggered,omitzero"`

	// nextFire is when the next at or sun trigger fires.
	// +optional
	NextFire metav1.Time `json:"nextFire,omitzero"`

	// lastFire is the scheduled time of the last at or sun trigger the runtime fired or skipped,
	// so a restart doesn't fire it again.
	// +optional
	LastFire metav1.Time `json:"lastFire,omitzero"`

	// observedGeneration is the generation the Ready condition was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions: Ready, set by the operator, and Scheduled, set by the runtime.
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
// +kubebuilder:printcolumn:name="Next Fire",type=date,JSONPath=`.status.nextFire`

// Rule is run by the Oikade runtime: when one of its triggers fires and the condition holds, it runs its actions.
type Rule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Rule
	// +required
	Spec RuleSpec `json:"spec"`

	// status defines the observed state of Rule
	// +optional
	Status RuleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RuleList contains a list of Rule
type RuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Rule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Rule{}, &RuleList{})
		return nil
	})
}
