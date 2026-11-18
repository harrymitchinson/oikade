package node

import (
	"testing"

	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

func TestALostCommissionableNameIsAdvertisedUnderANewOne(t *testing.T) {
	_, fake, _ := start(t, Config{DataDir: t.TempDir()})
	testkit.Eventually(t, "the initial window", func() bool { _, ok := fake.commissionableService(); return ok })
	first, _ := fake.commissionableService()

	fake.loseName(first)

	testkit.Eventually(t, "a new commissionable name", func() bool {
		svc, ok := fake.commissionableService()
		return ok && svc.Instance != first.Instance
	})
	again, _ := fake.commissionableService()
	if !equalText(again.Text, first.Text) {
		t.Errorf("advertised %q, want the same window as %q", again.Text, first.Text)
	}
}

func TestALostOperationalNameIsntReplaced(t *testing.T) {
	n, fake, _ := start(t, Config{DataDir: t.TempDir()})
	testkit.Eventually(t, "the node to run", func() bool { _, ok := fake.commissionableService(); return ok })
	svc := mdns.Operational{CompressedFabricID: 1, NodeID: 2}.Service()
	n.mu.Lock()
	n.operational[1] = &svc
	n.mu.Unlock()

	fake.loseName(svc)

	n.mu.Lock()
	_, still := n.operational[1]
	n.mu.Unlock()
	if still {
		t.Error("still counts the lost operational service as advertised")
	}
}

// equalText compares TXT strings, which a new window over the same verifier repeats.
func equalText(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
