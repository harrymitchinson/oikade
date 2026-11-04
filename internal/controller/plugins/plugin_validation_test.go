package plugins

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
)

var _ = Describe("Plugin discovery validation", func() {
	create := func(discovery *homev1alpha1.DiscoverySpec) (*pluginsv1alpha1.Plugin, error) {
		p := &pluginsv1alpha1.Plugin{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "discovery-", Namespace: "default"},
			Spec:       pluginsv1alpha1.PluginSpec{Image: "example.com/oikade-plugin-hub:test", Discovery: discovery},
		}
		err := k8sClient.Create(ctx, p)
		if err == nil {
			DeferCleanup(k8sClient.Delete, ctx, p)
		}
		return p, err
	}

	It("leaves a Plugin without discovery as it was", func() {
		p, err := create(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Spec.Discovery).To(BeNil())
	})

	It("defaults matter to Expose and removalPolicy to Retain", func() {
		p, err := create(&homev1alpha1.DiscoverySpec{Adopt: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(*p.Spec.Discovery).To(Equal(homev1alpha1.DiscoverySpec{
			Adopt: true, Matter: homev1alpha1.DiscoveryMatterExpose, RemovalPolicy: homev1alpha1.RemovalPolicyRetain,
		}))
	})

	It("accepts a grace period with removalPolicy Delete", func() {
		_, err := create(&homev1alpha1.DiscoverySpec{
			Adopt: true, Matter: homev1alpha1.DiscoveryMatterHide, RemovalPolicy: homev1alpha1.RemovalPolicyDelete,
			RemovalGracePeriod: &metav1.Duration{Duration: 2 * time.Hour},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects a grace period without removalPolicy Delete", func() {
		_, err := create(&homev1alpha1.DiscoverySpec{
			Adopt: true, RemovalGracePeriod: &metav1.Duration{Duration: time.Hour},
		})
		Expect(err).To(MatchError(ContainSubstring("removalGracePeriod applies only to removalPolicy Delete")))
	})

	It("rejects a grace period that isn't positive", func() {
		_, err := create(&homev1alpha1.DiscoverySpec{
			Adopt: true, RemovalPolicy: homev1alpha1.RemovalPolicyDelete, RemovalGracePeriod: &metav1.Duration{},
		})
		Expect(err).To(MatchError(ContainSubstring("removalGracePeriod must be positive")))
	})

	It("rejects an unknown matter or removalPolicy value", func() {
		_, err := create(&homev1alpha1.DiscoverySpec{Matter: "Sometimes"})
		Expect(err).To(MatchError(ContainSubstring("spec.discovery.matter")))
		_, err = create(&homev1alpha1.DiscoverySpec{RemovalPolicy: "Archive"})
		Expect(err).To(MatchError(ContainSubstring("spec.discovery.removalPolicy")))
	})
})
