package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Device condition types. The runtime writes Resolved and Available; the operator writes Ready
// from validating the spec.
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

// PluginDeviceSource identifies a device served by a Plugin.
type PluginDeviceSource struct {
	// pluginRef names the Plugin that serves the device.
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="pluginRef.name is required"
	// +required
	PluginRef corev1.LocalObjectReference `json:"pluginRef"`

	// id is the device's stable, plugin-local identifier, such as an eWeLink device ID. It appears in MQTT topics.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`
	// +required
	ID string `json:"id"`

	// parameters are plugin-specific settings. Oikade passes them through unchanged, and the plugin validates them.
	// Keys are up to 63 characters of letters, digits, '_', '.' and '-'; values are up to 1024 characters.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[A-Za-z0-9_.-]{1,63}$'))",message="parameter keys must be 1-63 characters of letters, digits, '_', '.' or '-'"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 1024)",message="parameter values must be at most 1024 characters"
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
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

// ControlResolve picks between claims at the same level. The bool values aren't On and Off,
// because YAML 1.1, which kubectl decodes, reads those unquoted as booleans.
// +kubebuilder:validation:Enum=PreferOn;PreferOff;Highest;Lowest
type ControlResolve string

const (
	// ControlResolvePreferOn prefers true, for bool capabilities.
	ControlResolvePreferOn ControlResolve = "PreferOn"
	// ControlResolvePreferOff prefers false, for bool capabilities.
	ControlResolvePreferOff ControlResolve = "PreferOff"
	// ControlResolveHighest prefers the highest value, for number capabilities.
	ControlResolveHighest ControlResolve = "Highest"
	// ControlResolveLowest prefers the lowest value, for number capabilities.
	ControlResolveLowest ControlResolve = "Lowest"
)

// ControlExternal says what a change from outside the rules does.
// +kubebuilder:validation:Enum=Override;Revert
type ControlExternal string

const (
	// ControlExternalOverride makes an outside change an Override claim for overrideHold.
	ControlExternalOverride ControlExternal = "Override"
	// ControlExternalRevert writes the winner straight back.
	ControlExternalRevert ControlExternal = "Revert"
)

// OverrideHoldNext ends an outside change's claim when the result without it next changes.
const OverrideHoldNext = "Next"

// CapabilityControl makes one capability arbitrated (0060): rules, HomeKit and the device itself
// make claims on it, and the device takes the winner.
// +kubebuilder:validation:XValidation:rule="self.external != 'Override' || has(self.overrideHold)",message="overrideHold is required when external is Override"
// +kubebuilder:validation:XValidation:rule="self.external != 'Revert' || !has(self.overrideHold)",message="overrideHold has no effect when external is Revert"
// +kubebuilder:validation:XValidation:rule="!has(self.overrideHold) || self.overrideHold == 'Next' || duration(self.overrideHold) > duration('0s')",message="overrideHold must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.minimumOn) || duration(self.minimumOn) >= duration('0s')",message="minimumOn must not be negative"
// +kubebuilder:validation:XValidation:rule="!has(self.maximumOn) || duration(self.maximumOn) > duration('0s')",message="maximumOn must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.minimumOff) || duration(self.minimumOff) >= duration('0s')",message="minimumOff must not be negative"
type CapabilityControl struct {
	// capability is the writable capability to arbitrate, e.g. switch.on.
	// +kubebuilder:validation:MinLength=1
	// +required
	Capability string `json:"capability"`

	// default is the value when nobody claims the capability: a bool or number matching its kind.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=""
	// +kubebuilder:pruning:PreserveUnknownFields
	// +required
	Default apiextensionsv1.JSON `json:"default"`

	// resolve picks between claims at the same level: PreferOn or PreferOff for a bool capability,
	// Highest or Lowest for a number.
	// +required
	Resolve ControlResolve `json:"resolve"`

	// external says what a change from outside the rules, through HomeKit or on the device
	// itself, does. Override makes it a claim for overrideHold; Revert writes the winner back.
	// +required
	External ControlExternal `json:"external"`

	// overrideHold is how long an outside change holds as an Override claim: a duration such as
	// 1h, or Next to hold until the result without it next changes.
	// +kubebuilder:validation:Pattern=`^(Next|([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+)$`
	// +kubebuilder:validation:MaxLength=32
	// +optional
	OverrideHold string `json:"overrideHold,omitempty"`

	// minimumOn keeps the device on at least this long once it starts. Equipment limits win over
	// every claim, Safety included.
	// +optional
	MinimumOn *metav1.Duration `json:"minimumOn,omitempty"`

	// maximumOn stops the device after it has been on this long. It then rests for minimumOff.
	// +optional
	MaximumOn *metav1.Duration `json:"maximumOn,omitempty"`

	// minimumOff keeps the device off at least this long once it stops.
	// +optional
	MinimumOff *metav1.Duration `json:"minimumOff,omitempty"`

	// maximumStartsPerDay limits how often the device turns on per local day. Unset is unlimited.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaximumStartsPerDay *int32 `json:"maximumStartsPerDay,omitempty"`
}

// DeviceSpec declares a device that should exist in the home. Exactly one driver source must be set.
// +kubebuilder:validation:XValidation:rule="[has(self.zigbee2mqtt), has(self.plugin)].filter(x, x).size() == 1",message="exactly one driver source (zigbee2mqtt or plugin) is required"
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

	// plugin sources the device from a Plugin.
	// +optional
	Plugin *PluginDeviceSource `json:"plugin,omitempty"`

	// homekit tunes how the device appears in HomeKit.
	// +optional
	HomeKit *DeviceHomeKit `json:"homekit,omitempty"`

	// control lists the capabilities that are arbitrated. Capabilities without an entry keep
	// last-write-wins.
	// +listType=map
	// +listMapKey=capability
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Control []CapabilityControl `json:"control,omitempty"`
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

// ControlLevel orders claims. A higher level always wins, and Default is the device's default
// value when nobody claims.
// +kubebuilder:validation:Enum=Safety;Override;Normal;Default
type ControlLevel string

const (
	ControlLevelSafety   ControlLevel = "Safety"
	ControlLevelOverride ControlLevel = "Override"
	ControlLevelNormal   ControlLevel = "Normal"
	ControlLevelDefault  ControlLevel = "Default"
)

// ControlClaim is one source's claim on an arbitrated capability.
type ControlClaim struct {
	// source is the path the claim came in by: HomeKit, Device or Rule/<name>.
	// +required
	Source string `json:"source"`

	// level is the claim's level.
	// +required
	Level ControlLevel `json:"level"`

	// value is the claimed value.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=""
	// +kubebuilder:pruning:PreserveUnknownFields
	// +required
	Value apiextensionsv1.JSON `json:"value"`

	// expires is when the claim ends, if it ends at a time.
	// +optional
	Expires metav1.Time `json:"expires,omitzero"`

	// latched is true for a latched hold, which lasts until a release action.
	// +optional
	Latched bool `json:"latched,omitempty"`

	// winner is true for the claim the device takes.
	// +optional
	Winner bool `json:"winner,omitempty"`

	// reverted is true for an outside change that was written back.
	// +optional
	Reverted bool `json:"reverted,omitempty"`
}

// CapabilityControlStatus is the state of one arbitrated capability. It changes at transitions,
// not on every value.
type CapabilityControlStatus struct {
	// capability is the arbitrated capability.
	// +required
	Capability string `json:"capability"`

	// value is the value the device was last given.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=""
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`

	// level is the winning claim's level.
	// +optional
	Level ControlLevel `json:"level,omitempty"`

	// claims are the current claims, the winner among them.
	// +listType=atomic
	// +optional
	Claims []ControlClaim `json:"claims,omitempty"`
}

// DeviceStatus is the driver's coarse view of the device. Live values are on the MQTT bus, never here.
type DeviceStatus struct {
	// control is the state of each arbitrated capability, written by the runtime.
	// +listType=map
	// +listMapKey=capability
	// +optional
	Control []CapabilityControlStatus `json:"control,omitempty"`

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

	// conditions: Ready, set by the operator, and Resolved and Available, set by the runtime.
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
