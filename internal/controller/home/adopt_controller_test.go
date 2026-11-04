package home

import (
	"fmt"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
)

var adoptSeq atomic.Int32

var _ = Describe("Adopting discovered devices", func() {
	var ns string
	BeforeEach(func() {
		ns = fmt.Sprintf("adopt-%d", adoptSeq.Add(1))
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		Expect(k8sClient.Create(ctx, &homev1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "study", Namespace: ns}})).To(Succeed())
	})

	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }
	device := func(name string) *homev1alpha1.Device {
		var d homev1alpha1.Device
		Expect(k8sClient.Get(ctx, key(name), &d)).To(Succeed())
		return &d
	}
	deviceNames := func() []string {
		var list homev1alpha1.DeviceList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
		names := make([]string, 0, len(list.Items))
		for _, d := range list.Items {
			names = append(names, d.Name)
		}
		return names
	}

	// plugin creates a Plugin named hue that discovers devices.
	plugin := func(discovery *homev1alpha1.DiscoverySpec, discovered ...homev1alpha1.DiscoveredDevice) *pluginsv1alpha1.Plugin {
		p := &pluginsv1alpha1.Plugin{
			ObjectMeta: metav1.ObjectMeta{Name: "hue", Namespace: ns},
			Spec:       pluginsv1alpha1.PluginSpec{Image: "example.com/oikade-plugin-hue:test", Discovery: discovery},
		}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		p.Status.Discovered = discovered
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		return p
	}
	setDiscovered := func(p *pluginsv1alpha1.Plugin, discovered ...homev1alpha1.DiscoveredDevice) {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		p.Status.Discovered = discovered
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}
	adopt := func(driver string) {
		r := &AdoptReconciler{Client: k8sClient, Scheme: scheme.Scheme, Driver: driver}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key("hue")})
		Expect(err).NotTo(HaveOccurred())
	}
	on := &homev1alpha1.DiscoverySpec{Adopt: true}
	deskLamp := homev1alpha1.DiscoveredDevice{
		ID: "3f2a9c71", Manufacturer: "Signify", Model: "LCA001", Name: "Desk lamp", Room: "Study",
		Capabilities: []string{"light.on"},
	}

	It("creates a Device named after the device in its app, in the matching Room", func() {
		p := plugin(on, deskLamp, homev1alpha1.DiscoveredDevice{ID: "garage-1", Name: "Garage door", Room: "Garage"})
		adopt(AdoptPlugin)

		d := device("desk-lamp")
		Expect(d.Spec.DisplayName).To(Equal("Desk lamp"))
		Expect(d.Spec.RoomRef).To(Equal(&corev1.LocalObjectReference{Name: "study"}))
		Expect(d.Spec.Plugin).To(Equal(&homev1alpha1.PluginDeviceSource{
			PluginRef: corev1.LocalObjectReference{Name: "hue"}, ID: "3f2a9c71",
		}))
		Expect(metav1.IsControlledBy(d, p)).To(BeTrue())
		Expect(d.Annotations).NotTo(HaveKey(homev1alpha1.AnnotationMatter))
		// Rooms are only matched, never created.
		Expect(device("garage-door").Spec.RoomRef).To(BeNil())

		// Adopting again changes nothing.
		adopt(AdoptPlugin)
		Expect(deviceNames()).To(ConsistOf("desk-lamp", "garage-door"))
	})

	It("does nothing unless adopt is set", func() {
		plugin(&homev1alpha1.DiscoverySpec{}, deskLamp)
		adopt(AdoptPlugin)
		Expect(deviceNames()).To(BeEmpty())
	})

	It("hides adopted Devices from Matter with discovery.matter Hide", func() {
		plugin(&homev1alpha1.DiscoverySpec{Adopt: true, Matter: homev1alpha1.DiscoveryMatterHide}, deskLamp)
		adopt(AdoptPlugin)
		Expect(device("desk-lamp").HiddenFromMatter()).To(BeTrue())
	})

	It("keeps names on a clash, and never renames a Device", func() {
		Expect(k8sClient.Create(ctx, &homev1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "lamp", Namespace: ns},
			Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
				PluginRef: corev1.LocalObjectReference{Name: "other"}, ID: "x",
			}},
		})).To(Succeed())
		p := plugin(on,
			homev1alpha1.DiscoveredDevice{ID: "a", Name: "Lamp", Room: "Study"},
			homev1alpha1.DiscoveredDevice{ID: "b", Name: "Lamp", Room: "Study"},
			homev1alpha1.DiscoveredDevice{ID: "c", Model: "LCA001"},
		)
		adopt(AdoptPlugin)
		Expect(deviceNames()).To(ConsistOf("lamp", "study-lamp", "study-lamp-2", "lca001-c"))

		// A rename in the app changes only displayName.
		setDiscovered(p, homev1alpha1.DiscoveredDevice{ID: "a", Name: "Reading lamp", Room: "Study"})
		adopt(AdoptPlugin)
		d := device("study-lamp")
		Expect(d.Spec.DisplayName).To(Equal("Reading lamp"))
		Expect(d.Spec.Plugin.ID).To(Equal("a"))
	})

	It("keeps your annotations, and hands the Device over once you write its spec", func() {
		p := plugin(on, deskLamp)
		adopt(AdoptPlugin)

		d := device("desk-lamp")
		d.Annotations = map[string]string{homev1alpha1.AnnotationMatter: homev1alpha1.MatterHide}
		Expect(k8sClient.Update(ctx, d)).To(Succeed())
		adopt(AdoptPlugin)
		d = device("desk-lamp")
		Expect(d.HiddenFromMatter()).To(BeTrue())
		Expect(metav1.IsControlledBy(d, p)).To(BeTrue(), "an annotation alone isn't a takeover")

		// Writing a spec field, as kubectl apply, kubectl edit or Flux would, takes the Device over.
		d.Spec.DisplayName = "Study lamp"
		Expect(k8sClient.Update(ctx, d)).To(Succeed())
		adopt(AdoptPlugin)
		d = device("desk-lamp")
		Expect(metav1.GetControllerOf(d)).To(BeNil())

		// It's no longer adopted: renames in the app don't reach it.
		setDiscovered(p, homev1alpha1.DiscoveredDevice{ID: "3f2a9c71", Name: "Lamp"})
		adopt(AdoptPlugin)
		Expect(device("desk-lamp").Spec.DisplayName).To(Equal("Study lamp"))
	})

	It("deletes its copy when another Device declares the same device", func() {
		plugin(on, deskLamp)
		adopt(AdoptPlugin)
		Expect(k8sClient.Create(ctx, &homev1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "study-light", Namespace: ns},
			Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
				PluginRef: corev1.LocalObjectReference{Name: "hue"}, ID: "3f2a9c71",
			}},
		})).To(Succeed())
		adopt(AdoptPlugin)
		Expect(deviceNames()).To(ConsistOf("study-light"))
	})

	It("adopts Zigbee2MQTT devices, copying their name once", func() {
		z := &zigbeev1alpha1.Zigbee2MQTT{
			ObjectMeta: metav1.ObjectMeta{Name: "hue", Namespace: ns},
			Spec: zigbeev1alpha1.Zigbee2MQTTSpec{
				Serial:    zigbeev1alpha1.SerialSpec{Port: "tcp://slzb-06:6638", Adapter: "zstack"},
				Discovery: on,
			},
		}
		Expect(k8sClient.Create(ctx, z)).To(Succeed())
		z.Status.Discovered = []homev1alpha1.DiscoveredDevice{{ID: "0x00158d0001a2b3c4", Name: "Hall motion"}}
		Expect(k8sClient.Status().Update(ctx, z)).To(Succeed())
		adopt(AdoptZigbee2MQTT)

		d := device("hall-motion")
		Expect(d.Spec.DisplayName).To(Equal("Hall motion"))
		Expect(d.Spec.Zigbee2MQTT).To(Equal(&homev1alpha1.ZigbeeDeviceSource{
			InstanceRef: corev1.LocalObjectReference{Name: "hue"}, IEEEAddress: "0x00158d0001a2b3c4",
		}))
		Expect(metav1.IsControlledBy(d, z)).To(BeTrue())

		// Zigbee2MQTT then shows the Device's name, which isn't followed back.
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(z), z)).To(Succeed())
		z.Status.Discovered[0].Name = "hall-motion"
		Expect(k8sClient.Status().Update(ctx, z)).To(Succeed())
		adopt(AdoptZigbee2MQTT)
		Expect(device("hall-motion").Spec.DisplayName).To(Equal("Hall motion"))
	})
})
