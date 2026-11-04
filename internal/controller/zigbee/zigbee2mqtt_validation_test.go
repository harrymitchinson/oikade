package zigbee

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
)

var _ = Describe("Zigbee2MQTT discovery validation", func() {
	create := func(discovery *homev1alpha1.DiscoverySpec) (*zigbeev1alpha1.Zigbee2MQTT, error) {
		z := &zigbeev1alpha1.Zigbee2MQTT{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "discovery-", Namespace: "default"},
			Spec: zigbeev1alpha1.Zigbee2MQTTSpec{
				Serial:    zigbeev1alpha1.SerialSpec{Port: "tcp://slzb-06:6638", Adapter: "zstack"},
				Discovery: discovery,
			},
		}
		err := k8sClient.Create(ctx, z)
		if err == nil {
			DeferCleanup(k8sClient.Delete, ctx, z)
		}
		return z, err
	}

	It("leaves an instance without discovery as it was", func() {
		z, err := create(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(z.Spec.Discovery).To(BeNil())
	})

	It("defaults matter to Expose and removalPolicy to Retain", func() {
		z, err := create(&homev1alpha1.DiscoverySpec{Adopt: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(z.Spec.Discovery.Matter).To(Equal(homev1alpha1.DiscoveryMatterExpose))
		Expect(z.Spec.Discovery.RemovalPolicy).To(Equal(homev1alpha1.RemovalPolicyRetain))
	})

	It("rejects a grace period without removalPolicy Delete", func() {
		_, err := create(&homev1alpha1.DiscoverySpec{
			Adopt: true, RemovalGracePeriod: &metav1.Duration{Duration: time.Hour},
		})
		Expect(err).To(MatchError(ContainSubstring("removalGracePeriod applies only to removalPolicy Delete")))
	})
})
