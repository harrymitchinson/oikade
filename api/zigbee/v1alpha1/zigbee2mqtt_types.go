package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

// LabelInstance is set on objects that belong to a Zigbee2MQTT instance, including retained ones
// (network Secret, PVC).
const LabelInstance = "zigbee.oikade.dev/instance"

// Adapter is the Zigbee coordinator firmware family.
// +kubebuilder:validation:Enum=zstack;ember;deconz;zigate;zboss
type Adapter string

// SerialSpec configures the Zigbee coordinator.
type SerialSpec struct {
	// port is the coordinator address: tcp://host:port for a network coordinator (recommended,
	// the pod can run anywhere) or a /dev path for a USB stick (the pod must run on that node,
	// see nodeSelector, and runs privileged to reach the device).
	// +kubebuilder:validation:Pattern=`^(tcp://.+|/dev/.+)$`
	// +required
	Port string `json:"port"`

	// adapter is the coordinator firmware family.
	// +required
	Adapter Adapter `json:"adapter"`

	// baudrate for USB coordinators. Defaults to Zigbee2MQTT's default.
	// +optional
	Baudrate int32 `json:"baudrate,omitempty"`
}

// StorageSpec configures the PersistentVolumeClaim that holds the Zigbee database and coordinator backups.
type StorageSpec struct {
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// +kubebuilder:default="1Gi"
	// +optional
	Size resource.Quantity `json:"size,omitzero"`
}

// Zigbee2MQTTSpec defines a Zigbee2MQTT instance. The operator renders its configuration.yaml from
// this spec and the Devices that reference it, and re-applies it on every start, so changes made in
// the Zigbee2MQTT frontend don't survive a restart.
type Zigbee2MQTTSpec struct {
	// image overrides the operator's default Zigbee2MQTT image.
	// +optional
	Image string `json:"image,omitempty"`

	// serial configures the coordinator.
	// +required
	Serial SerialSpec `json:"serial"`

	// baseTopic is the MQTT topic prefix. Defaults to the object name.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	// +optional
	BaseTopic string `json:"baseTopic,omitempty"`

	// channel is the Zigbee channel. Changing it means re-pairing every device.
	// +kubebuilder:validation:Minimum=11
	// +kubebuilder:validation:Maximum=26
	// +kubebuilder:default=11
	// +optional
	Channel int32 `json:"channel,omitempty"`

	// frontend enables the Zigbee2MQTT web UI (useful for pairing) behind a ClusterIP Service on port 8080.
	// +optional
	Frontend bool `json:"frontend,omitempty"`

	// storage configures the PersistentVolumeClaim.
	// +kubebuilder:default={size: "1Gi"}
	// +optional
	Storage StorageSpec `json:"storage,omitzero"`

	// nodeSelector pins the pod, e.g. to the node with the USB coordinator.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations for the pod.
	// +listType=atomic
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// resources for the Zigbee2MQTT container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`

	// discovery adopts the devices paired in Zigbee2MQTT as Devices. Off unless set.
	// +optional
	Discovery *homev1alpha1.DiscoverySpec `json:"discovery,omitempty"`
}

// Zigbee2MQTTStatus defines the observed state of Zigbee2MQTT.
type Zigbee2MQTTStatus struct {
	// panID is the Zigbee network's PAN ID. It is generated once and kept, so a lost network Secret
	// can be partly recovered; the network key itself only lives in the Secret.
	// +optional
	PanID int32 `json:"panID,omitempty"`

	// devices is the number of Devices declared for this instance.
	// +optional
	Devices int32 `json:"devices,omitempty"`

	// configHash is the hash of the rendered configuration.yaml.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Port",type=string,JSONPath=`.spec.serial.port`
// +kubebuilder:printcolumn:name="Devices",type=integer,JSONPath=`.status.devices`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Zigbee2MQTT is a Zigbee2MQTT instance managed by the operator.
type Zigbee2MQTT struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Zigbee2MQTT
	// +required
	Spec Zigbee2MQTTSpec `json:"spec"`

	// status defines the observed state of Zigbee2MQTT
	// +optional
	Status Zigbee2MQTTStatus `json:"status,omitzero"`
}

// Topic returns the MQTT base topic.
func (z *Zigbee2MQTT) Topic() string {
	if z.Spec.BaseTopic != "" {
		return z.Spec.BaseTopic
	}
	return z.Name
}

// +kubebuilder:object:root=true

// Zigbee2MQTTList contains a list of Zigbee2MQTT
type Zigbee2MQTTList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Zigbee2MQTT `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Zigbee2MQTT{}, &Zigbee2MQTTList{})
		return nil
	})
}
