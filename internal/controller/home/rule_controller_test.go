package home

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/engine"
)

var _ = Describe("Rule Controller", func() {
	reconcileReady := func(name, condition string, value string) *metav1.Condition {
		key := types.NamespacedName{Namespace: "default", Name: name}
		Expect(k8sClient.Create(ctx, &homev1alpha1.Rule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: homev1alpha1.RuleSpec{
				Triggers: []homev1alpha1.Trigger{{Change: &homev1alpha1.ChangeTrigger{
					Device: "hallway-motion", Capability: "occupancy",
				}}},
				Condition: condition,
				Do: []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
					Device: "hallway-light", Capability: "current-level", Value: apiextensionsv1.JSON{Raw: []byte(value)},
				}}},
			},
		})).To(Succeed())
		r := &RuleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var a homev1alpha1.Rule
		Expect(k8sClient.Get(ctx, key, &a)).To(Succeed())
		Expect(a.Status.ObservedGeneration).To(Equal(a.Generation))
		return meta.FindStatusCondition(a.Status.Conditions, homev1alpha1.ConditionReady)
	}

	It("marks a valid Rule Ready", func() {
		cond := reconcileReady("valid", "trigger.value && now.getHours(tz) >= 18", "40")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("reports a bad CEL condition", func() {
		cond := reconcileReady("bad-cel", "trigger.value &&", "40")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Invalid"))
		Expect(cond.Message).To(ContainSubstring("condition"))
	})

	It("reports a value of the wrong kind", func() {
		cond := reconcileReady("bad-value", "", `"bright"`)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring("takes a number"))
	})

	It("rejects an action with both set and delay at admission", func() {
		err := k8sClient.Create(ctx, &homev1alpha1.Rule{
			ObjectMeta: metav1.ObjectMeta{Name: "both", Namespace: "default"},
			Spec: homev1alpha1.RuleSpec{
				Triggers: []homev1alpha1.Trigger{{Change: &homev1alpha1.ChangeTrigger{Device: "d", Capability: "on-off"}}},
				Do: []homev1alpha1.Action{{
					Set:   &homev1alpha1.SetAction{Device: "d", Capability: "on-off", Value: apiextensionsv1.JSON{Raw: []byte("true")}},
					Delay: &metav1.Duration{Duration: 1},
				}},
			},
		})
		Expect(err).To(MatchError(ContainSubstring("exactly one of set, delay or release")))
	})

	Describe("rooms", Ordered, func() {
		// A namespace of its own, so the Room map only sees these Rules.
		const roomNS = "rooms"
		r := &RuleReconciler{}
		BeforeEach(func() {
			r.Client, r.Scheme = k8sClient, k8sClient.Scheme()
		})

		ready := func(name string) *metav1.Condition {
			key := types.NamespacedName{Namespace: roomNS, Name: name}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			var a homev1alpha1.Rule
			Expect(k8sClient.Get(ctx, key, &a)).To(Succeed())
			return meta.FindStatusCondition(a.Status.Conditions, homev1alpha1.ConditionReady)
		}
		create := func(name, value string) {
			Expect(k8sClient.Create(ctx, &homev1alpha1.Rule{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: roomNS},
				Spec: homev1alpha1.RuleSpec{
					Triggers: []homev1alpha1.Trigger{
						{Change: &homev1alpha1.ChangeTrigger{Room: "livingroom", Capability: "occupancy"}},
						{Change: &homev1alpha1.ChangeTrigger{Device: "hallway-motion", Capability: "occupancy"}},
					},
					Do: []homev1alpha1.Action{
						{Set: &homev1alpha1.SetAction{Room: "livingroom", Capability: "on-off", Value: apiextensionsv1.JSON{Raw: []byte(value)}}},
						{Set: &homev1alpha1.SetAction{Room: "kitchen", Capability: "on-off", Value: apiextensionsv1.JSON{Raw: []byte(value)}}},
					},
				},
			})).To(Succeed())
		}
		room := func(name string) *homev1alpha1.Room {
			return &homev1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: roomNS}}
		}

		It("reports missing Rooms until they exist", func() {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: roomNS}})).To(Succeed())
			create("goodnight", "false")

			cond := ready("goodnight")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("RoomNotFound"))
			Expect(cond.Message).To(Equal("Room(s) not found: kitchen, livingroom"))

			Expect(k8sClient.Create(ctx, room("kitchen"))).To(Succeed())
			Expect(ready("goodnight").Message).To(Equal("Room(s) not found: livingroom"))

			// Creating the last Room maps to the Rule, so the watch re-reconciles it unedited.
			living := room("livingroom")
			Expect(k8sClient.Create(ctx, living)).To(Succeed())
			Expect(r.rulesInNamespace(ctx, living)).To(ContainElement(
				reconcile.Request{NamespacedName: types.NamespacedName{Namespace: roomNS, Name: "goodnight"}}))
			cond = ready("goodnight")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Valid"))
		})

		It("reports Invalid before RoomNotFound", func() {
			create("bad-and-missing", `"off"`)
			Expect(k8sClient.Delete(ctx, room("kitchen"))).To(Succeed())
			cond := ready("bad-and-missing")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Invalid"))
		})

		It("doesn't map a Room to Rules in other namespaces", func() {
			Expect(r.rulesInNamespace(ctx, &homev1alpha1.Room{
				ObjectMeta: metav1.ObjectMeta{Name: "livingroom", Namespace: "empty"},
			})).To(BeEmpty())
		})
	})

	// kubectl decodes YAML 1.1, where some bare keys aren't strings (on is true), so the samples
	// go through the same decoder, strictly, before the API server sees them. Then they must
	// compile, as the operator would report them Invalid otherwise.
	It("accepts and compiles the samples as kubectl decodes them", func() {
		f, err := os.Open(filepath.Join("..", "..", "..", "config", "samples", "home_v1alpha1_rule.yaml"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(f.Close)
		docs := utilyaml.NewYAMLReader(bufio.NewReader(f))
		var names []string
		for {
			doc, err := docs.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			Expect(err).NotTo(HaveOccurred())
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}
			var rule homev1alpha1.Rule
			Expect(yaml.UnmarshalStrict(doc, &rule)).To(Succeed())
			rule.Namespace = "default"
			Expect(k8sClient.Create(ctx, &rule)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, &rule)
			_, err = engine.Compile(&rule)
			Expect(err).NotTo(HaveOccurred(), "sample %s", rule.Name)
			names = append(names, rule.Name)
		}
		Expect(names).To(ConsistOf("hallway-motion-light", "goodnight-living-room", "porch-light-sunset",
			"towel-rail-boost", "towel-rail-mornings", "workshop-overheat", "workshop-overheat-reset", "cellar-drying"))
	})

	It("brings every sample Rule to Ready next to the sample Devices and Rooms", func() {
		const ns = "samples"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		applySamples(ns, "home_v1alpha1_room.yaml", func() client.Object { return &homev1alpha1.Room{} })
		applySamples(ns, "home_v1alpha1_device.yaml", func() client.Object { return &homev1alpha1.Device{} })
		rules := applySamples(ns, "home_v1alpha1_rule.yaml", func() client.Object { return &homev1alpha1.Rule{} })
		Expect(rules).NotTo(BeEmpty())
		r := &RuleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for _, name := range rules {
			key := types.NamespacedName{Namespace: ns, Name: name}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			var a homev1alpha1.Rule
			Expect(k8sClient.Get(ctx, key, &a)).To(Succeed())
			cond := meta.FindStatusCondition(a.Status.Conditions, homev1alpha1.ConditionReady)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue), "%s: %s", name, cond.Message)
		}
	})

	It("rejects a moment with no trigger at admission", func() {
		err := k8sClient.Create(ctx, &homev1alpha1.Rule{
			ObjectMeta: metav1.ObjectMeta{Name: "no-trigger", Namespace: "default"},
			Spec: homev1alpha1.RuleSpec{
				Triggers: []homev1alpha1.Trigger{{}},
				Do:       []homev1alpha1.Action{{Delay: &metav1.Duration{Duration: 1}}},
			},
		})
		Expect(err).To(MatchError(ContainSubstring("exactly one of change, at or sun")))
	})

	Describe("time and sun triggers", func() {
		create := func(name string, trigger homev1alpha1.Trigger) error {
			return k8sClient.Create(ctx, &homev1alpha1.Rule{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: homev1alpha1.RuleSpec{
					Triggers: []homev1alpha1.Trigger{trigger},
					Do:       []homev1alpha1.Action{{Delay: &metav1.Duration{Duration: 1}}},
				},
			})
		}
		offset := func(d string) *metav1.Duration {
			dur, err := time.ParseDuration(d)
			Expect(err).NotTo(HaveOccurred())
			return &metav1.Duration{Duration: dur}
		}

		It("accepts at and sun triggers", func() {
			Expect(create("at-weekend-nights", homev1alpha1.Trigger{At: &homev1alpha1.AtTrigger{
				Time: "00:30", Days: []homev1alpha1.Weekday{"Sat", "Sun"},
			}})).To(Succeed())
			Expect(create("sun-before", homev1alpha1.Trigger{Sun: &homev1alpha1.SunTrigger{
				Event: homev1alpha1.SunEventSunset, Offset: offset("-15m"),
			}})).To(Succeed())
		})

		It("rejects two kinds in one trigger", func() {
			Expect(create("at-and-sun", homev1alpha1.Trigger{
				At:  &homev1alpha1.AtTrigger{Time: "07:00"},
				Sun: &homev1alpha1.SunTrigger{Event: homev1alpha1.SunEventSunrise},
			})).To(MatchError(ContainSubstring("exactly one of change, at or sun")))
		})

		It("rejects a malformed time, day or offset", func() {
			Expect(create("bad-time", homev1alpha1.Trigger{At: &homev1alpha1.AtTrigger{Time: "7:00"}})).
				To(MatchError(ContainSubstring("spec.triggers[0].at.time")))
			Expect(create("bad-day", homev1alpha1.Trigger{At: &homev1alpha1.AtTrigger{
				Time: "07:00", Days: []homev1alpha1.Weekday{"Monday"},
			}})).To(MatchError(ContainSubstring("spec.triggers[0].at.days[0]")))
			Expect(create("long-offset", homev1alpha1.Trigger{Sun: &homev1alpha1.SunTrigger{
				Event: homev1alpha1.SunEventSunset, Offset: offset("7h"),
			}})).To(MatchError(ContainSubstring("offset must be between -6h and 6h")))
		})
	})

	Describe("device or room", func() {
		create := func(name string, trigger homev1alpha1.ChangeTrigger, set homev1alpha1.SetAction) error {
			trigger.Capability = "occupancy"
			set.Capability, set.Value = "on-off", apiextensionsv1.JSON{Raw: []byte("true")}
			return k8sClient.Create(ctx, &homev1alpha1.Rule{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: homev1alpha1.RuleSpec{
					Triggers: []homev1alpha1.Trigger{{Change: &trigger}},
					Do:       []homev1alpha1.Action{{Set: &set}},
				},
			})
		}

		It("accepts a device or a room on change triggers and set actions", func() {
			Expect(create("device-device", homev1alpha1.ChangeTrigger{Device: "m"}, homev1alpha1.SetAction{Device: "l"})).To(Succeed())
			Expect(create("room-room", homev1alpha1.ChangeTrigger{Room: "hallway"}, homev1alpha1.SetAction{Room: "hallway"})).To(Succeed())
			Expect(create("device-room", homev1alpha1.ChangeTrigger{Device: "m"}, homev1alpha1.SetAction{Room: "hallway"})).To(Succeed())
		})

		It("rejects a trigger with neither or both", func() {
			Expect(create("trigger-neither", homev1alpha1.ChangeTrigger{}, homev1alpha1.SetAction{Device: "l"})).
				To(MatchError(ContainSubstring("exactly one of device or room")))
			Expect(create("trigger-both", homev1alpha1.ChangeTrigger{Device: "m", Room: "hallway"}, homev1alpha1.SetAction{Device: "l"})).
				To(MatchError(ContainSubstring("exactly one of device or room")))
		})

		It("rejects a set action with neither or both", func() {
			Expect(create("set-neither", homev1alpha1.ChangeTrigger{Device: "m"}, homev1alpha1.SetAction{})).
				To(MatchError(ContainSubstring("exactly one of device or room")))
			Expect(create("set-both", homev1alpha1.ChangeTrigger{Device: "m"}, homev1alpha1.SetAction{Device: "l", Room: "hallway"})).
				To(MatchError(ContainSubstring("exactly one of device or room")))
		})
	})

	Describe("holds", func() {
		create := func(name, spec string) error {
			var a homev1alpha1.Rule
			Expect(yaml.UnmarshalStrict([]byte("metadata: {name: "+name+", namespace: default}\nspec:\n"+spec), &a)).To(Succeed())
			err := k8sClient.Create(ctx, &a)
			if err == nil {
				DeferCleanup(k8sClient.Delete, ctx, &a)
			}
			return err
		}
		const held = "\n  hold: {device: d, capability: on-off, value: true}"

		It("accepts a window hold, a latched Safety hold and claims", func() {
			Expect(create("hold-window", `
  while: {window: {start: {sun: Sunset, offset: -15m}, end: "23:30", days: [Fri]}, offDelay: 5m}`+held)).To(Succeed())
			Expect(create("hold-latched", `
  priority: Safety
  while: {when: 'devices["t"]["temperature"] > 45', latch: true}`+held)).To(Succeed())
			Expect(create("claims", `
  triggers: [{at: {time: "07:00"}}]
  do:
  - set: {device: d, capability: on-off, value: true, for: 1h, until: Next, priority: Override}
  - release: {rule: hold-latched}`)).To(Succeed())
		})

		It("rejects malformed shapes at admission", func() {
			for spec, want := range map[string]string{
				`
  while: {when: "true"}`: "hold is required with while",
				`
  triggers: [{at: {time: "07:00"}}]
  do: [{delay: 1m}]
  while: {when: "true"}` + held: "exactly one of triggers or while",
				held: "exactly one of triggers or while",
				`
  while: {when: "true"}
  do: [{delay: 1m}]` + held: "do is required with triggers, and only with triggers",
				`
  triggers: [{at: {time: "07:00"}}]` + held + `
  do: [{delay: 1m}]`: "hold is required with while, and only with while",
				`
  while: {}` + held: "while needs a window, when or budget",
				`
  while: {window: {start: "06:00", end: "08:00"}, latch: true}` + held: "latch needs when",
				`
  while: {when: "true", offDelay: -1m}` + held: "offDelay must not be negative",
				`
  priority: Safety
  triggers: [{at: {time: "07:00"}}]
  do: [{delay: 1m}]`: "priority applies to a hold",
				`
  priority: Urgent
  while: {when: "true"}` + held: "spec.priority",
				`
  condition: "true"
  while: {when: "true"}` + held: "condition applies to triggers",
				`
  triggers: [{at: {time: "07:00"}}]
  do: [{set: {device: d, capability: on-off, value: true, for: 1h, until: Released}}]`: "for can't be combined with until: Released",
				`
  triggers: [{at: {time: "07:00"}}]
  do: [{set: {device: d, capability: on-off, value: true, until: Later}}]`: "spec.do[0].set.until",
				`
  triggers: [{at: {time: "07:00"}}]
  do: [{delay: 1m, release: {rule: x}}]`: "exactly one of set, delay or release",
				`
  while: {when: "true"}
  hold: {capability: on-off, value: true}`: "exactly one of device or room",
				`
  while: {budget: {minutes: {expression: "60"}}, window: {start: "06:00", end: "08:00"}}` + held: "budget must be the only field of while",
				`
  while: {budget: {minutes: {expression: "60"}}}
  hold: {room: r, capability: on-off, value: true}`: "a budget holds a device, not a room",
				`
  while: {budget: {minutes: {expression: "60", bands: {input: {device: h, capability: temperature}, missingMinutes: 0, ranges: [{minutes: 1}]}}}}` + held: "exactly one of bands or expression",
				`
  while: {budget: {slotMinutes: 20, minutes: {expression: "60"}}}` + held: "spec.while.budget.slotMinutes",
				`
  while: {budget: {planningTime: "24:00", minutes: {expression: "60"}}}` + held: "spec.while.budget.planningTime",
				`
  while: {budget: {minutes: {bands: {input: {device: h, capability: temperature}, missingMinutes: 2000, ranges: [{minutes: 1}]}}}}` + held: "missingMinutes",
			} {
				Expect(create("malformed", spec)).To(MatchError(ContainSubstring(want)), spec)
			}
		})
	})

	Describe("claims", Ordered, func() {
		// A namespace of its own, so the checks only see these objects.
		const ns = "claims"
		r := &RuleReconciler{}
		BeforeAll(func() {
			r.Client, r.Scheme = k8sClient, k8sClient.Scheme()
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		})
		ready := func(name string) *metav1.Condition {
			key := types.NamespacedName{Namespace: ns, Name: name}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			var a homev1alpha1.Rule
			Expect(k8sClient.Get(ctx, key, &a)).To(Succeed())
			return meta.FindStatusCondition(a.Status.Conditions, homev1alpha1.ConditionReady)
		}
		rule := func(name, spec string) *homev1alpha1.Rule {
			var a homev1alpha1.Rule
			Expect(yaml.UnmarshalStrict([]byte("metadata: {name: "+name+", namespace: "+ns+"}\nspec:\n"+spec), &a)).To(Succeed())
			Expect(k8sClient.Create(ctx, &a)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, &a)
			return &a
		}
		n := 0
		device := func(name, room string, control ...homev1alpha1.CapabilityControl) *homev1alpha1.Device {
			n++
			d := &homev1alpha1.Device{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: homev1alpha1.DeviceSpec{
					Zigbee2MQTT: &homev1alpha1.ZigbeeDeviceSource{
						InstanceRef: corev1.LocalObjectReference{Name: "z2m"},
						IEEEAddress: fmt.Sprintf("0x00158d00000001%02d", n),
					},
					Control: control,
				},
			}
			if room != "" {
				d.Spec.RoomRef = &corev1.LocalObjectReference{Name: room}
			}
			Expect(k8sClient.Create(ctx, d)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, d)
			return d
		}
		switchControl := homev1alpha1.CapabilityControl{
			Capability: "on-off", Default: apiextensionsv1.JSON{Raw: []byte("false")},
			Resolve: homev1alpha1.ControlResolvePreferOn, External: homev1alpha1.ControlExternalRevert,
		}
		setControl := func(d *homev1alpha1.Device, control ...homev1alpha1.CapabilityControl) {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(d), d)).To(Succeed())
			d.Spec.Control = control
			Expect(k8sClient.Update(ctx, d)).To(Succeed())
		}

		It("requires a set on an arbitrated capability to end, until it isn't arbitrated", func() {
			rail := device("towel-rail", "", switchControl)
			rule("boost", `
  triggers: [{change: {device: b, capability: contact}}]
  do:
  - set: {device: towel-rail, capability: on-off, value: true}
  - set: {device: towel-rail, capability: on-off, value: true, for: 1h}`)
			cond := ready("boost")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonUnendedClaim))
			Expect(cond.Message).To(Equal("do[0] sets on-off on towel-rail, which is arbitrated, so the set needs for or until"))

			Expect(r.rulesInNamespace(ctx, rail)).To(ContainElement(
				reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "boost"}}))
			setControl(rail)
			Expect(ready("boost").Status).To(Equal(metav1.ConditionTrue))
		})

		It("reports two holds on one unarbitrated capability on both, until it's arbitrated", func() {
			fan := device("bathroom-fan", "bathroom")
			fan.Status.Capabilities = []homev1alpha1.DeviceCapability{{Name: "on-off", Kind: homev1alpha1.CapabilityKindBool, Writable: true}}
			Expect(k8sClient.Status().Update(ctx, fan)).To(Succeed())
			device("bathroom-lamp", "bathroom") // reports nothing, so a Room hold leaves it out
			rule("fan-humid", `
  while: {when: "true", offDelay: 10m}
  hold: {device: bathroom-fan, capability: on-off, value: true}`)
			rule("bathroom-quiet", `
  while: {window: {start: "22:00", end: "07:00"}}
  hold: {room: bathroom, capability: on-off, value: false}`)
			Expect(k8sClient.Create(ctx, &homev1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "bathroom", Namespace: ns}})).To(Succeed())

			for name, other := range map[string]string{"fan-humid": "bathroom-quiet", "bathroom-quiet": "fan-humid"} {
				cond := ready(name)
				Expect(cond.Status).To(Equal(metav1.ConditionFalse), name)
				Expect(cond.Reason).To(Equal(ReasonConflictingHolds))
				Expect(cond.Message).To(Equal("Holds on-off on bathroom-fan with Rule " + other +
					", which has no control entry to arbitrate them"))
			}

			setControl(fan, switchControl)
			Expect(ready("fan-humid").Status).To(Equal(metav1.ConditionTrue))
			Expect(ready("bathroom-quiet").Status).To(Equal(metav1.ConditionTrue))
		})

		It("needs a budget's target to have a control entry the budget fits", func() {
			rule("cellar-drying", `
  while:
    budget:
      minutes:
        bands:
          input: {device: cellar-humidity, capability: relative-humidity}
          missingMinutes: 180
          ranges: [{below: 55, minutes: 60}, {below: 65, minutes: 180}, {minutes: 360}]
      prefer: [{window: {start: "23:00", end: "07:00"}, score: 3}]
      forbid: [{window: {start: "18:00", end: "22:00"}}]
  hold: {device: cellar-dehumidifier, capability: on-off, value: true}`)
			cond := ready("cellar-drying")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonNoControl))
			Expect(cond.Message).To(Equal("Device cellar-dehumidifier not found"))

			dehumidifier := device("cellar-dehumidifier", "")
			cond = ready("cellar-drying")
			Expect(cond.Reason).To(Equal(ReasonNoControl))
			Expect(cond.Message).To(Equal("Device cellar-dehumidifier has no control entry for on-off, which a budget needs"))

			uneven := switchControl
			uneven.MinimumOn = &metav1.Duration{Duration: 45 * time.Minute}
			setControl(dehumidifier, uneven)
			cond = ready("cellar-drying")
			Expect(cond.Reason).To(Equal(ReasonInvalidBudget))
			Expect(cond.Message).To(ContainSubstring("minimumOn 45m0s isn't a multiple of budget.slotMinutes (30)"))

			even := switchControl
			even.MinimumOn, even.MinimumOff = &metav1.Duration{Duration: time.Hour}, &metav1.Duration{Duration: 30 * time.Minute}
			setControl(dehumidifier, even)
			Expect(ready("cellar-drying").Status).To(Equal(metav1.ConditionTrue))
		})

		It("reports bands out of order on the Rule", func() {
			device("loft-fan", "", switchControl)
			rule("loft-airing", `
  while:
    budget:
      minutes:
        bands:
          input: {device: loft-temperature, capability: temperature}
          missingMinutes: 60
          ranges: [{below: 20, minutes: 30}, {below: 15, minutes: 60}, {minutes: 120}]
  hold: {device: loft-fan, capability: on-off, value: true}`)
			cond := ready("loft-airing")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonInvalidBudget))
			Expect(cond.Message).To(ContainSubstring("ranges[1]: below must be greater than the band before"))
		})

		It("doesn't count holds on a Device that doesn't exist as a conflict", func() {
			for _, name := range []string{"ghost-a", "ghost-b"} {
				rule(name, `
  while: {when: "true"}
  hold: {device: ghost, capability: on-off, value: true}`)
			}
			Expect(ready("ghost-a").Status).To(Equal(metav1.ConditionTrue))
		})

		It("requires a release to name a Rule that can be released, until it does", func() {
			rule("reset", `
  triggers: [{change: {device: b, capability: contact}}]
  do: [{release: {rule: overheat}}, {release: {rule: plain}}]`)
			rule("plain", `
  while: {when: "true"}
  hold: {device: heater, capability: on-off, value: false}`)
			cond := ready("reset")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonInvalidRelease))
			Expect(cond.Message).To(Equal("Can't release: Rule overheat doesn't exist; " +
				"Rule plain has no latched hold or until: Released set"))

			overheat := rule("overheat", `
  priority: Safety
  while: {when: 'devices["t"]["temperature"] > 45', latch: true}
  hold: {device: heater2, capability: on-off, value: false}`)
			Expect(r.rulesInNamespace(ctx, overheat)).To(ContainElement(
				reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "reset"}}))
			Expect(ready("reset").Message).To(Equal("Can't release: Rule plain has no latched hold or until: Released set"))
		})

		It("reports a hold's missing Room and a bad window as before", func() {
			rule("no-room", `
  while: {when: "true"}
  hold: {room: attic, capability: on-off, value: true}`)
			Expect(ready("no-room").Reason).To(Equal("RoomNotFound"))
			rule("bad-window", `
  while: {window: {start: "6:00", end: "08:00"}}
  hold: {device: d, capability: on-off, value: true}`)
			cond := ready("bad-window")
			Expect(cond.Reason).To(Equal("Invalid"))
			Expect(cond.Message).To(ContainSubstring(`start: 6:00 is neither "HH:MM" nor {sun, offset}`))
		})

		It("reads back a window time that is neither a string nor a sun event, and reports it", func() {
			// The field has no schema, so the API server keeps a number. Decoding it mustn't fail,
			// or no client could list Rules.
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "home.oikade.dev/v1alpha1", "kind": "Rule",
				"metadata": map[string]any{"name": "number-window", "namespace": ns},
				"spec": map[string]any{
					"while": map[string]any{"window": map[string]any{"start": int64(6), "end": map[string]any{"sun": "Sunset", "ofset": "1h"}}},
					"hold":  map[string]any{"device": "d", "capability": "on-off", "value": true},
				},
			}}
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, u)
			var list homev1alpha1.RuleList
			Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
			cond := ready("number-window")
			Expect(cond.Reason).To(Equal("Invalid"))
			Expect(cond.Message).To(SatisfyAll(
				ContainSubstring(`start: 6 is neither`),
				ContainSubstring(`end: {"ofset":"1h","sun":"Sunset"} is neither`),
			))
		})
	})
})

// applySamples creates every object in a samples file in the namespace, decoded as kubectl
// would, and returns their names.
func applySamples(ns, file string, newObj func() client.Object) []string {
	f, err := os.Open(filepath.Join("..", "..", "..", "config", "samples", file))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(f.Close)
	docs := utilyaml.NewYAMLReader(bufio.NewReader(f))
	var names []string
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return names
		}
		Expect(err).NotTo(HaveOccurred())
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj := newObj()
		Expect(yaml.UnmarshalStrict(doc, obj)).To(Succeed())
		obj.SetNamespace(ns)
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, obj)
		names = append(names, obj.GetName())
	}
}
