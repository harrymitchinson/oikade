package matter

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	homecontroller "github.com/harrymitchinson/oikade/internal/controller/home"
)

// A reset device's new ID moves onto its old Device with oikade.dev/replace-with (OIK-129), so the
// Device keeps its name and with it its Matter endpoint.
var _ = Describe("Replacing an adopted device's ID", func() {
	It("keeps the old Device's Matter endpoint", func() {
		t := newTestNS()
		r := &BridgeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Image: "oikade:test", MQTTURL: "mqtt://broker:1883"}
		adopt := func() {
			a := &homecontroller.AdoptReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Driver: homecontroller.AdoptPlugin}
			_, err := a.Reconcile(ctx, ctrl.Request{NamespacedName: t.key("hue")})
			Expect(err).NotTo(HaveOccurred())
		}
		setDiscovered := func(p *pluginsv1alpha1.Plugin, ids ...string) {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
			p.Status.Discovered = nil
			for _, id := range ids {
				p.Status.Discovered = append(p.Status.Discovered, homev1alpha1.DiscoveredDevice{ID: id, Name: "Desk lamp"})
			}
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		}

		p := &pluginsv1alpha1.Plugin{
			ObjectMeta: metav1.ObjectMeta{Name: "hue", Namespace: t.ns},
			Spec: pluginsv1alpha1.PluginSpec{
				Image: "example.com/oikade-plugin-hue:test", Discovery: &homev1alpha1.DiscoverySpec{Adopt: true},
			},
		}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		setDiscovered(p, "3f2a9c71")
		adopt()
		t.createBridge("main", 10)
		b := reconcileBridge(r, t.key("main"))
		Expect(endpoints(b)).To(Equal(map[string]int32{"desk-lamp": 2}))

		// Reset, the lamp comes back with a new ID and is adopted as desk-lamp-2.
		setDiscovered(p, "9d01")
		adopt()
		b = reconcileBridge(r, t.key("main"))
		Expect(endpoints(b)).To(Equal(map[string]int32{"desk-lamp": 2, "desk-lamp-2": 3}))

		d := get(t.key("desk-lamp"), &homev1alpha1.Device{})
		d.Annotations = map[string]string{homev1alpha1.AnnotationReplaceWith: "desk-lamp-2"}
		Expect(k8sClient.Update(ctx, d)).To(Succeed())
		adopt()
		Expect(get(t.key("desk-lamp"), &homev1alpha1.Device{}).Spec.Plugin.ID).To(Equal("9d01"))

		b = reconcileBridge(r, t.key("main"))
		Expect(endpoints(b)).To(HaveKeyWithValue("desk-lamp", int32(2)))
		Expect(b.Status.Devices).To(Equal(int32(1)), "desk-lamp-2 is gone; its endpoint ID stays reserved")
	})
})
