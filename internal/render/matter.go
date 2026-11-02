package render

import (
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
)

// The Matter bridge Secret's keys. The role reads the setup ones from files of the same names.
const (
	MatterPasscodeKey      = "passcode"
	MatterDiscriminatorKey = "discriminator"
	MatterUniqueIDKey      = "unique-id"
	MatterDACKey           = "dac.pem"
	MatterDACPrivateKey    = "dac-key.pem"
	// MatterEndpointsKey backs up Bridge.status.endpoints, so a Bridge that's deleted and
	// applied again gives its Devices the same endpoint IDs. The role doesn't read it.
	MatterEndpointsKey = "endpoints.json"
)

// MatterSetupKeys are the Secret keys mounted into the bridge pod.
var MatterSetupKeys = []string{MatterPasscodeKey, MatterDiscriminatorKey, MatterUniqueIDKey, MatterDACKey, MatterDACPrivateKey}

const (
	// MatterDataPath is where the bridge pod keeps its fabric table.
	MatterDataPath = "/data"
	// MatterSetupPath is where the bridge pod reads its setup code and attestation.
	MatterSetupPath = "/etc/oikade/matter"

	matterUserID = 65532 // distroless "nonroot"
)

// MatterName names a Bridge's Deployment, Secret and PVC.
func MatterName(b *matterv1alpha1.Bridge) string { return b.Name + "-matter" }

// MatterLabels are applied to everything created for a Matter Bridge.
func MatterLabels(b *matterv1alpha1.Bridge) map[string]string {
	return labels("oikade-matter", b.Name, matterv1alpha1.LabelBridge)
}

func matterOwner(b *matterv1alpha1.Bridge) *metav1ac.OwnerReferenceApplyConfiguration {
	return ownerRef(matterv1alpha1.GroupVersion.WithKind("Bridge"), b)
}

// MatterSecret holds a Bridge's setup code, unique ID and attestation, and a backup of its
// endpoint IDs. It deliberately has no owner reference: a new setup code or DAC doesn't unpair
// the bridge, but losing the endpoint IDs renumbers its Devices.
func MatterSecret(b *matterv1alpha1.Bridge, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: MatterName(b), Namespace: b.Namespace, Labels: MatterLabels(b)},
		Data:       data,
	}
}

// MatterPVC holds the fabric table: the operational keys and certificates and the ACLs. Like
// the Secret it has no owner: losing it means commissioning the bridge again, and controllers
// forget every room, scene and automation.
func MatterPVC(b *matterv1alpha1.Bridge) *corev1.PersistentVolumeClaim {
	size := b.Spec.Storage.Size
	if size.IsZero() {
		size = resource.MustParse("100Mi")
	}
	return pvc(MatterName(b), b.Namespace, MatterLabels(b), b.Spec.Storage.StorageClassName, size)
}

// MatterInput is everything needed to render a Bridge's Deployment.
type MatterInput struct {
	Bridge *matterv1alpha1.Bridge
	Image  string
	// ConfigHash covers inputs the pod only reads at start (port, name, setup code).
	ConfigHash string
}

// MatterDeployment runs the Matter bridge for a Bridge. It uses the host network because
// controllers find it over mDNS and reach it over IPv6. Recreate guarantees two pods never serve
// the same fabrics. The pod doesn't use the Kubernetes API.
func MatterDeployment(in MatterInput) *appsv1ac.DeploymentApplyConfiguration {
	b := in.Bridge
	port := b.Status.Port
	items := make([]*corev1ac.KeyToPathApplyConfiguration, 0, len(MatterSetupKeys))
	for _, k := range MatterSetupKeys {
		items = append(items, corev1ac.KeyToPath().WithKey(k).WithPath(k))
	}
	c := corev1ac.Container().
		WithName("matter").
		WithImage(in.Image).
		WithImagePullPolicy(corev1.PullIfNotPresent).
		WithCommand("/manager", "matter").
		WithArgs("--bridge="+b.Namespace+"/"+b.Name, "--name="+b.Title(), "--port="+strconv.Itoa(int(port)),
			"--data-dir="+MatterDataPath, "--setup-dir="+MatterSetupPath).
		WithPorts(corev1ac.ContainerPort().WithName("matter").WithProtocol(corev1.ProtocolUDP).
			WithContainerPort(port).WithHostPort(port)).
		WithVolumeMounts(
			corev1ac.VolumeMount().WithName("data").WithMountPath(MatterDataPath),
			corev1ac.VolumeMount().WithName("setup").WithMountPath(MatterSetupPath).WithReadOnly(true)).
		WithResources(corev1ac.ResourceRequirements().
			WithRequests(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			}).
			WithLimits(corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")})).
		WithSecurityContext(containerSecurityContext())

	pod := corev1ac.PodSpec().
		WithHostNetwork(true).
		WithDNSPolicy(corev1.DNSClusterFirstWithHostNet).
		WithAutomountServiceAccountToken(false).
		WithSecurityContext(podSecurityContext(matterUserID)).
		WithContainers(c).
		WithVolumes(
			corev1ac.Volume().WithName("data").WithPersistentVolumeClaim(
				corev1ac.PersistentVolumeClaimVolumeSource().WithClaimName(MatterName(b))),
			corev1ac.Volume().WithName("setup").WithSecret(
				corev1ac.SecretVolumeSource().WithSecretName(MatterName(b)).WithItems(items...).WithDefaultMode(0o440)))
	schedule(pod, b.Spec.NodeSelector, b.Spec.Tolerations)

	selector := map[string]string{matterv1alpha1.LabelBridge: b.Name}
	return appsv1ac.Deployment(MatterName(b), b.Namespace).
		WithLabels(MatterLabels(b)).
		WithOwnerReferences(matterOwner(b)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(1).
			WithStrategy(appsv1ac.DeploymentStrategy().WithType(appsv1.RecreateDeploymentStrategyType)).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selector)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(MatterLabels(b)).
				WithAnnotations(map[string]string{AnnotationConfigHash: in.ConfigHash}).
				WithSpec(pod)))
}

// MatterConfigHash hashes the inputs the bridge pod reads only at start: its port, name and the
// mounted Secret keys.
func MatterConfigHash(port int32, name string, secret map[string][]byte) string {
	b := []byte(strconv.Itoa(int(port)) + "/" + name)
	for _, k := range MatterSetupKeys {
		b = append(b, '/')
		b = append(b, secret[k]...)
	}
	return Hash(b)
}
