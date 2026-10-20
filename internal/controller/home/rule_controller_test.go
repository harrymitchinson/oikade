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

package home

import (
	"bufio"
	"bytes"
	"errors"
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
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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
					Device: "hallway-motion", Capability: "sensor.occupancy-detected",
				}}},
				Condition: condition,
				Do: []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
					Device: "hallway-light", Capability: "light.level", Value: apiextensionsv1.JSON{Raw: []byte(value)},
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
				Triggers: []homev1alpha1.Trigger{{Change: &homev1alpha1.ChangeTrigger{Device: "d", Capability: "light.on"}}},
				Do: []homev1alpha1.Action{{
					Set:   &homev1alpha1.SetAction{Device: "d", Capability: "light.on", Value: apiextensionsv1.JSON{Raw: []byte("true")}},
					Delay: &metav1.Duration{Duration: 1},
				}},
			},
		})
		Expect(err).To(MatchError(ContainSubstring("exactly one of set or delay")))
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
						{Change: &homev1alpha1.ChangeTrigger{Room: "livingroom", Capability: "sensor.occupancy-detected"}},
						{Change: &homev1alpha1.ChangeTrigger{Device: "hallway-motion", Capability: "sensor.occupancy-detected"}},
					},
					Do: []homev1alpha1.Action{
						{Set: &homev1alpha1.SetAction{Room: "livingroom", Capability: "light.on", Value: apiextensionsv1.JSON{Raw: []byte(value)}}},
						{Set: &homev1alpha1.SetAction{Room: "kitchen", Capability: "light.on", Value: apiextensionsv1.JSON{Raw: []byte(value)}}},
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
		Expect(names).To(ConsistOf("hallway-motion-light", "goodnight-living-room", "porch-light-sunset"))
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
			trigger.Capability = "sensor.occupancy-detected"
			set.Capability, set.Value = "light.on", apiextensionsv1.JSON{Raw: []byte("true")}
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
})
