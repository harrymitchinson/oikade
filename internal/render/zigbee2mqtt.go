package render

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/yaml"

	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
)

// Keys of the Zigbee network Secret.
const (
	NetworkKeyKey    = "networkKey"
	PanIDKey         = "panID"
	ExtendedPanIDKey = "extendedPanID"

	zigbeeConfigKey = "configuration.yaml"
	zigbeeDataPath  = "/app/data"
	zigbeeFrontend  = 8080
	zigbeeUserID    = 1000
)

func ZigbeeNetworkSecretName(z *zigbeev1alpha1.Zigbee2MQTT) string { return z.Name + "-network" }
func ZigbeeConfigSecretName(z *zigbeev1alpha1.Zigbee2MQTT) string  { return z.Name + "-config" }
func ZigbeePVCName(z *zigbeev1alpha1.Zigbee2MQTT) string           { return z.Name + "-data" }

// ZigbeeLabels are applied to everything created for a Zigbee2MQTT instance.
func ZigbeeLabels(z *zigbeev1alpha1.Zigbee2MQTT) map[string]string {
	return labels("zigbee2mqtt", z.Name, zigbeev1alpha1.LabelInstance)
}

// ZigbeeNetwork is the generated identity of a Zigbee network. Losing it means re-pairing every device.
type ZigbeeNetwork struct {
	Key           [16]byte
	PanID         uint16
	ExtendedPanID [8]byte
}

// NewZigbeeNetwork generates a random network identity.
func NewZigbeeNetwork() (ZigbeeNetwork, error) {
	var n ZigbeeNetwork
	var pan [2]byte
	for _, b := range [][]byte{n.Key[:], n.ExtendedPanID[:], pan[:]} {
		if _, err := rand.Read(b); err != nil {
			return n, err
		}
	}
	// 0x0000 and 0xFFFF are reserved.
	n.PanID = uint16(pan[0])<<8 | uint16(pan[1])
	n.PanID = 1 + n.PanID%0xFFFE
	return n, nil
}

// ParseZigbeeNetwork reads a network Secret.
func ParseZigbeeNetwork(data map[string][]byte) (ZigbeeNetwork, error) {
	var n ZigbeeNetwork
	key, err := hex.DecodeString(string(data[NetworkKeyKey]))
	if err != nil || len(key) != len(n.Key) {
		return n, fmt.Errorf("%s must be %d hex-encoded bytes", NetworkKeyKey, len(n.Key))
	}
	ext, err := hex.DecodeString(string(data[ExtendedPanIDKey]))
	if err != nil || len(ext) != len(n.ExtendedPanID) {
		return n, fmt.Errorf("%s must be %d hex-encoded bytes", ExtendedPanIDKey, len(n.ExtendedPanID))
	}
	var pan uint16
	if _, err := fmt.Sscanf(string(data[PanIDKey]), "%d", &pan); err != nil || pan == 0 || pan == 0xFFFF {
		return n, fmt.Errorf("%s must be a number between 1 and 65534", PanIDKey)
	}
	copy(n.Key[:], key)
	copy(n.ExtendedPanID[:], ext)
	n.PanID = pan
	return n, nil
}

// ZigbeeNetworkSecret stores the network identity. It has no owner reference so deleting and
// recreating the Zigbee2MQTT object keeps the network.
func ZigbeeNetworkSecret(z *zigbeev1alpha1.Zigbee2MQTT, n ZigbeeNetwork) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ZigbeeNetworkSecretName(z), Namespace: z.Namespace, Labels: ZigbeeLabels(z)},
		Data: map[string][]byte{
			NetworkKeyKey:    []byte(hex.EncodeToString(n.Key[:])),
			PanIDKey:         fmt.Appendf(nil, "%d", n.PanID),
			ExtendedPanIDKey: []byte(hex.EncodeToString(n.ExtendedPanID[:])),
		},
	}
}

// ZigbeePVC holds the Zigbee database and coordinator backups. Like the network Secret it has no owner.
func ZigbeePVC(z *zigbeev1alpha1.Zigbee2MQTT) *corev1.PersistentVolumeClaim {
	size := z.Spec.Storage.Size
	if size.IsZero() {
		size = resource.MustParse("1Gi")
	}
	return pvc(ZigbeePVCName(z), z.Namespace, ZigbeeLabels(z), z.Spec.Storage.StorageClassName, size)
}

// ZigbeeDevice is a declared device: its IEEE address and the friendly name to give it.
type ZigbeeDevice struct {
	IEEEAddress  string
	FriendlyName string
}

// ZigbeeConfigInput is everything needed to render configuration.yaml.
type ZigbeeConfigInput struct {
	Instance *zigbeev1alpha1.Zigbee2MQTT
	Network  ZigbeeNetwork
	MQTTURL  string
	// Devices must be sorted by IEEE address so the output is stable.
	Devices []ZigbeeDevice
}

type zigbeeConfig struct {
	Version       int                       `json:"version"`
	HomeAssistant map[string]bool           `json:"homeassistant"`
	MQTT          zigbeeMQTT                `json:"mqtt"`
	Serial        zigbeeSerial              `json:"serial"`
	Advanced      zigbeeAdvanced            `json:"advanced"`
	Availability  map[string]bool           `json:"availability"`
	Frontend      zigbeeFrontendConfig      `json:"frontend"`
	Devices       map[string]map[string]any `json:"devices"`
}

type zigbeeMQTT struct {
	BaseTopic string `json:"base_topic"`
	Server    string `json:"server"`
}

type zigbeeSerial struct {
	Port     string `json:"port"`
	Adapter  string `json:"adapter"`
	Baudrate int32  `json:"baudrate,omitempty"`
}

type zigbeeAdvanced struct {
	NetworkKey    []int    `json:"network_key"`
	PanID         int      `json:"pan_id"`
	ExtendedPanID []int    `json:"ext_pan_id"`
	Channel       int32    `json:"channel"`
	LogOutput     []string `json:"log_output"`
}

type zigbeeFrontendConfig struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port,omitempty"`
}

// ZigbeeConfig renders configuration.yaml. Zigbee2MQTT migrates it forward if its schema version is newer.
func ZigbeeConfig(in ZigbeeConfigInput) ([]byte, error) {
	z := in.Instance
	channel := z.Spec.Channel
	if channel == 0 {
		channel = 11
	}
	cfg := zigbeeConfig{
		Version:       4,
		HomeAssistant: map[string]bool{"enabled": false},
		MQTT:          zigbeeMQTT{BaseTopic: z.Topic(), Server: in.MQTTURL},
		Serial:        zigbeeSerial{Port: z.Spec.Serial.Port, Adapter: string(z.Spec.Serial.Adapter), Baudrate: z.Spec.Serial.Baudrate},
		Advanced: zigbeeAdvanced{
			NetworkKey:    ints(in.Network.Key[:]),
			PanID:         int(in.Network.PanID),
			ExtendedPanID: ints(in.Network.ExtendedPanID[:]),
			Channel:       channel,
			LogOutput:     []string{"console"},
		},
		Availability: map[string]bool{"enabled": true},
		Frontend:     zigbeeFrontendConfig{Enabled: z.Spec.Frontend},
		Devices:      map[string]map[string]any{},
	}
	if z.Spec.Frontend {
		cfg.Frontend.Port = zigbeeFrontend
	}
	for _, d := range in.Devices {
		cfg.Devices[d.IEEEAddress] = map[string]any{"friendly_name": d.FriendlyName}
	}
	return yaml.Marshal(cfg)
}

func ints(b []byte) []int {
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = int(v)
	}
	return out
}

func zigbeeOwner(z *zigbeev1alpha1.Zigbee2MQTT) *metav1ac.OwnerReferenceApplyConfiguration {
	return ownerRef(zigbeev1alpha1.GroupVersion.WithKind("Zigbee2MQTT"), z)
}

// ZigbeeConfigSecret holds the rendered configuration.yaml.
func ZigbeeConfigSecret(z *zigbeev1alpha1.Zigbee2MQTT, config []byte) *corev1ac.SecretApplyConfiguration {
	return corev1ac.Secret(ZigbeeConfigSecretName(z), z.Namespace).
		WithLabels(ZigbeeLabels(z)).
		WithOwnerReferences(zigbeeOwner(z)).
		WithData(map[string][]byte{zigbeeConfigKey: config})
}

// ZigbeeService exposes the frontend inside the cluster.
func ZigbeeService(z *zigbeev1alpha1.Zigbee2MQTT) *corev1ac.ServiceApplyConfiguration {
	return corev1ac.Service(z.Name, z.Namespace).
		WithLabels(ZigbeeLabels(z)).
		WithOwnerReferences(zigbeeOwner(z)).
		WithSpec(corev1ac.ServiceSpec().
			WithSelector(map[string]string{zigbeev1alpha1.LabelInstance: z.Name}).
			WithPorts(corev1ac.ServicePort().WithName("http").WithPort(zigbeeFrontend).
				WithTargetPort(intstr.FromString("http"))))
}

// usb reports whether the coordinator is a local device rather than a network coordinator.
func usb(z *zigbeev1alpha1.Zigbee2MQTT) bool {
	return strings.HasPrefix(z.Spec.Serial.Port, "/dev/")
}

// ZigbeeDeployment runs Zigbee2MQTT. An init container copies the rendered configuration.yaml over
// the one on the PVC at every start, so Git wins over changes made in the frontend.
// Recreate guarantees only one pod ever talks to the coordinator.
func ZigbeeDeployment(z *zigbeev1alpha1.Zigbee2MQTT, image, configHash string) *appsv1ac.DeploymentApplyConfiguration {
	data := corev1ac.VolumeMount().WithName("data").WithMountPath(zigbeeDataPath)

	initConfig := corev1ac.Container().
		WithName("config").
		WithImage(image).
		WithCommand("cp", "/config/"+zigbeeConfigKey, zigbeeDataPath+"/"+zigbeeConfigKey).
		WithVolumeMounts(data, corev1ac.VolumeMount().WithName("config").WithMountPath("/config").WithReadOnly(true))

	main := corev1ac.Container().
		WithName("zigbee2mqtt").
		WithImage(image).
		WithEnv(corev1ac.EnvVar().WithName("ZIGBEE2MQTT_DATA").WithValue(zigbeeDataPath)).
		WithVolumeMounts(data).
		WithResources(resources(z.Spec.Resources))
	if z.Spec.Frontend {
		probe := corev1ac.Probe().WithTCPSocket(corev1ac.TCPSocketAction().WithPort(intstr.FromString("http")))
		main.WithPorts(corev1ac.ContainerPort().WithName("http").WithContainerPort(zigbeeFrontend)).
			WithReadinessProbe(probe.WithPeriodSeconds(10))
	}

	volumes := []*corev1ac.VolumeApplyConfiguration{
		corev1ac.Volume().WithName("data").WithPersistentVolumeClaim(
			corev1ac.PersistentVolumeClaimVolumeSource().WithClaimName(ZigbeePVCName(z))),
		corev1ac.Volume().WithName("config").WithSecret(
			corev1ac.SecretVolumeSource().WithSecretName(ZigbeeConfigSecretName(z))),
	}
	pod := corev1ac.PodSpec()
	// Containers are finished before being added: the builders copy them by value.
	if usb(z) {
		// Serial devices are root-owned on most hosts; reaching one needs a privileged container.
		volumes = append(volumes, corev1ac.Volume().WithName("coordinator").WithHostPath(
			corev1ac.HostPathVolumeSource().WithPath(z.Spec.Serial.Port).WithType(corev1.HostPathCharDev)))
		main.WithVolumeMounts(corev1ac.VolumeMount().WithName("coordinator").WithMountPath(z.Spec.Serial.Port)).
			WithSecurityContext(corev1ac.SecurityContext().WithPrivileged(true))
	} else {
		pod.WithSecurityContext(podSecurityContext(zigbeeUserID))
		initConfig.WithSecurityContext(containerSecurityContext())
		main.WithSecurityContext(containerSecurityContext())
	}
	pod.WithInitContainers(initConfig).WithContainers(main).WithVolumes(volumes...)
	schedule(pod, z.Spec.NodeSelector, z.Spec.Tolerations)

	selector := map[string]string{zigbeev1alpha1.LabelInstance: z.Name}
	return appsv1ac.Deployment(z.Name, z.Namespace).
		WithLabels(ZigbeeLabels(z)).
		WithOwnerReferences(zigbeeOwner(z)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(1).
			WithStrategy(appsv1ac.DeploymentStrategy().WithType(appsv1.RecreateDeploymentStrategyType)).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selector)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(ZigbeeLabels(z)).
				WithAnnotations(map[string]string{AnnotationConfigHash: configHash}).
				WithSpec(pod)))
}
