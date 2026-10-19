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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

var _ = Describe("Device validation", func() {
	zigbee := &homev1alpha1.ZigbeeDeviceSource{
		InstanceRef: corev1.LocalObjectReference{Name: "z2m"},
		IEEEAddress: "0x000b57fffe000001",
	}
	plugin := func(id string) *homev1alpha1.PluginDeviceSource {
		return &homev1alpha1.PluginDeviceSource{PluginRef: corev1.LocalObjectReference{Name: "sonoff"}, ID: id}
	}
	schedule := &homev1alpha1.ScheduleDeviceSource{ScheduleRef: corev1.LocalObjectReference{Name: "pool-filtration"}}
	create := func(name string, spec homev1alpha1.DeviceSpec) error {
		d := &homev1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
		err := k8sClient.Create(ctx, d)
		if err == nil {
			DeferCleanup(k8sClient.Delete, ctx, d)
		}
		return err
	}
	const oneSource = "exactly one driver source (zigbee2mqtt, plugin or schedule) is required"

	It("accepts exactly one driver source", func() {
		Expect(create("source-zigbee", homev1alpha1.DeviceSpec{Zigbee2MQTT: zigbee})).To(Succeed())
		Expect(create("source-plugin", homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
			PluginRef:  corev1.LocalObjectReference{Name: "sonoff"},
			ID:         "1000aabbcc",
			Parameters: map[string]string{"model": "MINIR4", "capability": "light.on"},
		}})).To(Succeed())
		Expect(create("source-schedule", homev1alpha1.DeviceSpec{Schedule: schedule})).To(Succeed())
	})

	It("rejects neither or both driver sources", func() {
		Expect(create("source-neither", homev1alpha1.DeviceSpec{})).
			To(MatchError(ContainSubstring(oneSource)))
		Expect(create("source-both", homev1alpha1.DeviceSpec{Zigbee2MQTT: zigbee, Plugin: plugin("1000aabbcc")})).
			To(MatchError(ContainSubstring(oneSource)))
		Expect(create("source-schedule-zigbee", homev1alpha1.DeviceSpec{Zigbee2MQTT: zigbee, Schedule: schedule})).
			To(MatchError(ContainSubstring(oneSource)))
		Expect(create("source-schedule-plugin", homev1alpha1.DeviceSpec{Plugin: plugin("1000aabbcc"), Schedule: schedule})).
			To(MatchError(ContainSubstring(oneSource)))
	})

	It("rejects an empty scheduleRef name", func() {
		Expect(create("schedule-ref-empty", homev1alpha1.DeviceSpec{Schedule: &homev1alpha1.ScheduleDeviceSource{}})).
			To(MatchError(ContainSubstring("scheduleRef.name is required")))
	})

	It("accepts topic-safe plugin ids", func() {
		for i, id := range []string{"a", "1000aabbcc", "pool_pump.2-b", "A" + strings.Repeat("b", 62)} {
			Expect(create("id-ok-"+string(rune('a'+i)), homev1alpha1.DeviceSpec{Plugin: plugin(id)})).To(Succeed(), id)
		}
	})

	It("rejects plugin ids that are not topic-safe", func() {
		for i, id := range []string{"", "-leading", ".leading", "has/slash", "has+plus", "has#hash", "has space", strings.Repeat("a", 64)} {
			Expect(create("id-bad-"+string(rune('a'+i)), homev1alpha1.DeviceSpec{Plugin: plugin(id)})).
				To(MatchError(ContainSubstring("spec.plugin.id")), id)
		}
	})

	It("rejects more than 32 parameters", func() {
		params := map[string]string{}
		for i := range 33 {
			params[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
		}
		src := plugin("many-params")
		src.Parameters = params
		Expect(create("params-too-many", homev1alpha1.DeviceSpec{Plugin: src})).
			To(MatchError(ContainSubstring("spec.plugin.parameters")))
	})

	It("rejects an empty pluginRef name", func() {
		src := plugin("no-ref")
		src.PluginRef.Name = ""
		Expect(create("ref-empty", homev1alpha1.DeviceSpec{Plugin: src})).
			To(MatchError(ContainSubstring("pluginRef.name is required")))
	})

	It("bounds parameter keys and values", func() {
		withParams := func(params map[string]string) homev1alpha1.DeviceSpec {
			src := plugin("bounded")
			src.Parameters = params
			return homev1alpha1.DeviceSpec{Plugin: src}
		}
		Expect(create("params-ok", withParams(map[string]string{
			"model": "MINIR4", strings.Repeat("k", 63): strings.Repeat("v", 1024),
		}))).To(Succeed())
		for i, key := range []string{"has space", "has/slash", strings.Repeat("k", 64)} {
			Expect(create("params-bad-key-"+string(rune('a'+i)), withParams(map[string]string{key: "v"}))).
				To(MatchError(ContainSubstring("parameter keys must be")), key)
		}
		Expect(create("params-long-value", withParams(map[string]string{"model": strings.Repeat("v", 1025)}))).
			To(MatchError(ContainSubstring("parameter values must be at most 1024 characters")))
	})

	It("accepts every Device in the samples", func() {
		f, err := os.Open(filepath.Join("..", "..", "..", "config", "samples", "home_v1alpha1_device.yaml"))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = f.Close() }()

		dec := yaml.NewYAMLOrJSONDecoder(f, 4096)
		n := 0
		for {
			var d homev1alpha1.Device
			if err := dec.Decode(&d); errors.Is(err, io.EOF) {
				break
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			d.Name = "sample-" + d.Name
			Expect(create(d.Name, d.Spec)).To(Succeed(), d.Name)
			n++
		}
		Expect(n).To(BeNumerically(">", 1))
	})
})
