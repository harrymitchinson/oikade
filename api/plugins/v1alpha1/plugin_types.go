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

// LabelPlugin is set on objects that belong to a Plugin.
const LabelPlugin = "plugins.oikade.dev/plugin"

// PluginSpec declares a plugin process the operator runs. Devices reference it from spec.plugin.
type PluginSpec struct {
	// image is the plugin container image. It must speak the oikade/v1 contract.
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// hostNetwork runs the plugin on the node's network, for plugins that need mDNS or multicast.
	// +optional
	HostNetwork bool `json:"hostNetwork,omitempty"`

	// secretRef names a Secret mounted read-only at /etc/oikade/secrets. Plugins look keys up by device id.
	// The operator never owns, modifies or deletes it.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// nodeSelector constrains where the plugin runs.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations for the plugin pod.
	// +listType=atomic
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// resources for the plugin container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`
}

// PluginStatus is the observed state of a Plugin.
type PluginStatus struct {
	// devices is how many Devices reference this plugin.
	// +optional
	Devices int32 `json:"devices,omitempty"`

	// configHash is the hash of the rendered device file the running pod was started with.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
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
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Devices",type=integer,JSONPath=`.status.devices`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Plugin is an out-of-process driver managed by the operator. It serves the Devices that reference it
// over the oikade/v1 MQTT contract.
type Plugin struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Plugin
	// +required
	Spec PluginSpec `json:"spec"`

	// status defines the observed state of Plugin
	// +optional
	Status PluginStatus `json:"status,omitzero"`
}

// Topic is the plugin's oikade/v1 topic root.
func (p *Plugin) Topic() string {
	return "oikade/v1/" + p.Namespace + "/" + p.Name
}

// +kubebuilder:object:root=true

// PluginList contains a list of Plugin
type PluginList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Plugin `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Plugin{}, &PluginList{})
		return nil
	})
}
