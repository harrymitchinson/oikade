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

package render

import (
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
	homekitv1alpha1 "github.com/harry/oikade/api/homekit/v1alpha1"
	zigbeev1alpha1 "github.com/harry/oikade/api/zigbee/v1alpha1"
)

const (
	// IdentityUsernameKey is the identity Secret key holding the bridge username.
	IdentityUsernameKey = "username"
	// HAPDataPath is where the HAP pod keeps pairings and keys.
	HAPDataPath = "/data"

	hapUserID = 65532 // distroless "nonroot"
)

func IdentitySecretName(b *homekitv1alpha1.Bridge) string { return b.Name + "-identity" }
func HAPPVCName(b *homekitv1alpha1.Bridge) string         { return b.Name + "-hap" }
func HAPServiceAccountName(b *homekitv1alpha1.Bridge) string {
	return b.Name + "-hap"
}

// BridgeLabels are applied to everything created for a Bridge.
func BridgeLabels(b *homekitv1alpha1.Bridge) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "oikade-hap",
		"app.kubernetes.io/instance":   b.Name,
		"app.kubernetes.io/managed-by": ManagedBy,
		homekitv1alpha1.LabelBridge:    b.Name,
	}
}

func bridgeOwner(b *homekitv1alpha1.Bridge) *metav1ac.OwnerReferenceApplyConfiguration {
	return ownerRef(homekitv1alpha1.GroupVersion.WithKind("Bridge"), b)
}

// IdentitySecret holds the bridge username. It deliberately has no owner reference so that
// deleting and recreating the Bridge keeps its HomeKit identity.
func IdentitySecret(b *homekitv1alpha1.Bridge, username string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: IdentitySecretName(b), Namespace: b.Namespace, Labels: BridgeLabels(b)},
		Data:       map[string][]byte{IdentityUsernameKey: []byte(username)},
	}
}

// HAPPVC holds pairings and the bridge's keys. Like the identity Secret it has no owner: losing it
// means re-pairing, and the Home app forgets every room, scene and automation.
func HAPPVC(b *homekitv1alpha1.Bridge) *corev1.PersistentVolumeClaim {
	size := b.Spec.Storage.Size
	if size.IsZero() {
		size = resource.MustParse("100Mi")
	}
	return pvc(HAPPVCName(b), b.Namespace, BridgeLabels(b), b.Spec.Storage.StorageClassName, size)
}

// HAPRBAC lets the HAP pod read what it exposes. It never writes to the API.
func HAPRBAC(b *homekitv1alpha1.Bridge) (
	*corev1ac.ServiceAccountApplyConfiguration, *rbacv1ac.RoleApplyConfiguration, *rbacv1ac.RoleBindingApplyConfiguration,
) {
	name := HAPServiceAccountName(b)
	read := []string{"get", "list", "watch"}
	sa := corev1ac.ServiceAccount(name, b.Namespace).WithLabels(BridgeLabels(b)).WithOwnerReferences(bridgeOwner(b))
	role := rbacv1ac.Role(name, b.Namespace).WithLabels(BridgeLabels(b)).WithOwnerReferences(bridgeOwner(b)).
		WithRules(
			rbacv1ac.PolicyRule().WithAPIGroups(homekitv1alpha1.GroupVersion.Group).WithResources("bridges").WithVerbs(read...),
			rbacv1ac.PolicyRule().WithAPIGroups(homev1alpha1.GroupVersion.Group).WithResources("devices").WithVerbs(read...),
			rbacv1ac.PolicyRule().WithAPIGroups(zigbeev1alpha1.GroupVersion.Group).WithResources("zigbee2mqtts").WithVerbs(read...),
			rbacv1ac.PolicyRule().WithAPIGroups("").WithResources("secrets").
				WithResourceNames(b.Spec.PinSecretRef.Name, IdentitySecretName(b)).WithVerbs("get"),
		)
	rb := rbacv1ac.RoleBinding(name, b.Namespace).WithLabels(BridgeLabels(b)).WithOwnerReferences(bridgeOwner(b)).
		WithRoleRef(rbacv1ac.RoleRef().WithAPIGroup(rbacv1.GroupName).WithKind("Role").WithName(name)).
		WithSubjects(rbacv1ac.Subject().WithKind(rbacv1.ServiceAccountKind).WithName(name).WithNamespace(b.Namespace))
	return sa, role, rb
}

// HAPInput is everything needed to render a Bridge's Deployment.
type HAPInput struct {
	Bridge  *homekitv1alpha1.Bridge
	Image   string
	MQTTURL string
	// ConfigHash covers inputs the pod only reads at start (port, identity, setup code).
	ConfigHash string
}

// HAPDeployment runs the HomeKit accessory server for a Bridge. It uses the host network because
// HomeKit finds bridges over mDNS. Recreate guarantees two pods never advertise the same identity.
func HAPDeployment(in HAPInput) *appsv1ac.DeploymentApplyConfiguration {
	b := in.Bridge
	port := b.Status.Port
	probe := func() *corev1ac.ProbeApplyConfiguration {
		return corev1ac.Probe().WithTCPSocket(corev1ac.TCPSocketAction().WithPort(intstr.FromString("hap")))
	}
	hap := corev1ac.Container().
		WithName("hap").
		WithImage(in.Image).
		WithImagePullPolicy(corev1.PullIfNotPresent).
		WithCommand("/manager", "hap").
		WithArgs("--bridge="+b.Name, "--mqtt-url="+in.MQTTURL, "--data-dir="+HAPDataPath).
		WithEnv(corev1ac.EnvVar().WithName("POD_NAMESPACE").WithValueFrom(corev1ac.EnvVarSource().
			WithFieldRef(corev1ac.ObjectFieldSelector().WithFieldPath("metadata.namespace")))).
		WithPorts(corev1ac.ContainerPort().WithName("hap").WithProtocol(corev1.ProtocolTCP).
			WithContainerPort(port).WithHostPort(port)).
		WithVolumeMounts(corev1ac.VolumeMount().WithName("data").WithMountPath(HAPDataPath)).
		WithStartupProbe(probe().WithPeriodSeconds(5).WithFailureThreshold(60)).
		WithReadinessProbe(probe().WithPeriodSeconds(10)).
		WithLivenessProbe(probe().WithPeriodSeconds(30).WithFailureThreshold(3)).
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
		WithServiceAccountName(HAPServiceAccountName(b)).
		WithSecurityContext(podSecurityContext(hapUserID)).
		WithContainers(hap).
		WithVolumes(corev1ac.Volume().WithName("data").WithPersistentVolumeClaim(
			corev1ac.PersistentVolumeClaimVolumeSource().WithClaimName(HAPPVCName(b))))
	schedule(pod, b.Spec.NodeSelector, b.Spec.Tolerations)

	selector := map[string]string{homekitv1alpha1.LabelBridge: b.Name}
	return appsv1ac.Deployment(b.Name+"-hap", b.Namespace).
		WithLabels(BridgeLabels(b)).
		WithOwnerReferences(bridgeOwner(b)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(1).
			WithStrategy(appsv1ac.DeploymentStrategy().WithType(appsv1.RecreateDeploymentStrategyType)).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selector)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(BridgeLabels(b)).
				WithAnnotations(map[string]string{AnnotationConfigHash: in.ConfigHash}).
				WithSpec(pod)))
}

// HAPConfigHash hashes the inputs the HAP pod reads only at start.
func HAPConfigHash(port int32, username, pin string) string {
	return Hash([]byte(strconv.Itoa(int(port)) + "/" + username + "/" + pin))
}
