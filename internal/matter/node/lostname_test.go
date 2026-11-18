package node

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
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

func TestALostOperationalNameIsAdvertisedAgainLater(t *testing.T) {
	n, fake, _ := start(t, Config{DataDir: t.TempDir()})
	n.lostRetry = 20 * time.Millisecond
	testkit.Eventually(t, "the initial window", func() bool { _, ok := fake.commissionableService(); return ok })
	nodetest.New(t).Commission(addr(n), testSetup.Passcode)
	var op mdns.Service
	testkit.Eventually(t, "the operational service", func() bool {
		svc, ok := fake.operationalService()
		op = svc
		return ok
	})

	fake.loseName(op) // which stops advertising it

	testkit.Eventually(t, "the operational service again", func() bool {
		svc, ok := fake.operationalService()
		return ok && svc.Instance == op.Instance
	})
}

func TestAWindowThatClosedIsntReopened(t *testing.T) {
	n, fake, _ := start(t, Config{DataDir: t.TempDir()})
	testkit.Eventually(t, "the initial window", func() bool { _, ok := fake.commissionableService(); return ok })
	first, _ := fake.commissionableService()
	// The window closes just as the name is lost: the node mustn't open it again.
	n.mu.Lock()
	n.window = nil
	n.mu.Unlock()
	fake.loseName(first)
	time.Sleep(50 * time.Millisecond) // long enough for a reopen to have advertised
	if svc, ok := fake.commissionableService(); ok {
		t.Errorf("advertised %s for a window that closed", svc.Instance)
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
