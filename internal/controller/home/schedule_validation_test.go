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
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

var _ = Describe("Schedule validation", func() {
	target := homev1alpha1.CapabilityRef{Device: "pool-pump", Capability: "switch.on"}
	window := &homev1alpha1.TimeWindow{Start: "22:00", End: "06:00"}
	bands := &homev1alpha1.MinutesBands{
		Input:          homev1alpha1.MinutesInput{CapabilityRef: homev1alpha1.CapabilityRef{Device: "pool-probe", Capability: "sensor.temperature"}},
		MissingMinutes: 240,
		Ranges: []homev1alpha1.MinutesBand{
			{Below: new(resource.MustParse("12")), Minutes: 120},
			{Minutes: 480},
		},
	}
	create := func(name string, spec homev1alpha1.ScheduleSpec) (*homev1alpha1.Schedule, error) {
		s := &homev1alpha1.Schedule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
		err := k8sClient.Create(ctx, s)
		if err == nil {
			DeferCleanup(k8sClient.Delete, ctx, s)
		}
		return s, err
	}
	withRule := func(rule homev1alpha1.ScheduleRule) homev1alpha1.ScheduleSpec {
		return homev1alpha1.ScheduleSpec{Target: target, Minutes: &homev1alpha1.ScheduleMinutes{Bands: bands}, Rules: []homev1alpha1.ScheduleRule{rule}}
	}
	const oneEffect = "exactly one of require, forbid or prefer must be set"
	const liveRule = "live rules must set require and when"
	const oneMinutes = "exactly one of bands or expression must be set"

	It("defaults planningTime, slotMinutes, maxAge and the override duration", func() {
		s, err := create("defaults", homev1alpha1.ScheduleSpec{
			Target:  target,
			Minutes: &homev1alpha1.ScheduleMinutes{Bands: bands},
			Rules:   []homev1alpha1.ScheduleRule{{Name: "off-peak", Window: window, Prefer: ptr.To[int32](3)}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Spec.PlanningTime).To(Equal(homev1alpha1.TimeOfDay("00:00")))
		Expect(s.Spec.SlotMinutes).To(Equal(int32(30)))
		Expect(s.Spec.Minutes.Bands.Input.MaxAge.Duration).To(Equal(30 * time.Minute))
		Expect(s.Spec.Override.Duration.Duration).To(Equal(2 * time.Hour))
	})

	It("accepts each effect on its own", func() {
		for name, rule := range map[string]homev1alpha1.ScheduleRule{
			"effect-require":     {Name: "r", Window: window, Require: true},
			"effect-forbid":      {Name: "r", Window: window, Forbid: true},
			"effect-prefer":      {Name: "r", Window: window, Prefer: ptr.To[int32](-2)},
			"effect-prefer-zero": {Name: "r", Prefer: ptr.To[int32](0)},
			"effect-live":        {Name: "r", Live: true, When: `devices["outdoor"]["sensor.temperature"] < 1.0`, Require: true},
		} {
			_, err := create(name, withRule(rule))
			Expect(err).NotTo(HaveOccurred(), name)
		}
	})

	It("rejects rules with zero or several effects", func() {
		for name, rule := range map[string]homev1alpha1.ScheduleRule{
			"effect-none":           {Name: "r", Window: window},
			"effect-require-forbid": {Name: "r", Require: true, Forbid: true},
			"effect-require-prefer": {Name: "r", Require: true, Prefer: ptr.To[int32](1)},
			"effect-forbid-prefer":  {Name: "r", Forbid: true, Prefer: ptr.To[int32](1)},
		} {
			_, err := create(name, withRule(rule))
			Expect(err).To(MatchError(ContainSubstring(oneEffect)), name)
		}
	})

	It("rejects live rules without require and when", func() {
		for name, rule := range map[string]homev1alpha1.ScheduleRule{
			"live-no-when":    {Name: "r", Live: true, Require: true},
			"live-forbid":     {Name: "r", Live: true, When: "true", Forbid: true},
			"live-prefer":     {Name: "r", Live: true, When: "true", Prefer: ptr.To[int32](1)},
			"live-forbid-nil": {Name: "r", Live: true, Forbid: true},
		} {
			_, err := create(name, withRule(rule))
			Expect(err).To(MatchError(ContainSubstring(liveRule)), name)
		}
	})

	It("rejects minutes with both or neither of bands and expression", func() {
		for name, minutes := range map[string]*homev1alpha1.ScheduleMinutes{
			"minutes-neither": {},
			"minutes-both":    {Bands: bands, Expression: "240"},
		} {
			_, err := create(name, homev1alpha1.ScheduleSpec{Target: target, Minutes: minutes})
			Expect(err).To(MatchError(ContainSubstring(oneMinutes)), name)
		}
		_, err := create("minutes-expression", homev1alpha1.ScheduleSpec{
			Target: target, Minutes: &homev1alpha1.ScheduleMinutes{Expression: "240"},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects malformed windows, slot sizes and rule names", func() {
		for name, spec := range map[string]homev1alpha1.ScheduleSpec{
			"window-empty": {Target: target, Rules: []homev1alpha1.ScheduleRule{
				{Name: "r", Window: &homev1alpha1.TimeWindow{Start: "06:00", End: "06:00"}, Require: true},
			}},
			"window-bad-time": {Target: target, Rules: []homev1alpha1.ScheduleRule{
				{Name: "r", Window: &homev1alpha1.TimeWindow{Start: "24:00", End: "06:00"}, Require: true},
			}},
			"window-bad-day": {Target: target, Rules: []homev1alpha1.ScheduleRule{
				{Name: "r", Window: &homev1alpha1.TimeWindow{Start: "22:00", End: "06:00", Days: []homev1alpha1.Weekday{"Monday"}}, Require: true},
			}},
			"rule-bad-name": {Target: target, Rules: []homev1alpha1.ScheduleRule{{Name: "Off_Peak", Require: true}}},
			"slot-minutes":  {Target: target, SlotMinutes: 20},
			"planning-time": {Target: target, PlanningTime: "0:00"},
			"no-target":     {},
		} {
			_, err := create(name, spec)
			Expect(err).To(HaveOccurred(), name)
		}
	})

	It("rejects two rules with the same name", func() {
		_, err := create("rule-duplicate", homev1alpha1.ScheduleSpec{Target: target, Rules: []homev1alpha1.ScheduleRule{
			{Name: "r", Window: window, Require: true},
			{Name: "r", Require: true, Live: true, When: "true"},
		}})
		Expect(err).To(MatchError(ContainSubstring("Duplicate value")))
	})

	It("accepts every Schedule in the samples", func() {
		f, err := os.Open(filepath.Join("..", "..", "..", "config", "samples", "home_v1alpha1_schedule.yaml"))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = f.Close() }()

		dec := yaml.NewYAMLOrJSONDecoder(f, 4096)
		n := 0
		for {
			var raw runtime.RawExtension
			if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
				break
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			var meta metav1.TypeMeta
			Expect(yaml.Unmarshal(raw.Raw, &meta)).To(Succeed())
			name := ""
			switch meta.Kind {
			case "Schedule":
				var s homev1alpha1.Schedule
				Expect(yaml.Unmarshal(raw.Raw, &s)).To(Succeed())
				name = "sample-" + s.Name
				_, err = create(name, s.Spec)
			case "Device":
				var d homev1alpha1.Device
				Expect(yaml.Unmarshal(raw.Raw, &d)).To(Succeed())
				d.ObjectMeta = metav1.ObjectMeta{Name: "sample-" + d.Name, Namespace: "default"}
				name = d.Name
				err = k8sClient.Create(ctx, &d)
				if err == nil {
					DeferCleanup(k8sClient.Delete, ctx, &d)
				}
			default:
				Fail("unexpected kind " + meta.Kind)
			}
			Expect(err).NotTo(HaveOccurred(), name)
			n++
		}
		Expect(n).To(Equal(3))
	})
})
