package matter

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
)

var _ = Describe("Bridge validation", func() {
	var bridge *matterv1alpha1.Bridge
	BeforeEach(func() {
		bridge = &matterv1alpha1.Bridge{ObjectMeta: metav1.ObjectMeta{GenerateName: "matter-", Namespace: "default"}}
		Expect(k8sClient.Create(ctx, bridge)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, bridge)
	})
	setEndpoints := func(next int32, eps ...matterv1alpha1.Endpoint) error {
		bridge.Status.NextEndpoint = next
		bridge.Status.Endpoints = eps
		return k8sClient.Status().Update(ctx, bridge)
	}
	ep := func(device string, id int32) matterv1alpha1.Endpoint {
		return matterv1alpha1.Endpoint{Device: device, Endpoint: id}
	}

	It("defaults the network, port range and storage", func() {
		Expect(bridge.Spec.Network.Mode).To(Equal(matterv1alpha1.NetworkModeHostNetwork))
		Expect(bridge.Spec.PortRange).To(Equal(matterv1alpha1.PortRange{Start: 5540, End: 5640}))
		Expect(bridge.Spec.Storage.Size.String()).To(Equal("100Mi"))
	})

	It("accepts status with commissioning details, fabrics and endpoints", func() {
		disc := int32(3840)
		bridge.Status.Port = 5540
		bridge.Status.Discriminator = &disc
		bridge.Status.ManualPairingCode = "34970112332"
		bridge.Status.QRCode = "MT:Y.K9042C00KA0648G00"
		bridge.Status.Fabrics = []matterv1alpha1.Fabric{
			{FabricIndex: 1, FabricID: "0000000000000001", NodeID: "00000000DEADBEEF", VendorID: 0x1349, Label: "Home"},
		}
		Expect(setEndpoints(4, ep("desk-lamp", 2), ep("hall-sensor", 3))).To(Succeed())
	})

	It("rejects a duplicate endpoint", func() {
		Expect(setEndpoints(4, ep("desk-lamp", 2), ep("hall-sensor", 2))).
			To(MatchError(ContainSubstring("Duplicate value")))
	})

	It("rejects endpoints that aren't allocated densely", func() {
		Expect(setEndpoints(5, ep("desk-lamp", 2), ep("hall-sensor", 4))).
			To(MatchError(ContainSubstring("allocated densely")))
	})

	It("rejects an endpoint at or above nextEndpoint, or below the first", func() {
		Expect(setEndpoints(3, ep("desk-lamp", 3))).
			To(MatchError(ContainSubstring("every endpoint must be below nextEndpoint")))
		Expect(setEndpoints(3, ep("desk-lamp", 1))).
			To(MatchError(ContainSubstring("status.endpoints[0].endpoint")))
	})

	It("keeps endpoints append-only", func() {
		Expect(setEndpoints(4, ep("desk-lamp", 2), ep("hall-sensor", 3))).To(Succeed())
		Expect(setEndpoints(5, ep("desk-lamp", 2), ep("hall-sensor", 3), ep("porch-plug", 4))).To(Succeed())

		reset := func() { Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(bridge), bridge)).To(Succeed()) }
		Expect(setEndpoints(5, ep("desk-lamp", 2), ep("porch-plug", 4))).
			To(MatchError(ContainSubstring("allocated densely")))
		reset()
		Expect(setEndpoints(4, ep("desk-lamp", 2), ep("porch-plug", 3))).
			To(MatchError(ContainSubstring("nextEndpoint never decreases")))
		reset()
		Expect(setEndpoints(5, ep("desk-lamp", 4), ep("hall-sensor", 3), ep("porch-plug", 2))).
			To(MatchError(ContainSubstring("an endpoint's Device never changes")))
		reset()
		// A removed Device's ID handed to a new one.
		Expect(setEndpoints(5, ep("new-lamp", 2), ep("hall-sensor", 3), ep("porch-plug", 4))).
			To(MatchError(ContainSubstring("an endpoint's Device never changes")))
		reset()
		Expect(setEndpoints(6, ep("desk-lamp", 2), ep("hall-sensor", 3), ep("porch-plug", 4), ep("desk-lamp-2", 5))).To(Succeed())
	})

	It("rejects a displayName over 32 bytes, and cuts the name fallback to 32 bytes", func() {
		bridge.Spec.DisplayName = "Wohnzimmer Brücke Erdgeschoß 🏠" // 30 characters, 35 bytes
		Expect(k8sClient.Update(ctx, bridge)).To(MatchError(ContainSubstring("at most 32 bytes")))

		b := matterv1alpha1.Bridge{ObjectMeta: metav1.ObjectMeta{Name: "living-room-matter-bridge-upstairs"}}
		Expect(b.Title()).To(Equal("living-room-matter-bridge-upstai"))
		b.Spec.DisplayName = "Brücke"
		Expect(b.Title()).To(Equal("Brücke"))
	})

	It("rejects a malformed fabric", func() {
		bridge.Status.Fabrics = []matterv1alpha1.Fabric{{FabricIndex: 0, FabricID: "1", NodeID: "00000000DEADBEEF"}}
		err := k8sClient.Status().Update(ctx, bridge)
		Expect(err).To(MatchError(ContainSubstring("status.fabrics[0].fabricIndex")))
		Expect(err).To(MatchError(ContainSubstring("status.fabrics[0].fabricID")))
	})
})

var _ = Describe("Bridge samples", func() {
	It("accepts the Bridge in the samples", func() {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "samples", "matter_v1alpha1_bridge.yaml"))
		Expect(err).NotTo(HaveOccurred())
		var bridge matterv1alpha1.Bridge
		Expect(yaml.Unmarshal(b, &bridge)).To(Succeed())
		bridge.Namespace = "default"
		bridge.Name = "sample-" + bridge.Name
		Expect(k8sClient.Create(ctx, &bridge)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, &bridge)
	})
})
