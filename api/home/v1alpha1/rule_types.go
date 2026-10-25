package v1alpha1

import (
	"bytes"
	"encoding/json"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ConditionReady is True when the operator finds the spec valid: a Rule's condition compiles and
// its actions are valid, or a Device's control entries fit their capabilities.
const ConditionReady = "Ready"

// ConditionScheduled is set by the runtime on a Rule with time or sun triggers, and on a hold whose
// window has sun times that can't be computed. It is False when the Rule can't be scheduled, such
// as a sun trigger or sun window on a runtime without a location.
const ConditionScheduled = "Scheduled"

// ConditionPlanned is set by the runtime on a budget hold. It is True once today's plan is made,
// and False when the budget can't be planned, such as when its target has no control entry.
const ConditionPlanned = "Planned"

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
// +kubebuilder:validation:XValidation:rule="!has(self.for) || duration(self.for) > duration('0s')",message="for must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.for) || !has(self.until) || self.until != 'Released'",message="for can't be combined with until: Released"
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

	// for ends the claim after this long. A set on an arbitrated capability is a claim and must
	// end, with for, until or both; on any other capability it's a plain write, and for and until
	// have no effect.
	// +optional
	For *metav1.Duration `json:"for,omitempty"`

	// until ends the claim when the result without it next changes (Next), or at a release
	// action (Released). With for as well, Next ends the claim at whichever comes first.
	// +optional
	Until ClaimUntil `json:"until,omitempty"`

	// priority is the claim's level. Defaults to Normal.
	// +optional
	Priority RulePriority `json:"priority,omitempty"`
}

// ClaimUntil says what ends a claim.
// +kubebuilder:validation:Enum=Next;Released
type ClaimUntil string

const (
	// ClaimUntilNext ends the claim when the result without it next changes.
	ClaimUntilNext ClaimUntil = "Next"
	// ClaimUntilReleased ends the claim at a release action.
	ClaimUntilReleased ClaimUntil = "Released"
)

// RulePriority is the level of a Rule's claims. A higher level always wins.
// +kubebuilder:validation:Enum=Safety;Override;Normal
type RulePriority string

const (
	RulePrioritySafety   RulePriority = "Safety"
	RulePriorityOverride RulePriority = "Override"
	RulePriorityNormal   RulePriority = "Normal"
)

// ReleaseAction ends another Rule's latched hold, or its until: Released claims.
type ReleaseAction struct {
	// rule names a Rule in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Rule string `json:"rule"`
}

// Action is one step of a Rule. Exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="[has(self.set), has(self.delay), has(self.release)].exists_one(x, x)",message="exactly one of set, delay or release must be specified"
type Action struct {
	// set sets a capability on a device.
	// +optional
	Set *SetAction `json:"set,omitempty"`

	// delay waits before the next action, e.g. 5m.
	// +optional
	Delay *metav1.Duration `json:"delay,omitempty"`

	// release ends another Rule's latched hold, or its until: Released claims.
	// +optional
	Release *ReleaseAction `json:"release,omitempty"`
}

// WindowTime is a window's start or end: a local time as "HH:MM", or a sun event with an
// offset, as {sun: Sunset, offset: -15m}.
type WindowTime struct {
	// Time is the local time, HH:MM. It is written as a bare string. A value that is neither a
	// string nor a sun event is kept here as JSON, for the operator to report.
	Time string `json:"-"`

	// sun is Sunrise or Sunset, from the runtime's --latitude and --longitude.
	Sun SunEvent `json:"sun,omitempty"`

	// offset moves the time from the sun event, e.g. -15m for 15 minutes before.
	Offset *metav1.Duration `json:"offset,omitempty"`
}

type windowSun WindowTime

// MarshalJSON writes a time as a bare string.
func (w WindowTime) MarshalJSON() ([]byte, error) {
	if w.Time != "" {
		return json.Marshal(w.Time)
	}
	return json.Marshal(windowSun(w))
}

// UnmarshalJSON reads a bare string as a time, and an object as a sun event. The field has no
// schema, so the API server stores anything. Anything else, or an object with unknown keys,
// becomes a Time the operator reports as invalid, rather than an error that would stop every
// client from listing Rules.
func (w *WindowTime) UnmarshalJSON(data []byte) error {
	*w = WindowTime{}
	b := bytes.TrimSpace(data)
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &w.Time)
	}
	if len(b) > 0 && b[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var sun windowSun
		if err := dec.Decode(&sun); err == nil && sun.Sun != "" {
			*w = WindowTime(sun)
			return nil
		}
	}
	w.Time = string(b)
	return nil
}

// Window is a span of local time each day, which may cross midnight.
type Window struct {
	// start is "HH:MM", or {sun, offset}.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=""
	// +kubebuilder:pruning:PreserveUnknownFields
	// +required
	Start WindowTime `json:"start"`

	// end is "HH:MM", or {sun, offset}. An end at or before the start is the next day.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=""
	// +kubebuilder:pruning:PreserveUnknownFields
	// +required
	End WindowTime `json:"end"`

	// days limits the window to those starting on these days. Empty means every day.
	// +kubebuilder:validation:MaxItems=7
	// +listType=set
	// +optional
	Days []Weekday `json:"days,omitempty"`
}

// MinutesInput is the capability a budget's bands read at planningTime.
type MinutesInput struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Device string `json:"device"`

	// capability is a number capability, e.g. sensor.temperature.
	// +kubebuilder:validation:MinLength=1
	// +required
	Capability string `json:"capability"`

	// maxAge is how old the value may be. An older value counts as missing.
	// +kubebuilder:default="30m"
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
}

// MinutesBand gives the budget for input values below a threshold.
type MinutesBand struct {
	// below is the band's upper bound, e.g. "12" or "-0.5". Only the last band omits it, and it
	// takes every value the others don't.
	// +optional
	Below *resource.Quantity `json:"below,omitempty"`

	// minutes is the day's budget for values in the band.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1440
	// +required
	Minutes int32 `json:"minutes"`
}

// MinutesBands picks the budget from the band an input's value falls in.
type MinutesBands struct {
	// input is the capability read at planningTime.
	// +required
	Input MinutesInput `json:"input"`

	// missingMinutes is the budget when the input hasn't reported, or is older than maxAge.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1440
	// +required
	MissingMinutes int32 `json:"missingMinutes"`

	// ranges ascend by below, and the first that the value is below wins.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +required
	Ranges []MinutesBand `json:"ranges"`
}

// BudgetMinutes is how many minutes a day the hold is active. Exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="has(self.bands) != has(self.expression)",message="exactly one of bands or expression must be specified"
type BudgetMinutes struct {
	// bands picks the minutes from an input's value.
	// +optional
	Bands *MinutesBands `json:"bands,omitempty"`

	// expression is CEL over devices, now and tz, returning the minutes. A fraction is rounded.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Expression string `json:"expression,omitempty"`
}

// BudgetPrefer weights the slots in a window.
type BudgetPrefer struct {
	// window is the span of local time, "HH:MM" to "HH:MM".
	// +required
	Window Window `json:"window"`

	// score is added to each slot the window touches. Negative avoids them.
	// +kubebuilder:validation:Minimum=-100
	// +kubebuilder:validation:Maximum=100
	// +required
	Score int32 `json:"score"`
}

// BudgetForbid keeps the budget out of a window.
type BudgetForbid struct {
	// window is the span of local time, "HH:MM" to "HH:MM".
	// +required
	Window Window `json:"window"`
}

// Budget makes a hold active for a number of minutes a day. At planningTime the runtime plans the
// day in slots: it places the minutes in the best-scoring slots outside forbid windows, within
// the target's control entry's minimumOn, minimumOff and maximumStartsPerDay, and freezes the plan
// for the day.
type Budget struct {
	// planningTime starts each planning day: local time, HH:MM.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +kubebuilder:default="00:00"
	// +optional
	PlanningTime string `json:"planningTime,omitempty"`

	// slotMinutes is the length of the slots the day is planned in.
	// +kubebuilder:validation:Enum=15;30;60
	// +kubebuilder:default=30
	// +optional
	SlotMinutes int32 `json:"slotMinutes,omitempty"`

	// minutes is the day's budget.
	// +required
	Minutes BudgetMinutes `json:"minutes"`

	// prefer weights windows for the budget. Scores of overlapping windows add up.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Prefer []BudgetPrefer `json:"prefer,omitempty"`

	// forbid keeps the budget out of windows.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Forbid []BudgetForbid `json:"forbid,omitempty"`
}

// BudgetBlock is a planned run, from start to end.
type BudgetBlock struct {
	Start metav1.Time `json:"start"`
	End   metav1.Time `json:"end"`
}

// BudgetDay is a planning day's frozen plan, and how much of it has run.
type BudgetDay struct {
	// date is the planning day, YYYY-MM-DD, local time.
	Date string `json:"date"`

	// targetSource is where the day's minutes came from: Bands, Missing (the bands input hadn't
	// reported, or was too old), Expression or None.
	TargetSource string `json:"targetSource"`

	// targetMinutes is the day's budget, rounded up to whole slots.
	TargetMinutes int32 `json:"targetMinutes"`

	// scheduledMinutes is how much of it the plan places.
	ScheduledMinutes int32 `json:"scheduledMinutes"`

	// shortfallMinutes is how much of it doesn't fit, within the forbid windows and the control
	// entry's limits.
	ShortfallMinutes int32 `json:"shortfallMinutes"`

	// blocks are the planned runs.
	// +listType=atomic
	// +optional
	Blocks []BudgetBlock `json:"blocks,omitempty"`

	// runMinutes is how long the target has been on and available today, updated at most every
	// 5 minutes.
	RunMinutes int32 `json:"runMinutes"`

	// starts counts the planned runs the hold has started today.
	Starts int32 `json:"starts"`
}

// While is when a hold is active: all of its conditions together, or a budget on its own.
// +kubebuilder:validation:XValidation:rule="has(self.window) || has(self.when) || has(self.budget)",message="while needs a window, when or budget"
// +kubebuilder:validation:XValidation:rule="!has(self.budget) || !(has(self.window) || has(self.when) || has(self.onDelay) || has(self.offDelay) || (has(self.latch) && self.latch))",message="budget must be the only field of while"
// +kubebuilder:validation:XValidation:rule="!has(self.latch) || !self.latch || has(self.when)",message="latch needs when"
// +kubebuilder:validation:XValidation:rule="!has(self.onDelay) || duration(self.onDelay) >= duration('0s')",message="onDelay must not be negative"
// +kubebuilder:validation:XValidation:rule="!has(self.offDelay) || duration(self.offDelay) >= duration('0s')",message="offDelay must not be negative"
type While struct {
	// window is a span of local time each day.
	// +optional
	Window *Window `json:"window,omitempty"`

	// when is a CEL expression over devices, now and tz, as in a Rule's condition.
	// For example: devices["bathroom-sensor"]["sensor.relative-humidity"] > 70
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	When string `json:"when,omitempty"`

	// onDelay is how long the rest must stay true before the hold starts.
	// +optional
	OnDelay *metav1.Duration `json:"onDelay,omitempty"`

	// offDelay is how long the hold stays active after the rest stops being true.
	// +optional
	OffDelay *metav1.Duration `json:"offDelay,omitempty"`

	// latch keeps the hold active once when has been true, until a release action.
	// +optional
	Latch bool `json:"latch,omitempty"`

	// budget makes the hold active for a number of minutes a day, in planned slots. It holds one
	// Device's bool capability on, and that capability needs a control entry.
	// +optional
	Budget *Budget `json:"budget,omitempty"`
}

// Hold is the value a hold keeps a capability at while it's active.
// +kubebuilder:validation:XValidation:rule="has(self.device) != has(self.room)",message="exactly one of device or room must be specified"
type Hold struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Device string `json:"device,omitempty"`

	// room names a Room in the same namespace. Every discovered Device in it that reports the
	// capability is held.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Room string `json:"room,omitempty"`

	// capability is the writable capability to hold, e.g. switch.on.
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

// RuleSpec is a moment or a hold. A moment has triggers: when one fires and the condition holds,
// it runs the actions. A hold has while: while that is true, it keeps a capability at a value.
// +kubebuilder:validation:XValidation:rule="has(self.triggers) != has(self.while)",message="exactly one of triggers or while must be specified"
// +kubebuilder:validation:XValidation:rule="has(self.triggers) == has(self.do)",message="do is required with triggers, and only with triggers"
// +kubebuilder:validation:XValidation:rule="has(self.while) == has(self.hold)",message="hold is required with while, and only with while"
// +kubebuilder:validation:XValidation:rule="!has(self.priority) || has(self.while)",message="priority applies to a hold; a set action takes its own"
// +kubebuilder:validation:XValidation:rule="!has(self.condition) || has(self.triggers)",message="condition applies to triggers; a hold uses while.when"
// +kubebuilder:validation:XValidation:rule="!has(self.startingDeadline) || has(self.triggers)",message="startingDeadline applies to triggers"
// +kubebuilder:validation:XValidation:rule="!has(self.while) || !has(self.while.budget) || (has(self.hold) && has(self.hold.device))",message="a budget holds a device, not a room"
type RuleSpec struct {
	// triggers are the moments that start the Rule.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Triggers []Trigger `json:"triggers,omitempty"`

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
	// +optional
	Do []Action `json:"do,omitempty"`

	// mode decides what happens when a trigger fires while a run is in progress.
	// +kubebuilder:default=Single
	// +optional
	Mode RuleMode `json:"mode,omitempty"`

	// startingDeadline is how late an at or sun trigger may still fire after the runtime was
	// down at its time. A later one is skipped and counted as missed. Unset skips every missed
	// time.
	// +optional
	StartingDeadline *metav1.Duration `json:"startingDeadline,omitempty"`

	// while is when the hold is active: a window, a when expression, or both, or a budget.
	// +optional
	While *While `json:"while,omitempty"`

	// hold is the capability the Rule keeps at a value while it's active.
	// +optional
	Hold *Hold `json:"hold,omitempty"`

	// priority is the level of the hold's claim on an arbitrated capability. Defaults to Normal.
	// +optional
	Priority RulePriority `json:"priority,omitempty"`
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

	// active is true while a hold is active.
	// +optional
	Active bool `json:"active,omitempty"`

	// nextChange is when a hold next starts or ends, if that's known from the clock.
	// +optional
	NextChange metav1.Time `json:"nextChange,omitzero"`

	// today is a budget hold's plan for the current planning day, and its progress. The runtime
	// writes it, and reads it back after a restart the same day rather than planning again.
	// +optional
	Today *BudgetDay `json:"today,omitempty"`

	// skipped lists the Room members a set or hold left out, because the capability is
	// arbitrated on them and the set has no end.
	// +listType=atomic
	// +optional
	Skipped []string `json:"skipped,omitempty"`

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
// +kubebuilder:printcolumn:name="Active",type=boolean,JSONPath=`.status.active`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last Triggered",type=date,JSONPath=`.status.lastTriggered`
// +kubebuilder:printcolumn:name="Next Fire",type=date,JSONPath=`.status.nextFire`
// +kubebuilder:printcolumn:name="Budget",type=integer,JSONPath=`.status.today.targetMinutes`,priority=1
// +kubebuilder:printcolumn:name="Run",type=integer,JSONPath=`.status.today.runMinutes`,priority=1

// Rule is run by the Oikade runtime. A moment runs its actions when a trigger fires and the
// condition holds; a hold keeps a capability at a value while its while is true.
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
