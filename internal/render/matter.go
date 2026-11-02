package render

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/node"
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

// MatterRBAC lets the bridge pod read what it exposes. It never writes to the API, and reads no
// Secrets: its own is mounted.
func MatterRBAC(b *matterv1alpha1.Bridge) (
	*corev1ac.ServiceAccountApplyConfiguration, *rbacv1ac.RoleApplyConfiguration, *rbacv1ac.RoleBindingApplyConfiguration,
) {
	name := MatterName(b)
	read := []string{"get", "list", "watch"}
	sa := corev1ac.ServiceAccount(name, b.Namespace).WithLabels(MatterLabels(b)).WithOwnerReferences(matterOwner(b))
	role := rbacv1ac.Role(name, b.Namespace).WithLabels(MatterLabels(b)).WithOwnerReferences(matterOwner(b)).
		WithRules(
			rbacv1ac.PolicyRule().WithAPIGroups(matterv1alpha1.GroupVersion.Group).WithResources("bridges").WithVerbs(read...),
			rbacv1ac.PolicyRule().WithAPIGroups(homev1alpha1.GroupVersion.Group).WithResources("devices").WithVerbs(read...),
			rbacv1ac.PolicyRule().WithAPIGroups(zigbeev1alpha1.GroupVersion.Group).WithResources("zigbee2mqtts").WithVerbs(read...),
			rbacv1ac.PolicyRule().WithAPIGroups(pluginsv1alpha1.GroupVersion.Group).WithResources("plugins").WithVerbs(read...),
		)
	rb := rbacv1ac.RoleBinding(name, b.Namespace).WithLabels(MatterLabels(b)).WithOwnerReferences(matterOwner(b)).
		WithRoleRef(rbacv1ac.RoleRef().WithAPIGroup(rbacv1.GroupName).WithKind("Role").WithName(name)).
		WithSubjects(rbacv1ac.Subject().WithKind(rbacv1.ServiceAccountKind).WithName(name).WithNamespace(b.Namespace))
	return sa, role, rb
}

// MatterInput is everything needed to render a Bridge's Deployment.
type MatterInput struct {
	Bridge  *matterv1alpha1.Bridge
	Image   string
	MQTTURL string
	// ConfigHash covers inputs the pod only reads at start (port, name, setup code).
	ConfigHash string
}

// MatterDeployment runs the Matter bridge for a Bridge. It uses the host network because
// controllers find it over mDNS and reach it over IPv6. Recreate guarantees two pods never serve
// the same fabrics.
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
		WithArgs("--bridge="+b.Name, "--mqtt-url="+in.MQTTURL, "--name="+b.Title(), "--port="+strconv.Itoa(int(port)),
			"--data-dir="+MatterDataPath, "--setup-dir="+MatterSetupPath).
		WithEnv(corev1ac.EnvVar().WithName("POD_NAMESPACE").WithValueFrom(corev1ac.EnvVarSource().
			WithFieldRef(corev1ac.ObjectFieldSelector().WithFieldPath("metadata.namespace")))).
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
		WithServiceAccountName(MatterName(b)).
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

// MatterConfigHash hashes the mounted Secret keys, which the bridge pod reads only at start. The
// rest of what it reads is in its arguments.
func MatterConfigHash(secret map[string][]byte) string {
	var b []byte
	for _, k := range MatterSetupKeys {
		b = append(b, secret[k]...)
		b = append(b, '/')
	}
	return Hash(b)
}

// MatterSetup reads what a bridge needs from its Secret's keys, through get: the setup code, the
// unique ID and the attestation. The operator checks a Secret with it, and the pod reads its
// mounted files with it.
func MatterSetup(get func(key string) ([]byte, error)) (node.Config, error) {
	var cfg node.Config
	value := func(key string) (string, error) {
		b, err := get(key)
		if err != nil {
			return "", err
		}
		if len(b) == 0 {
			return "", fmt.Errorf("%s is empty", key)
		}
		return strings.TrimSpace(string(b)), nil
	}
	number := func(key string, bits int) (uint64, error) {
		v, err := value(key)
		if err != nil {
			return 0, err
		}
		n, err := strconv.ParseUint(v, 10, bits)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		return n, nil
	}
	passcode, err := number(MatterPasscodeKey, 32)
	if err != nil {
		return cfg, err
	}
	discriminator, err := number(MatterDiscriminatorKey, 16)
	if err != nil {
		return cfg, err
	}
	cfg.Setup = node.Setup{Passcode: uint32(passcode), Discriminator: uint16(discriminator)}
	if !cfg.Setup.Valid() {
		return cfg, errors.New("invalid passcode or discriminator")
	}
	if cfg.Info.UniqueID, err = value(MatterUniqueIDKey); err != nil {
		return cfg, err
	}
	if len(cfg.Info.UniqueID) > 32 {
		return cfg, fmt.Errorf("%s is over 32 characters", MatterUniqueIDKey)
	}
	dac, err := get(MatterDACKey)
	if err != nil {
		return cfg, err
	}
	key, err := get(MatterDACPrivateKey)
	if err != nil {
		return cfg, err
	}
	cfg.Attestation, err = attestation.WithDAC(dac, key)
	return cfg, err
}
