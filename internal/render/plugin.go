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
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// Paths and keys of the oikade/v1 plugin environment (docs/plugin-contract.md).
const (
	PluginDevicesKey = "devices.json"

	pluginConfigPath = "/etc/oikade/config"
	pluginSecretPath = "/etc/oikade/secrets"
	// pluginUserID is the node user in the official Node images.
	pluginUserID = 1000
)

// AnnotationSecretVersion on a plugin pod template rolls the pod when the user's Secret changes,
// so plugins that read their keys at start pick up rotated ones.
const AnnotationSecretVersion = "plugins.oikade.dev/secret-version"

// Plugin objects carry a -plugin suffix so they can't collide with a Zigbee2MQTT of the same name.
func PluginDeploymentName(p *pluginsv1alpha1.Plugin) string   { return p.Name + "-plugin" }
func PluginConfigSecretName(p *pluginsv1alpha1.Plugin) string { return p.Name + "-plugin-config" }

// PluginLabels are applied to everything created for a Plugin.
func PluginLabels(p *pluginsv1alpha1.Plugin) map[string]string {
	return labels("oikade-plugin", p.Name, pluginsv1alpha1.LabelPlugin)
}

// PluginConfig renders devices.json. Devices must be sorted so the output, and so the hash, is stable.
func PluginConfig(devices []contractv1.DeclaredDevice) ([]byte, error) {
	if devices == nil {
		devices = []contractv1.DeclaredDevice{}
	}
	return json.Marshal(contractv1.DevicesFile{Contract: contractv1.Contract, Devices: devices})
}

func pluginOwner(p *pluginsv1alpha1.Plugin) *metav1ac.OwnerReferenceApplyConfiguration {
	return ownerRef(pluginsv1alpha1.GroupVersion.WithKind("Plugin"), p)
}

// PluginConfigSecret holds the rendered devices.json. It's a Secret because parameters may turn out
// to be sensitive.
func PluginConfigSecret(p *pluginsv1alpha1.Plugin, config []byte) *corev1ac.SecretApplyConfiguration {
	return corev1ac.Secret(PluginConfigSecretName(p), p.Namespace).
		WithLabels(PluginLabels(p)).
		WithOwnerReferences(pluginOwner(p)).
		WithData(map[string][]byte{PluginDevicesKey: config})
}

// PluginInput is everything needed to render a plugin's Deployment.
type PluginInput struct {
	Plugin  *pluginsv1alpha1.Plugin
	MQTTURL string
	// ConfigHash is the hash of devices.json.
	ConfigHash string
	// SecretVersion is the resourceVersion of the user's Secret, empty when there is none.
	SecretVersion string
}

// PluginDeployment runs a plugin. Recreate guarantees only one pod ever answers /set.
// The user's Secret is optional so a missing one shows as per-device errors rather than a stuck pod.
func PluginDeployment(in PluginInput) *appsv1ac.DeploymentApplyConfiguration {
	p := in.Plugin
	env := []*corev1ac.EnvVarApplyConfiguration{
		corev1ac.EnvVar().WithName(contractv1.EnvMQTTURL).WithValue(in.MQTTURL),
		corev1ac.EnvVar().WithName(contractv1.EnvTopic).WithValue(p.Topic()),
		corev1ac.EnvVar().WithName(contractv1.EnvDevicesFile).WithValue(pluginConfigPath + "/" + PluginDevicesKey),
	}
	mounts := []*corev1ac.VolumeMountApplyConfiguration{
		corev1ac.VolumeMount().WithName("config").WithMountPath(pluginConfigPath).WithReadOnly(true),
		corev1ac.VolumeMount().WithName("tmp").WithMountPath("/tmp"),
	}
	volumes := []*corev1ac.VolumeApplyConfiguration{
		corev1ac.Volume().WithName("config").WithSecret(
			corev1ac.SecretVolumeSource().WithSecretName(PluginConfigSecretName(p))),
		corev1ac.Volume().WithName("tmp").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
	}
	if ref := p.Spec.SecretRef; ref != nil {
		env = append(env, corev1ac.EnvVar().WithName(contractv1.EnvSecretsDir).WithValue(pluginSecretPath))
		mounts = append(mounts, corev1ac.VolumeMount().WithName("secrets").WithMountPath(pluginSecretPath).WithReadOnly(true))
		volumes = append(volumes, corev1ac.Volume().WithName("secrets").WithSecret(
			corev1ac.SecretVolumeSource().WithSecretName(ref.Name).WithOptional(true)))
	}

	main := corev1ac.Container().
		WithName("plugin").
		WithImage(p.Spec.Image).
		WithEnv(env...).
		WithVolumeMounts(mounts...).
		WithResources(resources(p.Spec.Resources)).
		WithSecurityContext(containerSecurityContext().WithReadOnlyRootFilesystem(true))

	pod := corev1ac.PodSpec().
		WithAutomountServiceAccountToken(false).
		WithSecurityContext(podSecurityContext(pluginUserID)).
		WithContainers(main).
		WithVolumes(volumes...)
	if p.Spec.HostNetwork {
		pod.WithHostNetwork(true).WithDNSPolicy(corev1.DNSClusterFirstWithHostNet)
	}
	schedule(pod, p.Spec.NodeSelector, p.Spec.Tolerations)

	annotations := map[string]string{AnnotationConfigHash: in.ConfigHash}
	if in.SecretVersion != "" {
		annotations[AnnotationSecretVersion] = in.SecretVersion
	}
	selector := map[string]string{pluginsv1alpha1.LabelPlugin: p.Name}
	return appsv1ac.Deployment(PluginDeploymentName(p), p.Namespace).
		WithLabels(PluginLabels(p)).
		WithOwnerReferences(pluginOwner(p)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(1).
			WithStrategy(appsv1ac.DeploymentStrategy().WithType(appsv1.RecreateDeploymentStrategyType)).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selector)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(PluginLabels(p)).
				WithAnnotations(annotations).
				WithSpec(pod)))
}
