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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
)

var _ = Describe("Automation Controller", func() {
	reconcileReady := func(name, condition string, value string) *metav1.Condition {
		key := types.NamespacedName{Namespace: "default", Name: name}
		Expect(k8sClient.Create(ctx, &homev1alpha1.Automation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: homev1alpha1.AutomationSpec{
				Triggers:  []homev1alpha1.Trigger{{Device: "hallway-motion", Capability: "sensor.occupancy-detected"}},
				Condition: condition,
				Actions: []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
					Device: "hallway-light", Capability: "light.level", Value: apiextensionsv1.JSON{Raw: []byte(value)},
				}}},
			},
		})).To(Succeed())
		r := &AutomationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var a homev1alpha1.Automation
		Expect(k8sClient.Get(ctx, key, &a)).To(Succeed())
		Expect(a.Status.ObservedGeneration).To(Equal(a.Generation))
		return meta.FindStatusCondition(a.Status.Conditions, homev1alpha1.ConditionReady)
	}

	It("marks a valid Automation Ready", func() {
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
		err := k8sClient.Create(ctx, &homev1alpha1.Automation{
			ObjectMeta: metav1.ObjectMeta{Name: "both", Namespace: "default"},
			Spec: homev1alpha1.AutomationSpec{
				Triggers: []homev1alpha1.Trigger{{Device: "d", Capability: "light.on"}},
				Actions: []homev1alpha1.Action{{
					Set:   &homev1alpha1.SetAction{Device: "d", Capability: "light.on", Value: apiextensionsv1.JSON{Raw: []byte("true")}},
					Delay: &metav1.Duration{Duration: 1},
				}},
			},
		})
		Expect(err).To(MatchError(ContainSubstring("exactly one of set or delay")))
	})

	Describe("device or room", func() {
		create := func(name string, trigger homev1alpha1.Trigger, set homev1alpha1.SetAction) error {
			trigger.Capability = "sensor.occupancy-detected"
			set.Capability, set.Value = "light.on", apiextensionsv1.JSON{Raw: []byte("true")}
			return k8sClient.Create(ctx, &homev1alpha1.Automation{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: homev1alpha1.AutomationSpec{
					Triggers: []homev1alpha1.Trigger{trigger},
					Actions:  []homev1alpha1.Action{{Set: &set}},
				},
			})
		}

		It("accepts a device or a room on triggers and set actions", func() {
			Expect(create("device-device", homev1alpha1.Trigger{Device: "m"}, homev1alpha1.SetAction{Device: "l"})).To(Succeed())
			Expect(create("room-room", homev1alpha1.Trigger{Room: "hallway"}, homev1alpha1.SetAction{Room: "hallway"})).To(Succeed())
			Expect(create("device-room", homev1alpha1.Trigger{Device: "m"}, homev1alpha1.SetAction{Room: "hallway"})).To(Succeed())
		})

		It("rejects a trigger with neither or both", func() {
			Expect(create("trigger-neither", homev1alpha1.Trigger{}, homev1alpha1.SetAction{Device: "l"})).
				To(MatchError(ContainSubstring("exactly one of device or room")))
			Expect(create("trigger-both", homev1alpha1.Trigger{Device: "m", Room: "hallway"}, homev1alpha1.SetAction{Device: "l"})).
				To(MatchError(ContainSubstring("exactly one of device or room")))
		})

		It("rejects a set action with neither or both", func() {
			Expect(create("set-neither", homev1alpha1.Trigger{Device: "m"}, homev1alpha1.SetAction{})).
				To(MatchError(ContainSubstring("exactly one of device or room")))
			Expect(create("set-both", homev1alpha1.Trigger{Device: "m"}, homev1alpha1.SetAction{Device: "l", Room: "hallway"})).
				To(MatchError(ContainSubstring("exactly one of device or room")))
		})
	})
})
