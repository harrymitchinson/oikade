package zigbee

import (
	"fmt"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/render"
)

var testSeq atomic.Int32

const (
	instance      = "z2m"
	networkSecret = instance + "-network"
)

func newNamespace() string {
	ns := fmt.Sprintf("zigbee-%d", testSeq.Add(1))
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	return ns
}

func createInstance(ns string) {
	Expect(k8sClient.Create(ctx, &zigbeev1alpha1.Zigbee2MQTT{
		ObjectMeta: metav1.ObjectMeta{Name: instance, Namespace: ns},
		Spec: zigbeev1alpha1.Zigbee2MQTTSpec{
			Serial: zigbeev1alpha1.SerialSpec{Port: "tcp://slzb-06:6638", Adapter: "zstack"},
		},
	})).To(Succeed())
}

func createDevice(ns, name, instance, ieee string) {
	Expect(k8sClient.Create(ctx, &homev1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: homev1alpha1.DeviceSpec{Zigbee2MQTT: &homev1alpha1.ZigbeeDeviceSource{
			InstanceRef: corev1.LocalObjectReference{Name: instance}, IEEEAddress: ieee,
		}},
	})).To(Succeed())
}

func reconcileInstance(r *Zigbee2MQTTReconciler, key types.NamespacedName) *zigbeev1alpha1.Zigbee2MQTT {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return get(key, &zigbeev1alpha1.Zigbee2MQTT{})
}

func get[T client.Object](key types.NamespacedName, obj T) T {
	ExpectWithOffset(1, k8sClient.Get(ctx, key, obj)).To(Succeed())
	return obj
}

// renderedDevices returns IEEE address -> friendly_name from the rendered configuration.yaml.
func renderedDevices(ns, name string) map[string]string {
	s := get(types.NamespacedName{Namespace: ns, Name: name + "-config"}, &corev1.Secret{})
	var cfg struct {
		Devices map[string]struct {
			FriendlyName string `json:"friendly_name"`
		} `json:"devices"`
	}
	ExpectWithOffset(1, yaml.Unmarshal(s.Data["configuration.yaml"], &cfg)).To(Succeed())
	out := map[string]string{}
	for addr, d := range cfg.Devices {
		out[addr] = d.FriendlyName
	}
	return out
}

var _ = Describe("Zigbee2MQTT Controller", func() {
	var r *Zigbee2MQTTReconciler

	BeforeEach(func() {
		r = &Zigbee2MQTTReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Image: "koenkk/zigbee2mqtt:test", MQTTURL: "mqtt://broker:1883",
		}
	})

	It("renders config from declared Devices and runs Zigbee2MQTT", func() {
		ns := newNamespace()
		createInstance(ns)
		createDevice(ns, "hallway-light", instance, "0x000b57fffe000001")
		createDevice(ns, "front-door", instance, "0x00158d0000000003")
		createDevice(ns, "elsewhere", "other", "0x00158d0000000009")
		key := types.NamespacedName{Namespace: ns, Name: instance}

		z := reconcileInstance(r, key)
		Expect(z.Status.Devices).To(Equal(int32(2)))
		Expect(z.Status.PanID).NotTo(BeZero())
		Expect(z.Status.ConfigHash).NotTo(BeEmpty())
		Expect(renderedDevices(ns, instance)).To(Equal(map[string]string{
			"0x000b57fffe000001": "hallway-light",
			"0x00158d0000000003": "front-door",
		}))

		dep := get(key, &appsv1.Deployment{})
		Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(render.AnnotationConfigHash, z.Status.ConfigHash))
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("koenkk/zigbee2mqtt:test"))
		Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))

		// envtest has no kubelet, so the Deployment never becomes available.
		ready := meta.FindStatusCondition(z.Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("Progressing"))

		By("adding a Device, which changes the config hash")
		createDevice(ns, "desk-plug", instance, "0x00124b0000000004")
		z2 := reconcileInstance(r, key)
		Expect(z2.Status.Devices).To(Equal(int32(3)))
		Expect(z2.Status.ConfigHash).NotTo(Equal(z.Status.ConfigHash))
	})

	It("keeps the network identity and its storage when the instance is deleted", func() {
		ns := newNamespace()
		createInstance(ns)
		key := types.NamespacedName{Namespace: ns, Name: instance}
		z := reconcileInstance(r, key)

		netKey := types.NamespacedName{Namespace: ns, Name: networkSecret}
		network := get(netKey, &corev1.Secret{})
		Expect(network.OwnerReferences).To(BeEmpty())
		Expect(get(types.NamespacedName{Namespace: ns, Name: "z2m-data"}, &corev1.PersistentVolumeClaim{}).OwnerReferences).To(BeEmpty())
		Expect(get(types.NamespacedName{Namespace: ns, Name: "z2m-config"}, &corev1.Secret{}).OwnerReferences).To(HaveLen(1))

		By("re-creating the instance")
		Expect(k8sClient.Delete(ctx, z)).To(Succeed())
		createInstance(ns)
		z2 := reconcileInstance(r, key)
		Expect(z2.Status.PanID).To(Equal(z.Status.PanID))
		Expect(get(netKey, &corev1.Secret{}).Data).To(Equal(network.Data))
	})

	It("reports a corrupt network Secret instead of replacing it", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: networkSecret, Namespace: ns},
			StringData: map[string]string{render.NetworkKeyKey: "nope"},
		})).To(Succeed())
		createInstance(ns)

		z := reconcileInstance(r, types.NamespacedName{Namespace: ns, Name: instance})
		ready := meta.FindStatusCondition(z.Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("InvalidNetworkSecret"))
		Expect(string(get(types.NamespacedName{Namespace: ns, Name: networkSecret}, &corev1.Secret{}).Data[render.NetworkKeyKey])).To(Equal("nope"))
	})

	It("flags an IEEE address claimed by two Devices", func() {
		ns := newNamespace()
		createInstance(ns)
		createDevice(ns, "a-lamp", instance, "0x000b57fffe000001")
		createDevice(ns, "b-lamp", instance, "0x000b57fffe000001")

		z := reconcileInstance(r, types.NamespacedName{Namespace: ns, Name: instance})
		Expect(meta.FindStatusCondition(z.Status.Conditions, ConditionReady).Reason).To(Equal("DuplicateDevice"))
		Expect(renderedDevices(ns, instance)).To(Equal(map[string]string{"0x000b57fffe000001": "a-lamp"}))
	})

	It("creates the frontend Service only when enabled", func() {
		ns := newNamespace()
		createInstance(ns)
		key := types.NamespacedName{Namespace: ns, Name: instance}
		reconcileInstance(r, key)
		Expect(k8sClient.Get(ctx, key, &corev1.Service{})).NotTo(Succeed())

		z := get(key, &zigbeev1alpha1.Zigbee2MQTT{})
		z.Spec.Frontend = true
		Expect(k8sClient.Update(ctx, z)).To(Succeed())
		reconcileInstance(r, key)
		Expect(get(key, &corev1.Service{}).Spec.Ports[0].Port).To(Equal(int32(8080)))

		z = get(key, &zigbeev1alpha1.Zigbee2MQTT{})
		z.Spec.Frontend = false
		Expect(k8sClient.Update(ctx, z)).To(Succeed())
		reconcileInstance(r, key)
		Expect(k8sClient.Get(ctx, key, &corev1.Service{})).NotTo(Succeed())
	})
})
