package home

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

var _ = Describe("Device spec.matter validation", func() {
	It("accepts each spec.matter device type, and rejects an unknown one", func() {
		device := func(name string, t homev1alpha1.MatterDeviceType) *homev1alpha1.Device {
			return &homev1alpha1.Device{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: homev1alpha1.DeviceSpec{
					Plugin: &homev1alpha1.PluginDeviceSource{PluginRef: corev1.LocalObjectReference{Name: "relays"}, ID: name},
					Matter: &homev1alpha1.DeviceMatter{DeviceType: t},
				},
			}
		}
		for i, t := range []homev1alpha1.MatterDeviceType{
			homev1alpha1.MatterDeviceTypeLight, homev1alpha1.MatterDeviceTypePlugInUnit, homev1alpha1.MatterDeviceTypeMountedControl,
		} {
			d := device("matter-ok-"+string(rune('a'+i)), t)
			Expect(k8sClient.Create(ctx, d)).To(Succeed(), string(t))
			DeferCleanup(k8sClient.Delete, ctx, d)
		}
		for i, t := range []homev1alpha1.MatterDeviceType{"Lightbulb", "OnOffLight", ""} {
			Expect(k8sClient.Create(ctx, device("matter-bad-"+string(rune('a'+i)), t))).
				To(MatchError(ContainSubstring("spec.matter.deviceType")), string(t))
		}
	})

})
