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

package homekit

import (
	"fmt"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	homekitv1alpha1 "github.com/harrymitchinson/oikade/api/homekit/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/identity"
	"github.com/harrymitchinson/oikade/internal/render"
)

// Ports are unique cluster-wide, so every test gets its own namespace and port range.
var testSeq atomic.Int32

type testNS struct {
	ns    string
	start int32
}

func newTestNS() testNS {
	n := testSeq.Add(1)
	t := testNS{ns: fmt.Sprintf("homekit-%d", n), start: 53000 + n*10}
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: t.ns}})).To(Succeed())
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pin", Namespace: t.ns},
		StringData: map[string]string{"pin": "031-45-154"},
	})).To(Succeed())
	return t
}

func (t testNS) createBridge(name string, size int32, mutate ...func(*homekitv1alpha1.Bridge)) {
	b := &homekitv1alpha1.Bridge{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: t.ns},
		Spec: homekitv1alpha1.BridgeSpec{
			PinSecretRef: homekitv1alpha1.SecretKeyRef{Name: "pin", Key: "pin"},
			PortRange:    homekitv1alpha1.PortRange{Start: t.start, End: t.start + size - 1},
		},
	}
	for _, m := range mutate {
		m(b)
	}
	Expect(k8sClient.Create(ctx, b)).To(Succeed())
}

func (t testNS) createDevice(name string, labels map[string]string) {
	Expect(k8sClient.Create(ctx, &homev1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: t.ns, Labels: labels},
		Spec: homev1alpha1.DeviceSpec{Zigbee2MQTT: &homev1alpha1.ZigbeeDeviceSource{
			InstanceRef: corev1.LocalObjectReference{Name: "z2m"}, IEEEAddress: "0x000b57fffe000001",
		}},
	})).To(Succeed())
}

func (t testNS) deleteDevice(name string) {
	Expect(k8sClient.Delete(ctx, &homev1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: t.ns}})).To(Succeed())
}

func (t testNS) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: t.ns, Name: name}
}

func newReconciler() *BridgeReconciler {
	return &BridgeReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(),
		Image: "oikade:test", MQTTURL: "mqtt://broker:1883",
	}
}

func reconcileBridge(r *BridgeReconciler, key types.NamespacedName) *homekitv1alpha1.Bridge {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var b homekitv1alpha1.Bridge
	if err := k8sClient.Get(ctx, key, &b); err != nil {
		return nil
	}
	return &b
}

func get[T client.Object](key types.NamespacedName, obj T) T {
	ExpectWithOffset(1, k8sClient.Get(ctx, key, obj)).To(Succeed())
	return obj
}

func aids(b *homekitv1alpha1.Bridge) map[string]uint64 {
	out := map[string]uint64{}
	for _, a := range b.Status.Accessories {
		out[a.Device] = a.AID
	}
	return out
}

var _ = Describe("Bridge Controller", func() {
	var r *BridgeReconciler

	BeforeEach(func() {
		r = newReconciler()
	})

	It("provisions identity, port, storage, RBAC and the HAP Deployment", func() {
		t := newTestNS()
		t.createBridge("main", 10)
		b := reconcileBridge(r, t.key("main"))

		Expect(identity.ValidUsername(b.Status.Username)).To(BeTrue())
		Expect(b.Status.Port).To(Equal(t.start))
		Expect(meta.IsStatusConditionTrue(b.Status.Conditions, ConditionPortAllocated)).To(BeTrue())
		Expect(meta.FindStatusCondition(b.Status.Conditions, ConditionReady).Reason).To(Equal("Progressing"))

		dep := get(t.key("main-hap"), &appsv1.Deployment{})
		pod := dep.Spec.Template.Spec
		Expect(pod.HostNetwork).To(BeTrue())
		Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		Expect(pod.Containers[0].Image).To(Equal("oikade:test"))
		Expect(pod.Containers[0].Args).To(ContainElements("--bridge=main", "--mqtt-url=mqtt://broker:1883"))
		Expect(pod.Containers[0].Ports[0].HostPort).To(Equal(t.start))
		Expect(pod.ServiceAccountName).To(Equal("main-hap"))

		role := get(t.key("main-hap"), &rbacv1.Role{})
		Expect(role.Rules).To(ContainElement(HaveField("ResourceNames", ConsistOf("pin", "main-identity"))))
		Expect(role.Rules).To(ContainElement(SatisfyAll(
			HaveField("APIGroups", ConsistOf("plugins.oikade.dev")),
			HaveField("Resources", ConsistOf("plugins")),
			HaveField("Verbs", ConsistOf("get", "list", "watch")),
		)))
		Expect(get(t.key("main-identity"), &corev1.Secret{}).OwnerReferences).To(BeEmpty())
		Expect(get(t.key("main-hap"), &corev1.PersistentVolumeClaim{}).OwnerReferences).To(BeEmpty())
	})

	It("never reuses or reorders accessory IDs", func() {
		t := newTestNS()
		t.createDevice("b-lamp", nil)
		t.createDevice("a-sensor", nil)
		t.createBridge("main", 10)
		b := reconcileBridge(r, t.key("main"))
		Expect(aids(b)).To(Equal(map[string]uint64{"a-sensor": 2, "b-lamp": 3}))
		Expect(b.Status.Devices).To(Equal(int32(2)))

		By("deleting a Device and adding another")
		t.deleteDevice("a-sensor")
		t.createDevice("c-plug", nil)
		b = reconcileBridge(r, t.key("main"))
		Expect(aids(b)).To(Equal(map[string]uint64{"a-sensor": 2, "b-lamp": 3, "c-plug": 4}))
		Expect(b.Status.Devices).To(Equal(int32(2)))

		By("re-creating the deleted Device")
		t.createDevice("a-sensor", nil)
		b = reconcileBridge(r, t.key("main"))
		Expect(aids(b)).To(HaveKeyWithValue("a-sensor", uint64(2)))
		Expect(b.Status.NextAID).To(Equal(uint64(5)))
	})

	It("only exposes Devices matching the selector", func() {
		t := newTestNS()
		t.createDevice("lamp", map[string]string{"homekit": "yes"})
		t.createDevice("router", nil)
		t.createBridge("main", 10, func(b *homekitv1alpha1.Bridge) {
			b.Spec.DeviceSelector = metav1.LabelSelector{MatchLabels: map[string]string{"homekit": "yes"}}
		})
		b := reconcileBridge(r, t.key("main"))
		Expect(aids(b)).To(Equal(map[string]uint64{"lamp": 2}))
	})

	It("restores a deleted identity Secret from status", func() {
		t := newTestNS()
		t.createBridge("main", 10)
		first := reconcileBridge(r, t.key("main")).Status.Username

		Expect(k8sClient.Delete(ctx, get(t.key("main-identity"), &corev1.Secret{}))).To(Succeed())
		b := reconcileBridge(r, t.key("main"))
		Expect(b.Status.Username).To(Equal(first))
		Expect(string(get(t.key("main-identity"), &corev1.Secret{}).Data[render.IdentityUsernameKey])).To(Equal(first))
	})

	It("keeps identity and pairings when the Bridge is re-created", func() {
		t := newTestNS()
		t.createBridge("main", 10)
		first := reconcileBridge(r, t.key("main"))

		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		r.release(t.key("main"))
		t.createBridge("main", 10)
		second := reconcileBridge(r, t.key("main"))
		Expect(second.Status.Username).To(Equal(first.Status.Username))
	})

	It("rejects a weak setup code", func() {
		t := newTestNS()
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "weak", Namespace: t.ns},
			StringData: map[string]string{"pin": "123-45-678"},
		})).To(Succeed())
		t.createBridge("main", 10, func(b *homekitv1alpha1.Bridge) { b.Spec.PinSecretRef.Name = "weak" })
		b := reconcileBridge(r, t.key("main"))
		Expect(meta.FindStatusCondition(b.Status.Conditions, ConditionReady).Reason).To(Equal("InvalidPin"))
		Expect(k8sClient.Get(ctx, t.key("main-hap"), &appsv1.Deployment{})).NotTo(Succeed())
	})

	It("allocates distinct ports and reports exhaustion", func() {
		t := newTestNS()
		t.createBridge("one", 2)
		t.createBridge("two", 2)
		t.createBridge("three", 2)
		p1 := reconcileBridge(r, t.key("one")).Status.Port
		p2 := reconcileBridge(r, t.key("two")).Status.Port
		Expect([]int32{p1, p2}).To(ConsistOf(t.start, t.start+1))

		b := reconcileBridge(r, t.key("three"))
		Expect(b.Status.Port).To(BeZero())
		Expect(meta.FindStatusCondition(b.Status.Conditions, ConditionReady).Reason).To(Equal("PortRangeExhausted"))
	})
})
