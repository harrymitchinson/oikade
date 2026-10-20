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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// RoomSpec defines a room. HomeKit can't receive rooms from a bridge, so rooms label
// metrics and group devices for rules; assign them once in the Home app as well.
type RoomSpec struct {
	// displayName is the human-friendly name. Defaults to the object name.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	DisplayName string `json:"displayName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.displayName`

// Room is a room in the home.
type Room struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the room
	// +optional
	Spec RoomSpec `json:"spec,omitzero"`
}

// +kubebuilder:object:root=true

// RoomList contains a list of Room
type RoomList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Room `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Room{}, &RoomList{})
		return nil
	})
}
