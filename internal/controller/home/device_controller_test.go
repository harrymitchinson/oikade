package home

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

var _ = Describe("Device Controller", func() {
	zigbee := func(n int) *homev1alpha1.ZigbeeDeviceSource {
		return &homev1alpha1.ZigbeeDeviceSource{
			InstanceRef: corev1.LocalObjectReference{Name: "z2m"},
			IEEEAddress: "0x000b57fffe0001" + string(rune('0'+n/10)) + string(rune('0'+n%10)),
		}
	}
	entry := func(capability, def string, resolve homev1alpha1.ControlResolve) homev1alpha1.CapabilityControl {
		return homev1alpha1.CapabilityControl{
			Capability: capability, Default: apiextensionsv1.JSON{Raw: []byte(def)},
			Resolve: resolve, External: homev1alpha1.ControlExternalRevert,
		}
	}
	n := 0
	create := func(name string, control ...homev1alpha1.CapabilityControl) (*homev1alpha1.Device, error) {
		n++
		d := &homev1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       homev1alpha1.DeviceSpec{Zigbee2MQTT: zigbee(n), Control: control},
		}
		err := k8sClient.Create(ctx, d)
		if err == nil {
			DeferCleanup(k8sClient.Delete, ctx, d)
		}
		return d, err
	}
	ready := func(name string) *metav1.Condition {
		key := types.NamespacedName{Namespace: "default", Name: name}
		r := &DeviceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var d homev1alpha1.Device
		Expect(k8sClient.Get(ctx, key, &d)).To(Succeed())
		cond := meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.ObservedGeneration).To(Equal(d.Generation))
		return cond
	}

	It("leaves a Device without control unchanged, and Ready", func() {
		d, err := create("plain")
		Expect(err).NotTo(HaveOccurred())
		spec := d.Spec.DeepCopy()
		Expect(ready("plain").Status).To(Equal(metav1.ConditionTrue))

		var got homev1alpha1.Device
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(d), &got)).To(Succeed())
		Expect(got.Spec).To(Equal(*spec))
		Expect(got.Spec.Control).To(BeNil())
	})

	It("marks valid control entries Ready", func() {
		light := entry("light.on", "false", homev1alpha1.ControlResolvePreferOn)
		light.External, light.OverrideHold = homev1alpha1.ControlExternalOverride, homev1alpha1.OverrideHoldNext
		level := entry("light.level", "40", homev1alpha1.ControlResolveHighest)
		level.External, level.OverrideHold = homev1alpha1.ControlExternalOverride, "1h30m"
		level.MinimumOn, level.MinimumOff = &metav1.Duration{Duration: time.Minute}, &metav1.Duration{Duration: time.Minute}
		level.MaximumOn, level.MaximumStartsPerDay = &metav1.Duration{Duration: time.Hour}, ptr.To[int32](4)
		_, err := create("valid-control", light, level)
		Expect(err).NotTo(HaveOccurred())
		cond := ready("valid-control")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue), cond.Message)
	})

	It("reports entries that don't fit their capability, naming it", func() {
		_, err := create("bad-control",
			entry("sensor.temperature", "20", homev1alpha1.ControlResolveHighest),
			entry("light.on", "false", homev1alpha1.ControlResolveHighest),
			entry("light.level", "true", homev1alpha1.ControlResolveLowest),
		)
		Expect(err).NotTo(HaveOccurred())
		cond := ready("bad-control")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("InvalidControl"))
		Expect(cond.Message).To(SatisfyAll(
			ContainSubstring("control sensor.temperature: capability is read-only"),
			ContainSubstring("control light.on: resolve must be PreferOn or PreferOff"),
			ContainSubstring("control light.level: default: light.level takes a number"),
		))
	})

	It("clears Ready=False once the entry is fixed", func() {
		d, err := create("fixed-control", entry("switch.on", "1", homev1alpha1.ControlResolvePreferOn))
		Expect(err).NotTo(HaveOccurred())
		Expect(ready("fixed-control").Status).To(Equal(metav1.ConditionFalse))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(d), d)).To(Succeed())
		d.Spec.Control[0].Default = apiextensionsv1.JSON{Raw: []byte("true")}
		Expect(k8sClient.Update(ctx, d)).To(Succeed())
		Expect(ready("fixed-control").Status).To(Equal(metav1.ConditionTrue))
	})

	It("rejects malformed entries at apply time", func() {
		ok := func() homev1alpha1.CapabilityControl {
			return entry("switch.on", "false", homev1alpha1.ControlResolvePreferOn)
		}
		with := func(f func(*homev1alpha1.CapabilityControl)) homev1alpha1.CapabilityControl {
			c := ok()
			f(&c)
			return c
		}
		for name, tc := range map[string]struct {
			control []homev1alpha1.CapabilityControl
			want    string
		}{
			"duplicate": {[]homev1alpha1.CapabilityControl{ok(), ok()}, "Duplicate value"},
			"bad overrideHold": {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) {
				c.External, c.OverrideHold = homev1alpha1.ControlExternalOverride, "soon"
			})}, "spec.control[0].overrideHold"},
			"override, no hold":  {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) { c.External = homev1alpha1.ControlExternalOverride })}, "overrideHold is required when external is Override"},
			"revert with a hold": {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) { c.OverrideHold = "1h" })}, "overrideHold has no effect when external is Revert"},
			"zero overrideHold": {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) {
				c.External, c.OverrideHold = homev1alpha1.ControlExternalOverride, "0s"
			})}, "overrideHold must be positive"},
			"unknown resolve":     {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) { c.Resolve = "Newest" })}, "spec.control[0].resolve"},
			"missing external":    {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) { c.External = "" })}, "spec.control[0].external"},
			"negative minimumOn":  {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) { c.MinimumOn = &metav1.Duration{Duration: -time.Minute} })}, "minimumOn must not be negative"},
			"zero maximumOn":      {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) { c.MaximumOn = &metav1.Duration{} })}, "maximumOn must be positive"},
			"negative minimumOff": {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) { c.MinimumOff = &metav1.Duration{Duration: -time.Minute} })}, "minimumOff must not be negative"},
			"zero starts":         {[]homev1alpha1.CapabilityControl{with(func(c *homev1alpha1.CapabilityControl) { c.MaximumStartsPerDay = ptr.To[int32](0) })}, "spec.control[0].maximumStartsPerDay"},
		} {
			_, err := create("malformed", tc.control...)
			Expect(err).To(MatchError(ContainSubstring(tc.want)), name)
		}
	})

	It("rejects a default that is missing", func() {
		c := entry("switch.on", "false", homev1alpha1.ControlResolvePreferOn)
		c.Default = apiextensionsv1.JSON{}
		_, err := create("no-default", c)
		Expect(err).To(MatchError(ContainSubstring("spec.control[0].default")))
	})

	It("accepts status.control as the runtime will write it", func() {
		d, err := create("control-status", entry("switch.on", "true", homev1alpha1.ControlResolvePreferOff))
		Expect(err).NotTo(HaveOccurred())
		d.Status.Control = []homev1alpha1.CapabilityControlStatus{{
			Capability: "switch.on",
			Value:      &apiextensionsv1.JSON{Raw: []byte("false")},
			Level:      homev1alpha1.ControlLevelSafety,
			Claims: []homev1alpha1.ControlClaim{
				{Source: "Rule/leak-shutoff", Level: homev1alpha1.ControlLevelSafety, Value: apiextensionsv1.JSON{Raw: []byte("false")}, Latched: true, Winner: true},
				{Source: "HomeKit", Level: homev1alpha1.ControlLevelOverride, Value: apiextensionsv1.JSON{Raw: []byte("true")}, Reverted: true,
					Expires: metav1.NewTime(time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC))},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())

		var got homev1alpha1.Device
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(d), &got)).To(Succeed())
		Expect(got.Status.Control).To(HaveLen(1))
		Expect(got.Status.Control[0].Claims).To(HaveLen(2))
		Expect(got.Status.Control[0].Claims[0].Winner).To(BeTrue())
		Expect(string(got.Status.Control[0].Claims[1].Value.Raw)).To(Equal("true"))
	})
})
