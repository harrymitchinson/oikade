package home

import (
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
)

var adoptSeq atomic.Int32

var _ = Describe("Adopting discovered devices", func() {
	var (
		ns       string
		clk      *clocktesting.FakePassiveClock
		recorder *events.FakeRecorder
	)
	BeforeEach(func() {
		clk = clocktesting.NewFakePassiveClock(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
		recorder = events.NewFakeRecorder(16)
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
	reconcile := func(driver string) ctrl.Result {
		r := &AdoptReconciler{Client: k8sClient, Scheme: scheme.Scheme, Driver: driver, Recorder: recorder, Clock: clk}
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key("hue")})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	adopt := func(driver string) { reconcile(driver) }
	on := &homev1alpha1.DiscoverySpec{Adopt: true}
	deskLamp := homev1alpha1.DiscoveredDevice{
		ID: "3f2a9c71", Manufacturer: "Signify", Model: "LCA001", Name: "Desk lamp", Room: "Study",
		Capabilities: []string{"on-off"},
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

		// A rename in the app changes nothing: the name was copied once.
		before := device("study-lamp")
		Expect(before.Spec.DisplayName).To(Equal("Lamp"))
		setDiscovered(p, homev1alpha1.DiscoveredDevice{ID: "a", Name: "Reading lamp", Room: "Study"})
		adopt(AdoptPlugin)
		d := device("study-lamp")
		Expect(d.Spec).To(Equal(before.Spec))
		Expect(metav1.IsControlledBy(d, p)).To(BeTrue())

		// Nor is a name given in the app after adoption: the copy was taken once, empty.
		Expect(device("lca001-c").Spec.DisplayName).To(BeEmpty())
		setDiscovered(p, homev1alpha1.DiscoveredDevice{ID: "c", Model: "LCA001", Name: "Hall lamp"})
		adopt(AdoptPlugin)
		Expect(device("lca001-c").Spec.DisplayName).To(BeEmpty())
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

	It("hands the Device over on a client-side kubectl apply of the same spec", func() {
		p := plugin(on, deskLamp)
		adopt(AdoptPlugin)
		d := device("desk-lamp")
		d.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}
		Expect(k8sClient.Update(ctx, d)).To(Succeed())
		adopt(AdoptPlugin)
		Expect(metav1.IsControlledBy(device("desk-lamp"), p)).To(BeFalse())
	})

	It("keeps only the oldest of two adopted copies of one device", func() {
		p := plugin(on, deskLamp)
		adopt(AdoptPlugin)
		// A second copy, as a stale cache could create after a rename in the app.
		dup := &homev1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "reading-lamp", Namespace: ns},
			Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
				PluginRef: corev1.LocalObjectReference{Name: "hue"}, ID: deskLamp.ID,
			}},
		}
		Expect(controllerutil.SetControllerReference(p, dup, scheme.Scheme)).To(Succeed())
		Expect(k8sClient.Create(ctx, dup, client.FieldOwner("oikade"))).To(Succeed())
		adopt(AdoptPlugin)
		Expect(deviceNames()).To(ConsistOf("desk-lamp"))
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

		// Neither the Device's own name echoed back nor a rename in the frontend is followed.
		for _, name := range []string{"hall-motion", "Landing motion"} {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(z), z)).To(Succeed())
			z.Status.Discovered[0].Name = name
			Expect(k8sClient.Status().Update(ctx, z)).To(Succeed())
			adopt(AdoptZigbee2MQTT)
			Expect(device("hall-motion").Spec.DisplayName).To(Equal("Hall motion"))
		}
	})

	Describe("when a device is removed from its app", func() {
		hallMotion := homev1alpha1.DiscoveredDevice{ID: "a71c", Name: "Hall motion"}
		deleteAfterHour := &homev1alpha1.DiscoverySpec{
			Adopt: true, RemovalPolicy: homev1alpha1.RemovalPolicyDelete,
			RemovalGracePeriod: &metav1.Duration{Duration: time.Hour},
		}
		pending := func(name string) *metav1.Condition {
			return meta.FindStatusCondition(device(name).Status.Conditions, homev1alpha1.ConditionRemovalPending)
		}

		DescribeTable("never removes an offline device", func(discovery *homev1alpha1.DiscoverySpec) {
			p := plugin(discovery, deskLamp)
			adopt(AdoptPlugin)

			// Unplugged, the runtime reports it offline, and it stays in the app's list, so it isn't gone.
			d := device("desk-lamp")
			meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
				Type: homev1alpha1.ConditionAvailable, Status: metav1.ConditionFalse, Reason: "Offline",
			})
			Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
			clk.SetTime(clk.Now().Add(48 * time.Hour))
			Expect(reconcile(AdoptPlugin).RequeueAfter).To(BeZero())
			Expect(pending("desk-lamp")).To(BeNil())

			// A runtime that hasn't reported the list since it started says nothing either.
			setDiscovered(p)
			clk.SetTime(clk.Now().Add(48 * time.Hour))
			adopt(AdoptPlugin)
			Expect(deviceNames()).To(ConsistOf("desk-lamp"))
			Expect(pending("desk-lamp")).To(BeNil())
			Expect(recorder.Events).To(BeEmpty())
		},
			Entry("under Retain", &homev1alpha1.DiscoverySpec{Adopt: true, RemovalPolicy: homev1alpha1.RemovalPolicyRetain}),
			Entry("under Delete", deleteAfterHour),
		)

		It("keeps the Device under Retain", func() {
			p := plugin(&homev1alpha1.DiscoverySpec{Adopt: true, RemovalPolicy: homev1alpha1.RemovalPolicyRetain}, deskLamp)
			adopt(AdoptPlugin)
			setDiscovered(p, homev1alpha1.DiscoveredDevice{ID: "other"})
			clk.SetTime(clk.Now().Add(30 * 24 * time.Hour))
			adopt(AdoptPlugin)
			Expect(deviceNames()).To(ContainElement("desk-lamp"))
			Expect(pending("desk-lamp")).To(BeNil())
		})

		It("deletes the Device under Delete once the grace period has passed", func() {
			p := plugin(deleteAfterHour, deskLamp, hallMotion)
			adopt(AdoptPlugin)
			setDiscovered(p, hallMotion)

			start := clk.Now()
			Expect(reconcile(AdoptPlugin).RequeueAfter).To(Equal(time.Hour))
			cond := pending("desk-lamp")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.LastTransitionTime.Time).To(BeTemporally("==", start))
			Expect(recorder.Events).To(Receive(HavePrefix("Warning RemovalPending")))
			Expect(pending("hall-motion")).To(BeNil())

			clk.SetTime(start.Add(40 * time.Minute))
			Expect(reconcile(AdoptPlugin).RequeueAfter).To(Equal(20 * time.Minute))
			Expect(deviceNames()).To(ContainElement("desk-lamp"))
			Expect(recorder.Events).To(BeEmpty(), "the pending event is recorded once")

			clk.SetTime(start.Add(time.Hour))
			adopt(AdoptPlugin)
			Expect(deviceNames()).To(ConsistOf("hall-motion"))
			Expect(recorder.Events).To(Receive(HavePrefix("Normal Removed")))

			// Gone from the app, it isn't adopted again.
			adopt(AdoptPlugin)
			Expect(deviceNames()).To(ConsistOf("hall-motion"))
		})

		It("keeps a Device whose device returns within the grace period", func() {
			p := plugin(deleteAfterHour, deskLamp)
			adopt(AdoptPlugin)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
			p.Status.Discovered = []homev1alpha1.DiscoveredDevice{}
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
			adopt(AdoptPlugin)
			Expect(pending("desk-lamp")).NotTo(BeNil(), "an empty list means the app has no devices")

			clk.SetTime(clk.Now().Add(50 * time.Minute))
			setDiscovered(p, deskLamp)
			Expect(reconcile(AdoptPlugin).RequeueAfter).To(BeZero())
			Expect(pending("desk-lamp")).To(BeNil())

			// Gone again, the grace period starts over.
			clk.SetTime(clk.Now().Add(20 * time.Minute))
			setDiscovered(p, hallMotion)
			Expect(reconcile(AdoptPlugin).RequeueAfter).To(Equal(time.Hour))
			Expect(deviceNames()).To(ContainElement("desk-lamp"))
		})

		It("clears RemovalPending once the Device is taken over, or adoption is turned off", func() {
			p := plugin(deleteAfterHour, deskLamp, hallMotion)
			adopt(AdoptPlugin)
			setDiscovered(p, homev1alpha1.DiscoveredDevice{ID: "other"})
			adopt(AdoptPlugin)
			Expect(pending("desk-lamp")).NotTo(BeNil())
			Expect(pending("hall-motion")).NotTo(BeNil())

			d := device("desk-lamp")
			d.Spec.DisplayName = "Study lamp"
			Expect(k8sClient.Update(ctx, d)).To(Succeed())
			adopt(AdoptPlugin)
			Expect(metav1.GetControllerOf(device("desk-lamp"))).To(BeNil())
			Expect(pending("desk-lamp")).To(BeNil())

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
			p.Spec.Discovery.Adopt = false
			Expect(k8sClient.Update(ctx, p)).To(Succeed())
			clk.SetTime(clk.Now().Add(48 * time.Hour))
			adopt(AdoptPlugin)
			Expect(pending("hall-motion")).To(BeNil())
			Expect(deviceNames()).To(ContainElements("desk-lamp", "hall-motion"))
		})

		It("keeps a Device annotated oikade.dev/removal-policy: retain under Delete", func() {
			p := plugin(deleteAfterHour, deskLamp)
			adopt(AdoptPlugin)
			d := device("desk-lamp")
			d.Annotations = map[string]string{homev1alpha1.AnnotationRemovalPolicy: "Retain"}
			Expect(k8sClient.Update(ctx, d)).To(Succeed())

			setDiscovered(p, hallMotion)
			adopt(AdoptPlugin)
			clk.SetTime(clk.Now().Add(48 * time.Hour))
			adopt(AdoptPlugin)
			Expect(deviceNames()).To(ContainElement("desk-lamp"))
			Expect(pending("desk-lamp")).To(BeNil())
			Expect(metav1.IsControlledBy(device("desk-lamp"), p)).To(BeTrue(), "an annotation isn't a takeover")
		})
	})

	Describe("replacing a reset device's ID", func() {
		resetLamp := homev1alpha1.DiscoveredDevice{ID: "9d01", Name: "Desk lamp"}
		replaced := func(name string) *metav1.Condition {
			return meta.FindStatusCondition(device(name).Status.Conditions, homev1alpha1.ConditionReplaced)
		}
		annotate := func(name, target string) {
			d := device(name)
			if d.Annotations == nil {
				d.Annotations = map[string]string{}
			}
			d.Annotations[homev1alpha1.AnnotationReplaceWith] = target
			Expect(k8sClient.Update(ctx, d)).To(Succeed())
		}

		It("moves the new ID onto the old Device and deletes the new one", func() {
			p := plugin(on, deskLamp)
			adopt(AdoptPlugin)
			// Reset, the lamp comes back with a new ID and is adopted as a new Device.
			setDiscovered(p, resetLamp)
			adopt(AdoptPlugin)
			Expect(deviceNames()).To(ConsistOf("desk-lamp", "desk-lamp-2"))
			uid := device("desk-lamp").UID

			annotate("desk-lamp", "desk-lamp-2")
			Expect(reconcile(AdoptPlugin).RequeueAfter).NotTo(BeZero())
			Expect(deviceNames()).To(ConsistOf("desk-lamp"))
			d := device("desk-lamp")
			Expect(d.UID).To(Equal(uid))
			Expect(d.Spec.Plugin.ID).To(Equal("9d01"))
			Expect(d.Annotations).NotTo(HaveKey(homev1alpha1.AnnotationReplaceWith))
			Expect(metav1.IsControlledBy(d, p)).To(BeTrue(), "it stays adopted")
			Expect(replaced("desk-lamp").Status).To(Equal(metav1.ConditionTrue))
			Expect(recorder.Events).To(Receive(HavePrefix("Normal Replaced")))

			// The new ID is declared now, so it isn't adopted again.
			adopt(AdoptPlugin)
			Expect(deviceNames()).To(ConsistOf("desk-lamp"))
		})

		It("keeps the annotated Device even when it is the newer one, and works with adoption off", func() {
			p := plugin(on, deskLamp)
			adopt(AdoptPlugin)
			setDiscovered(p, deskLamp, resetLamp)
			adopt(AdoptPlugin)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
			p.Spec.Discovery.Adopt = false
			Expect(k8sClient.Update(ctx, p)).To(Succeed())

			annotate("desk-lamp-2", "desk-lamp")
			adopt(AdoptPlugin)
			adopt(AdoptPlugin)
			Expect(deviceNames()).To(ConsistOf("desk-lamp-2"))
			Expect(device("desk-lamp-2").Spec.Plugin.ID).To(Equal(deskLamp.ID))
		})

		It("replaces a Zigbee2MQTT device's IEEE address", func() {
			z := &zigbeev1alpha1.Zigbee2MQTT{
				ObjectMeta: metav1.ObjectMeta{Name: "hue", Namespace: ns},
				Spec: zigbeev1alpha1.Zigbee2MQTTSpec{
					Serial:    zigbeev1alpha1.SerialSpec{Port: "tcp://slzb-06:6638", Adapter: "zstack"},
					Discovery: on,
				},
			}
			Expect(k8sClient.Create(ctx, z)).To(Succeed())
			z.Status.Discovered = []homev1alpha1.DiscoveredDevice{
				{ID: "0x00158d0001a2b3c4", Name: "Porch sensor"}, {ID: "0x00158d0001ffee01", Name: "Porch sensor"},
			}
			Expect(k8sClient.Status().Update(ctx, z)).To(Succeed())
			adopt(AdoptZigbee2MQTT)
			Expect(deviceNames()).To(ConsistOf("porch-sensor", "porch-sensor-2"))

			annotate("porch-sensor", "porch-sensor-2")
			adopt(AdoptZigbee2MQTT)
			Expect(deviceNames()).To(ConsistOf("porch-sensor"))
			Expect(device("porch-sensor").Spec.Zigbee2MQTT.IEEEAddress).To(Equal("0x00158d0001ffee01"))
		})

		It("refuses a target that is declared in Git, or adopted from another instance", func() {
			plugin(on, deskLamp, resetLamp)
			adopt(AdoptPlugin)
			Expect(k8sClient.Create(ctx, &homev1alpha1.Device{
				ObjectMeta: metav1.ObjectMeta{Name: "study-light", Namespace: ns},
				Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
					PluginRef: corev1.LocalObjectReference{Name: "hue"}, ID: "c0ffee",
				}},
			})).To(Succeed())
			other := &pluginsv1alpha1.Plugin{
				ObjectMeta: metav1.ObjectMeta{Name: "ewelink", Namespace: ns},
				Spec:       pluginsv1alpha1.PluginSpec{Image: "example.com/oikade-plugin-ewelink:test"},
			}
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			fan := &homev1alpha1.Device{
				ObjectMeta: metav1.ObjectMeta{Name: "fan", Namespace: ns},
				Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
					PluginRef: corev1.LocalObjectReference{Name: "ewelink"}, ID: "1000aabbcc",
				}},
			}
			Expect(controllerutil.SetControllerReference(other, fan, scheme.Scheme)).To(Succeed())
			Expect(k8sClient.Create(ctx, fan, client.FieldOwner("oikade"))).To(Succeed())

			for target, reason := range map[string]string{
				"study-light": "TargetNotAdopted",
				"fan":         "OtherInstance",
				"nothing":     "TargetNotFound",
				"desk-lamp":   "InvalidTarget",
			} {
				By(target)
				annotate("desk-lamp", target)
				adopt(AdoptPlugin)
				c := replaced("desk-lamp")
				Expect(c).NotTo(BeNil())
				Expect(c.Status).To(Equal(metav1.ConditionFalse))
				Expect(c.Reason).To(Equal(reason))
				Expect(recorder.Events).To(Receive(HavePrefix("Warning ReplaceRefused")))
				Expect(device("desk-lamp").Spec.Plugin.ID).To(Equal(deskLamp.ID))
			}
			Expect(deviceNames()).To(ConsistOf("desk-lamp", "desk-lamp-2", "study-light", "fan"))

			// Refused again on the next pass, without another event.
			adopt(AdoptPlugin)
			Expect(recorder.Events).To(BeEmpty())

			// Removing the annotation clears the refusal.
			d := device("desk-lamp")
			delete(d.Annotations, homev1alpha1.AnnotationReplaceWith)
			Expect(k8sClient.Update(ctx, d)).To(Succeed())
			adopt(AdoptPlugin)
			Expect(replaced("desk-lamp")).To(BeNil())
		})

		It("refuses on a Device declared in Git", func() {
			plugin(on, resetLamp)
			adopt(AdoptPlugin)
			Expect(k8sClient.Create(ctx, &homev1alpha1.Device{
				ObjectMeta: metav1.ObjectMeta{Name: "study-light", Namespace: ns},
				Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
					PluginRef: corev1.LocalObjectReference{Name: "hue"}, ID: "c0ffee",
				}},
			})).To(Succeed())
			annotate("study-light", "desk-lamp")
			adopt(AdoptPlugin)
			Expect(replaced("study-light").Reason).To(Equal("NotAdopted"))
			Expect(deviceNames()).To(ConsistOf("study-light", "desk-lamp"))
		})
	})
})
