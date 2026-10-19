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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ConditionPlanned is True when today's Schedule plan meets its target. It is written by the runtime.
const ConditionPlanned = "Planned"

// TimeOfDay is a local wall-clock time, HH:MM, in the runtime's --timezone.
// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
// +kubebuilder:validation:MaxLength=5
type TimeOfDay string

// Weekday is a day of the week.
// +kubebuilder:validation:Enum=Mon;Tue;Wed;Thu;Fri;Sat;Sun
type Weekday string

// TimeWindow is a daily window. An end at or before start wraps past midnight and belongs to the day it starts.
// +kubebuilder:validation:XValidation:rule="self.start != self.end",message="start and end must differ; omit window for the whole day"
type TimeWindow struct {
	// start is the first minute of the window.
	// +required
	Start TimeOfDay `json:"start"`

	// end is the minute after the window ends.
	// +required
	End TimeOfDay `json:"end"`

	// days limits the window to these weekdays. Empty means every day.
	// +listType=set
	// +kubebuilder:validation:MaxItems=7
	// +optional
	Days []Weekday `json:"days,omitempty"`
}

// ScheduleRule shapes when the target runs. Exactly one of require, forbid or prefer is set.
// +kubebuilder:validation:XValidation:rule="[has(self.require) && self.require, has(self.forbid) && self.forbid, has(self.prefer)].filter(x, x).size() == 1",message="exactly one of require, forbid or prefer must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.live) && self.live) || (has(self.require) && self.require && has(self.when))",message="live rules must set require and when"
type ScheduleRule struct {
	// name identifies the rule in status, logs and metrics.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// window limits the rule to a time of day. Omitted means the whole day.
	// +optional
	Window *TimeWindow `json:"window,omitempty"`

	// when is a CEL condition, ANDed with the window. Planning rules see devices, now, tz and slot
	// (start, end, hour, minute and weekday, 0 = Sunday). Live rules see devices, now and tz.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	When string `json:"when,omitempty"`

	// live re-evaluates the rule whenever an input changes, instead of once at planningTime.
	// Only require rules can be live.
	// +optional
	Live bool `json:"live,omitempty"`

	// require turns the target on in the matching slots.
	// +optional
	Require bool `json:"require,omitempty"`

	// forbid keeps the budget out of the matching slots. Require rules still win.
	// +optional
	Forbid bool `json:"forbid,omitempty"`

	// prefer weights the matching slots for the budget. Negative values avoid them.
	// +kubebuilder:validation:Minimum=-100
	// +kubebuilder:validation:Maximum=100
	// +optional
	Prefer *int32 `json:"prefer,omitempty"`
}

// CapabilityRef names a capability on a Device in the same namespace.
type CapabilityRef struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Device string `json:"device"`

	// capability is the canonical capability, e.g. switch.on.
	// +kubebuilder:validation:MinLength=1
	// +required
	Capability string `json:"capability"`
}

// MinutesInput is a capability read at planningTime.
type MinutesInput struct {
	CapabilityRef `json:",inline"`

	// maxAge is how old the value may be. Older values count as missing.
	// +kubebuilder:default="30m"
	// +optional
	MaxAge metav1.Duration `json:"maxAge,omitzero"`
}

// MinutesBand maps input values below a threshold to a daily budget. The last band has no threshold.
type MinutesBand struct {
	// below is the exclusive upper bound of input values in this band, e.g. "12" or "-2.5".
	// +optional
	Below *resource.Quantity `json:"below,omitempty"`

	// minutes is the daily budget for this band.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1440
	// +required
	Minutes int32 `json:"minutes"`
}

// MinutesBands picks the daily budget from the band an input value falls in.
type MinutesBands struct {
	// input is the capability whose value picks the band.
	// +required
	Input MinutesInput `json:"input"`

	// missingMinutes is the budget when the input is missing or older than maxAge.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1440
	// +required
	MissingMinutes int32 `json:"missingMinutes"`

	// ranges must ascend by below, and only the last omits below.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +required
	Ranges []MinutesBand `json:"ranges"`
}

// ScheduleMinutes sets the daily budget. Exactly one of bands or expression is set.
// +kubebuilder:validation:XValidation:rule="has(self.bands) != has(self.expression)",message="exactly one of bands or expression must be set"
type ScheduleMinutes struct {
	// bands picks the budget from an input capability.
	// +optional
	Bands *MinutesBands `json:"bands,omitempty"`

	// expression is CEL over devices, now and tz, returning minutes as an int.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Expression string `json:"expression,omitempty"`
}

// ScheduleConstraints limit how the budget is placed. Durations must be multiples of slotMinutes.
type ScheduleConstraints struct {
	// minimumRun is the shortest planned block, and the shortest run after a live rule switches the target on.
	// +optional
	MinimumRun metav1.Duration `json:"minimumRun,omitzero"`

	// minimumRest is the shortest gap between runs.
	// +optional
	MinimumRest metav1.Duration `json:"minimumRest,omitzero"`

	// maximumStartsPerDay limits starts per planning day. Zero means unlimited. Live rules ignore it.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaximumStartsPerDay int32 `json:"maximumStartsPerDay,omitempty"`
}

// ScheduleOverride sets how manual changes to the target are held.
type ScheduleOverride struct {
	// duration is how long a manual change, or turning automatic off, holds the target.
	// +kubebuilder:default="2h"
	// +optional
	Duration metav1.Duration `json:"duration,omitzero"`
}

// ScheduleSpec decides when one writable bool capability is on.
type ScheduleSpec struct {
	// target is the capability the schedule drives, e.g. switch.on.
	// +required
	Target CapabilityRef `json:"target"`

	// planningTime starts each planning day, in the runtime's --timezone.
	// +kubebuilder:default="00:00"
	// +optional
	PlanningTime TimeOfDay `json:"planningTime,omitempty"`

	// slotMinutes divides the day into slots. Windows and durations must be multiples of it.
	// +kubebuilder:validation:Enum=15;30;60
	// +kubebuilder:default=30
	// +optional
	SlotMinutes int32 `json:"slotMinutes,omitempty"`

	// minutes is the daily budget. Without it, the target is on exactly in required slots.
	// +optional
	Minutes *ScheduleMinutes `json:"minutes,omitempty"`

	// rules decide which slots are required, forbidden or preferred. Precedence is require > forbid > prefer.
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Rules []ScheduleRule `json:"rules,omitempty"`

	// constraints limit how the budget is placed.
	// +optional
	Constraints ScheduleConstraints `json:"constraints,omitzero"`

	// override sets how manual changes to the target are held.
	// +kubebuilder:default={}
	// +optional
	Override ScheduleOverride `json:"override,omitzero"`
}

// ScheduleBlock is one planned run of the target.
type ScheduleBlock struct {
	// start is when the target switches on.
	// +required
	Start metav1.Time `json:"start"`

	// end is when the target switches off.
	// +required
	End metav1.Time `json:"end"`
}

// ScheduleDay is the frozen plan for one planning day, plus coarse progress.
type ScheduleDay struct {
	// date is the planning day, YYYY-MM-DD, local time.
	// +required
	Date string `json:"date"`

	// targetSource is Bands, Missing, Expression or None.
	// +kubebuilder:validation:Enum=Bands;Missing;Expression;None
	// +required
	TargetSource string `json:"targetSource"`

	// targetMinutes is today's budget.
	// +required
	TargetMinutes int32 `json:"targetMinutes"`

	// scheduledMinutes is the total length of today's blocks.
	// +required
	ScheduledMinutes int32 `json:"scheduledMinutes"`

	// shortfallMinutes is how much of the budget the constraints left unplaced.
	// +required
	ShortfallMinutes int32 `json:"shortfallMinutes"`

	// blocks are today's planned runs.
	// +listType=atomic
	// +optional
	Blocks []ScheduleBlock `json:"blocks,omitempty"`

	// runMinutes is the credited runtime so far, checkpointed at most every 5 minutes.
	// +required
	RunMinutes int32 `json:"runMinutes"`

	// starts counts on commands sent today.
	// +required
	Starts int32 `json:"starts"`
}

// ScheduleStatus is the runtime's plan for today and the manager's validation.
type ScheduleStatus struct {
	// today is the frozen plan for the current planning day. It is written by the runtime.
	// +optional
	Today *ScheduleDay `json:"today,omitempty"`

	// observedGeneration is the generation the Ready condition was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions: Ready (manager) and Planned (runtime).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.device`
// +kubebuilder:printcolumn:name="Today",type=integer,JSONPath=`.status.today.targetMinutes`
// +kubebuilder:printcolumn:name="Run",type=integer,JSONPath=`.status.today.runMinutes`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Planned",type=string,JSONPath=`.status.conditions[?(@.type=="Planned")].status`

// Schedule drives one writable bool capability from rules and an optional daily budget. It is run by the Oikade runtime.
type Schedule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Schedule
	// +required
	Spec ScheduleSpec `json:"spec"`

	// status defines the observed state of Schedule
	// +optional
	Status ScheduleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ScheduleList contains a list of Schedule
type ScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Schedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Schedule{}, &ScheduleList{})
		return nil
	})
}
