package matter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/render"
)

// Ports are unique cluster-wide, so every test gets its own namespace and port range.
var testSeq atomic.Int32

type testNS struct {
	ns    string
	start int32
}

func newTestNS() testNS {
	n := testSeq.Add(1)
	t := testNS{ns: fmt.Sprintf("matter-%d", n), start: 54000 + n*10}
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: t.ns}})).To(Succeed())
	return t
}

func (t testNS) createBridge(name string, size int32, mutate ...func(*matterv1alpha1.Bridge)) {
	b := &matterv1alpha1.Bridge{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: t.ns},
		Spec:       matterv1alpha1.BridgeSpec{PortRange: matterv1alpha1.PortRange{Start: t.start, End: t.start + size - 1}},
	}
	for _, m := range mutate {
		m(b)
	}
	Expect(k8sClient.Create(ctx, b)).To(Succeed())
}

func (t testNS) createDevice(name string, labels map[string]string) {
	Expect(k8sClient.Create(ctx, &homev1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: t.ns, Labels: labels},
		Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
			PluginRef: corev1.LocalObjectReference{Name: "relays"}, ID: name,
		}},
	})).To(Succeed())
}

func (t testNS) deleteDevice(name string) {
	Expect(k8sClient.Delete(ctx, &homev1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: t.ns}})).To(Succeed())
}

func (t testNS) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: t.ns, Name: name}
}

func reconcileBridge(r *BridgeReconciler, key types.NamespacedName) *matterv1alpha1.Bridge {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var b matterv1alpha1.Bridge
	if err := k8sClient.Get(ctx, key, &b); err != nil {
		return nil
	}
	return &b
}

func get[T client.Object](key types.NamespacedName, obj T) T {
	ExpectWithOffset(1, k8sClient.Get(ctx, key, obj)).To(Succeed())
	return obj
}

func endpoints(b *matterv1alpha1.Bridge) map[string]int32 {
	out := map[string]int32{}
	for _, e := range b.Status.Endpoints {
		out[e.Device] = e.Endpoint
	}
	return out
}

func readyReason(b *matterv1alpha1.Bridge) string {
	return meta.FindStatusCondition(b.Status.Conditions, ConditionReady).Reason
}

var _ = Describe("Bridge Controller", func() {
	var r *BridgeReconciler

	BeforeEach(func() {
		r = &BridgeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Image: "oikade:test"}
	})

	It("provisions the Secret, port, PVC and Deployment, and shows the pairing code", func() {
		t := newTestNS()
		t.createBridge("main", 10, func(b *matterv1alpha1.Bridge) { b.Spec.DisplayName = "Upstairs" })
		b := reconcileBridge(r, t.key("main"))

		Expect(b.Status.Port).To(Equal(t.start))
		Expect(meta.IsStatusConditionTrue(b.Status.Conditions, ConditionPortAllocated)).To(BeTrue())
		Expect(readyReason(b)).To(Equal("Progressing"))

		secret := get(t.key("main-matter"), &corev1.Secret{})
		Expect(secret.OwnerReferences).To(BeEmpty())
		Expect(secret.Labels).To(HaveKeyWithValue(matterv1alpha1.LabelBridge, "main"))
		for _, k := range render.MatterSetupKeys {
			Expect(secret.Data).To(HaveKey(k))
		}
		Expect(string(secret.Data[render.MatterUniqueIDKey])).To(MatchRegexp(`^[0-9A-F]{32}$`))
		disc, err := strconv.Atoi(string(secret.Data[render.MatterDiscriminatorKey]))
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Status.Discriminator).To(HaveValue(Equal(int32(disc))))
		Expect(b.Status.ManualPairingCode).To(MatchRegexp(`^[0-9]{11}$`))
		Expect(b.Status.QRCode).To(HavePrefix("MT:"))

		pvc := get(t.key("main-matter"), &corev1.PersistentVolumeClaim{})
		Expect(pvc.OwnerReferences).To(BeEmpty())
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("100Mi"))

		dep := get(t.key("main-matter"), &appsv1.Deployment{})
		Expect(dep.OwnerReferences).To(ConsistOf(HaveField("Name", "main")))
		Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		pod := dep.Spec.Template.Spec
		Expect(pod.HostNetwork).To(BeTrue())
		Expect(*pod.AutomountServiceAccountToken).To(BeFalse())
		c := pod.Containers[0]
		Expect(c.Image).To(Equal("oikade:test"))
		Expect(c.Command).To(Equal([]string{"/manager", "matter"}))
		Expect(c.Args).To(ContainElements("--name=Upstairs", fmt.Sprintf("--port=%d", t.start)))
		Expect(c.Ports[0].Protocol).To(Equal(corev1.ProtocolUDP))
		Expect(c.Ports[0].HostPort).To(Equal(t.start))
		Expect(pod.Volumes).To(ContainElement(HaveField("Secret.SecretName", "main-matter")))
		Expect(pod.Volumes).To(ContainElement(HaveField("PersistentVolumeClaim.ClaimName", "main-matter")))

		By("keeping the setup code on the next reconcile")
		again := reconcileBridge(r, t.key("main"))
		Expect(again.Status.ManualPairingCode).To(Equal(b.Status.ManualPairingCode))
	})

	It("never reuses or moves endpoint IDs", func() {
		t := newTestNS()
		t.createDevice("b-lamp", nil)
		t.createDevice("a-sensor", nil)
		t.createBridge("main", 10)
		b := reconcileBridge(r, t.key("main"))
		Expect(endpoints(b)).To(Equal(map[string]int32{"a-sensor": 2, "b-lamp": 3}))
		Expect(b.Status.NextEndpoint).To(Equal(int32(4)))
		Expect(b.Status.Devices).To(Equal(int32(2)))

		By("deleting a Device and adding another")
		t.deleteDevice("a-sensor")
		t.createDevice("c-plug", nil)
		b = reconcileBridge(r, t.key("main"))
		Expect(endpoints(b)).To(Equal(map[string]int32{"a-sensor": 2, "b-lamp": 3, "c-plug": 4}))
		Expect(b.Status.Devices).To(Equal(int32(2)))

		By("re-creating the deleted Device")
		t.createDevice("a-sensor", nil)
		b = reconcileBridge(r, t.key("main"))
		Expect(endpoints(b)).To(Equal(map[string]int32{"a-sensor": 2, "b-lamp": 3, "c-plug": 4}))
		Expect(b.Status.NextEndpoint).To(Equal(int32(5)))

		By("backing them up in the Secret")
		var backup []matterv1alpha1.Endpoint
		Expect(json.Unmarshal(get(t.key("main-matter"), &corev1.Secret{}).Data[render.MatterEndpointsKey], &backup)).To(Succeed())
		Expect(backup).To(Equal(b.Status.Endpoints))
	})

	It("only exposes Devices matching the selector", func() {
		t := newTestNS()
		t.createDevice("lamp", map[string]string{"matter": "yes"})
		t.createDevice("router", nil)
		t.createBridge("main", 10, func(b *matterv1alpha1.Bridge) {
			b.Spec.DeviceSelector = metav1.LabelSelector{MatchLabels: map[string]string{"matter": "yes"}}
		})
		b := reconcileBridge(r, t.key("main"))
		Expect(endpoints(b)).To(Equal(map[string]int32{"lamp": 2}))
	})

	It("keeps the setup code and endpoint IDs when the Bridge is re-created", func() {
		t := newTestNS()
		t.createDevice("lamp", nil)
		t.createDevice("plug", nil)
		t.createBridge("main", 10)
		first := reconcileBridge(r, t.key("main"))

		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		r.release(t.key("main"))
		t.createDevice("door", nil) // sorts first, but must not take the others' IDs
		t.createBridge("main", 10)
		second := reconcileBridge(r, t.key("main"))
		Expect(second.Status.ManualPairingCode).To(Equal(first.Status.ManualPairingCode))
		Expect(endpoints(second)).To(Equal(map[string]int32{"lamp": 2, "plug": 3, "door": 4}))
	})

	It("restores a deleted Secret's endpoint backup from status", func() {
		t := newTestNS()
		t.createDevice("lamp", nil)
		t.createBridge("main", 10)
		reconcileBridge(r, t.key("main"))
		Expect(k8sClient.Delete(ctx, get(t.key("main-matter"), &corev1.Secret{}))).To(Succeed())

		b := reconcileBridge(r, t.key("main"))
		Expect(endpoints(b)).To(Equal(map[string]int32{"lamp": 2}))
		var backup []matterv1alpha1.Endpoint
		Expect(json.Unmarshal(get(t.key("main-matter"), &corev1.Secret{}).Data[render.MatterEndpointsKey], &backup)).To(Succeed())
		Expect(backup).To(Equal(b.Status.Endpoints))
	})

	It("stops when status and the Secret's backup disagree", func() {
		t := newTestNS()
		t.createDevice("lamp", nil)
		t.createBridge("main", 10)
		reconcileBridge(r, t.key("main"))
		secret := get(t.key("main-matter"), &corev1.Secret{})
		secret.Data[render.MatterEndpointsKey] = []byte(`[{"device":"plug","endpoint":2}]`)
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		b := reconcileBridge(r, t.key("main"))
		Expect(readyReason(b)).To(Equal("EndpointConflict"))
		Expect(endpoints(b)).To(Equal(map[string]int32{"lamp": 2}))
	})

	It("reports a Secret it can't use", func() {
		t := newTestNS()
		t.createBridge("main", 10)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "main-matter", Namespace: t.ns},
			StringData: map[string]string{render.MatterPasscodeKey: "12345678", render.MatterDiscriminatorKey: "3840"},
		})).To(Succeed())
		b := reconcileBridge(r, t.key("main"))
		Expect(readyReason(b)).To(Equal("InvalidSecret"))
		Expect(meta.FindStatusCondition(b.Status.Conditions, ConditionReady).Message).To(ContainSubstring(render.MatterDACKey))
		Expect(k8sClient.Get(ctx, t.key("main-matter"), &appsv1.Deployment{})).NotTo(Succeed())
	})

	It("allocates distinct ports and reports exhaustion", func() {
		t := newTestNS()
		t.createBridge("one", 2)
		t.createBridge("two", 2)
		t.createBridge("three", 2)
		p1 := reconcileBridge(r, t.key("one")).Status.Port
		p2 := reconcileBridge(r, t.key("two")).Status.Port
		Expect([]int32{p1, p2}).To(ConsistOf(t.start, t.start+1))

		b := reconcileBridge(r, t.key("three"))
		Expect(b.Status.Port).To(BeZero())
		Expect(readyReason(b)).To(Equal("PortRangeExhausted"))
	})
})

var _ = Describe("endpoint allocation", func() {
	ep := func(d string, id int32) matterv1alpha1.Endpoint {
		return matterv1alpha1.Endpoint{Device: d, Endpoint: id}
	}

	It("merges a backup that holds what status lost", func() {
		got, err := mergeEndpoints([]matterv1alpha1.Endpoint{ep("a", 2)}, []matterv1alpha1.Endpoint{ep("a", 2), ep("b", 3)})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]matterv1alpha1.Endpoint{ep("a", 2), ep("b", 3)}))
	})

	It("refuses a backup that disagrees, or leaves a gap", func() {
		_, err := mergeEndpoints([]matterv1alpha1.Endpoint{ep("a", 2)}, []matterv1alpha1.Endpoint{ep("b", 2)})
		Expect(err).To(MatchError(ContainSubstring("endpoint 2 is a and b")))
		_, err = mergeEndpoints([]matterv1alpha1.Endpoint{ep("a", 2)}, []matterv1alpha1.Endpoint{ep("a", 3)})
		Expect(err).To(MatchError(ContainSubstring("device a is endpoint 2 and 3")))
		_, err = mergeEndpoints(nil, []matterv1alpha1.Endpoint{ep("a", 3)})
		Expect(err).To(MatchError(ContainSubstring("endpoint 2 is missing")))
	})

	It("allocates new Devices in name order after the rest", func() {
		got, err := allocate([]matterv1alpha1.Endpoint{ep("m", 2)}, []string{"z", "m", "a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]matterv1alpha1.Endpoint{ep("m", 2), ep("a", 3), ep("z", 4)}))
	})

	It("stops at the last endpoint ID", func() {
		var full []matterv1alpha1.Endpoint
		for id := int32(matterv1alpha1.FirstEndpoint); id <= matterv1alpha1.MaxEndpoint; id++ {
			full = append(full, ep(strings.Repeat("x", 1)+strconv.Itoa(int(id)), id))
		}
		_, err := allocate(full, []string{"new"})
		Expect(err).To(MatchError(ContainSubstring("no endpoint ID left for new")))
	})
})
