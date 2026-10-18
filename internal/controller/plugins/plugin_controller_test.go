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

package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harry/oikade/api/plugins/v1alpha1"
	"github.com/harry/oikade/internal/render"
)

var testSeq atomic.Int32

const pluginName = "sonoff"

func newNamespace() string {
	ns := fmt.Sprintf("plugins-%d", testSeq.Add(1))
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	return ns
}

func createPlugin(ns string, mutate func(*pluginsv1alpha1.PluginSpec)) types.NamespacedName {
	p := &pluginsv1alpha1.Plugin{
		ObjectMeta: metav1.ObjectMeta{Name: pluginName, Namespace: ns},
		Spec:       pluginsv1alpha1.PluginSpec{Image: "example.com/oikade-plugin-sonoff:test"},
	}
	if mutate != nil {
		mutate(&p.Spec)
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, p)).To(Succeed())
	return client.ObjectKeyFromObject(p)
}

func createDevice(ns, name, plugin, id string, params map[string]string) {
	ExpectWithOffset(1, k8sClient.Create(ctx, &homev1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: homev1alpha1.DeviceSpec{
			RoomRef: &corev1.LocalObjectReference{Name: "porch"},
			Plugin: &homev1alpha1.PluginDeviceSource{
				PluginRef: corev1.LocalObjectReference{Name: plugin}, ID: id, Parameters: params,
			},
		},
	})).To(Succeed())
}

func reconcilePlugin(r *PluginReconciler, key types.NamespacedName) *pluginsv1alpha1.Plugin {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return get(key, &pluginsv1alpha1.Plugin{})
}

func get[T client.Object](key types.NamespacedName, obj T) T {
	ExpectWithOffset(1, k8sClient.Get(ctx, key, obj)).To(Succeed())
	return obj
}

func depKey(key types.NamespacedName) types.NamespacedName {
	return types.NamespacedName{Namespace: key.Namespace, Name: key.Name + "-plugin"}
}

func cfgKey(key types.NamespacedName) types.NamespacedName {
	return types.NamespacedName{Namespace: key.Namespace, Name: key.Name + "-plugin-config"}
}

func ready(p *pluginsv1alpha1.Plugin) *metav1.Condition {
	return meta.FindStatusCondition(p.Status.Conditions, ConditionReady)
}

// renderedDevices returns the parsed devices.json from the config Secret.
func renderedDevices(key types.NamespacedName) []render.PluginDevice {
	s := get(cfgKey(key), &corev1.Secret{})
	var cfg struct {
		Contract int                   `json:"contract"`
		Devices  []render.PluginDevice `json:"devices"`
	}
	ExpectWithOffset(1, json.Unmarshal(s.Data[render.PluginDevicesKey], &cfg)).To(Succeed())
	ExpectWithOffset(1, cfg.Contract).To(Equal(1))
	return cfg.Devices
}

// markAvailable stands in for the kubelet and deployment controller, which envtest doesn't run.
func markAvailable(key types.NamespacedName) {
	dep := get(depKey(key), &appsv1.Deployment{})
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, dep)).To(Succeed())
}

var _ = Describe("Plugin Controller", func() {
	var r *PluginReconciler

	BeforeEach(func() {
		r = &PluginReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), MQTTURL: "mqtt://broker:1883"}
	})

	It("renders declared Devices and runs the plugin", func() {
		ns := newNamespace()
		key := createPlugin(ns, nil)
		createDevice(ns, "porch-light", pluginName, "1000aabbcc", map[string]string{"capability": "light.on"})
		createDevice(ns, "hall-light", pluginName, "1000000001", nil)
		createDevice(ns, "elsewhere", "other", "1000000002", nil)

		p := reconcilePlugin(r, key)
		Expect(p.Status.Devices).To(Equal(int32(2)))
		Expect(p.Status.ConfigHash).NotTo(BeEmpty())
		Expect(p.Status.ObservedGeneration).To(Equal(p.Generation))
		Expect(renderedDevices(key)).To(Equal([]render.PluginDevice{
			{ID: "1000000001", Name: "hall-light", DisplayName: "hall-light", Room: "porch"},
			{ID: "1000aabbcc", Name: "porch-light", DisplayName: "porch-light", Room: "porch",
				Parameters: map[string]string{"capability": "light.on"}},
		}))
		cfg := get(cfgKey(key), &corev1.Secret{})
		Expect(metav1.IsControlledBy(cfg, p)).To(BeTrue())

		dep := get(depKey(key), &appsv1.Deployment{})
		Expect(metav1.IsControlledBy(dep, p)).To(BeTrue())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(render.AnnotationConfigHash, p.Status.ConfigHash))
		Expect(dep.Spec.Template.Labels).To(HaveKeyWithValue(pluginsv1alpha1.LabelPlugin, pluginName))
		Expect(dep.Spec.Template.Spec.HostNetwork).To(BeFalse())
		Expect(*dep.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("example.com/oikade-plugin-sonoff:test"))
		Expect(dep.Spec.Template.Spec.Containers[0].Env).To(ContainElement(
			corev1.EnvVar{Name: "OIKADE_TOPIC", Value: "oikade/v1/" + ns + "/" + pluginName}))

		Expect(ready(p).Status).To(Equal(metav1.ConditionFalse))
		Expect(ready(p).Reason).To(Equal("Progressing"))

		By("becoming available")
		markAvailable(key)
		p = reconcilePlugin(r, key)
		Expect(ready(p).Status).To(Equal(metav1.ConditionTrue))
		Expect(ready(p).Reason).To(Equal("Available"))

		By("changing a Device, which changes the config hash and so restarts the pod")
		d := get(types.NamespacedName{Namespace: ns, Name: "hall-light"}, &homev1alpha1.Device{})
		d.Spec.DisplayName = "Hall light"
		Expect(k8sClient.Update(ctx, d)).To(Succeed())
		p2 := reconcilePlugin(r, key)
		Expect(p2.Status.ConfigHash).NotTo(Equal(p.Status.ConfigHash))
		Expect(get(depKey(key), &appsv1.Deployment{}).Spec.Template.Annotations).
			To(HaveKeyWithValue(render.AnnotationConfigHash, p2.Status.ConfigHash))
		Expect(renderedDevices(key)[0].DisplayName).To(Equal("Hall light"))

		By("reconciling again without changes, which keeps the hash")
		Expect(reconcilePlugin(r, key).Status.ConfigHash).To(Equal(p2.Status.ConfigHash))
	})

	It("runs on the host network when asked", func() {
		ns := newNamespace()
		key := createPlugin(ns, func(s *pluginsv1alpha1.PluginSpec) { s.HostNetwork = true })
		reconcilePlugin(r, key)
		pod := get(depKey(key), &appsv1.Deployment{}).Spec.Template.Spec
		Expect(pod.HostNetwork).To(BeTrue())
		Expect(pod.DNSPolicy).To(Equal(corev1.DNSClusterFirstWithHostNet))
	})

	It("mounts the user's Secret and reports it missing without taking ownership", func() {
		ns := newNamespace()
		key := createPlugin(ns, func(s *pluginsv1alpha1.PluginSpec) {
			s.SecretRef = &corev1.LocalObjectReference{Name: "sonoff-lan-keys"}
		})

		p := reconcilePlugin(r, key)
		Expect(ready(p).Reason).To(Equal("SecretNotFound"))
		pod := get(depKey(key), &appsv1.Deployment{}).Spec.Template.Spec
		Expect(pod.Volumes).To(ContainElement(corev1.Volume{Name: "secrets", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: "sonoff-lan-keys", Optional: new(true), DefaultMode: new(int32(0o644)),
			},
		}}))
		Expect(pod.Containers[0].Env).To(ContainElement(
			corev1.EnvVar{Name: "OIKADE_SECRETS_DIR", Value: "/etc/oikade/secrets"}))

		By("creating the Secret")
		keys := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sonoff-lan-keys", Namespace: ns},
			StringData: map[string]string{"1000aabbcc": "key"},
		}
		Expect(k8sClient.Create(ctx, keys)).To(Succeed())
		Expect(r.pluginsForSecret(ctx, keys)).To(ConsistOf(reconcile.Request{NamespacedName: key}))
		// The new secret version rolls the pod.
		Expect(ready(reconcilePlugin(r, key)).Reason).To(Equal("Progressing"))
		markAvailable(key)
		p = reconcilePlugin(r, key)
		Expect(ready(p).Reason).To(Equal("Available"))
		Expect(get(client.ObjectKeyFromObject(keys), &corev1.Secret{}).OwnerReferences).To(BeEmpty())
		version := get(depKey(key), &appsv1.Deployment{}).Spec.Template.Annotations[render.AnnotationSecretVersion]
		Expect(version).NotTo(BeEmpty())

		By("rotating the Secret, which restarts the pod")
		keys = get(client.ObjectKeyFromObject(keys), &corev1.Secret{})
		keys.Data["1000aabbcc"] = []byte("rotated")
		Expect(k8sClient.Update(ctx, keys)).To(Succeed())
		reconcilePlugin(r, key)
		Expect(get(depKey(key), &appsv1.Deployment{}).Spec.Template.Annotations).
			To(HaveKeyWithValue(render.AnnotationSecretVersion, Not(Equal(version))))
	})

	It("refuses to take over objects it doesn't own", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: pluginName + "-plugin-config", Namespace: ns},
			StringData: map[string]string{"mine": "yes"},
		})).To(Succeed())
		key := createPlugin(ns, nil)

		p := reconcilePlugin(r, key)
		Expect(ready(p).Reason).To(Equal("NameConflict"))
		Expect(get(cfgKey(key), &corev1.Secret{}).Data).To(Equal(map[string][]byte{"mine": []byte("yes")}))
		Expect(k8sClient.Get(ctx, depKey(key), &appsv1.Deployment{})).NotTo(Succeed())
	})

	It("refuses a secretRef naming its own config Secret", func() {
		ns := newNamespace()
		key := createPlugin(ns, func(s *pluginsv1alpha1.PluginSpec) {
			s.SecretRef = &corev1.LocalObjectReference{Name: pluginName + "-plugin-config"}
		})
		p := reconcilePlugin(r, key)
		Expect(ready(p).Reason).To(Equal("NameConflict"))
		Expect(k8sClient.Get(ctx, cfgKey(key), &corev1.Secret{})).NotTo(Succeed())
	})

	It("flags an id claimed by two Devices, keeping the first by name", func() {
		ns := newNamespace()
		key := createPlugin(ns, nil)
		createDevice(ns, "b-light", pluginName, "1000aabbcc", nil)
		createDevice(ns, "a-light", pluginName, "1000aabbcc", nil)
		reconcilePlugin(r, key)
		markAvailable(key)

		// A running pod doesn't hide the conflict.
		p := reconcilePlugin(r, key)
		Expect(ready(p).Status).To(Equal(metav1.ConditionFalse))
		Expect(ready(p).Reason).To(Equal("DuplicateDevice"))
		Expect(ready(p).Message).To(ContainSubstring("1000aabbcc"))
		Expect(p.Status.Devices).To(Equal(int32(1)))
		devices := renderedDevices(key)
		Expect(devices).To(HaveLen(1))
		Expect(devices[0].Name).To(Equal("a-light"))
	})

	It("accepts the Plugin and plugin-backed Device samples", func() {
		ns := newNamespace()
		var plugins, devices int
		for _, file := range []string{"plugins_v1alpha1_plugin.yaml", "home_v1alpha1_device.yaml"} {
			f, err := os.Open(filepath.Join("..", "..", "..", "config", "samples", file))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.Close)
			dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
			for {
				var u unstructured.Unstructured
				if err := dec.Decode(&u.Object); errors.Is(err, io.EOF) {
					break
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
				switch {
				case u.GetKind() == "Plugin":
					plugins++
				case u.GetKind() == "Device" && u.Object["spec"].(map[string]any)["plugin"] != nil:
					devices++
				default:
					continue
				}
				u.SetNamespace(ns)
				Expect(k8sClient.Create(ctx, &u, client.DryRunAll)).To(Succeed(), "%s %s", u.GetKind(), u.GetName())
			}
		}
		Expect(plugins).To(Equal(1))
		Expect(devices).To(BeNumerically(">=", 1))
	})

	It("maps Devices to their Plugin", func() {
		d := &homev1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "home"}}
		Expect(pluginForDevice(ctx, d)).To(BeEmpty())
		d.Spec.Plugin = &homev1alpha1.PluginDeviceSource{PluginRef: corev1.LocalObjectReference{Name: pluginName}, ID: "1"}
		Expect(pluginForDevice(ctx, d)).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "home", Name: pluginName},
		}))
	})
})
