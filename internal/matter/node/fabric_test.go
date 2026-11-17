package node

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// TestARemovedFabricsSessionsEndAfterTheGrace: removing a fabric ends its sessions
// fabricSessionGrace later on the node's clock, not at once, so the RemoveFabric response gets out
// on the session first.
func TestARemovedFabricsSessionsEndAfterTheGrace(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	n, fake, _ := start(t, Config{DataDir: t.TempDir(), Clock: clk})
	testkit.Eventually(t, "the initial window", func() bool { return fake.commissionable() != nil })
	cm := nodetest.New(t)
	admin := cm.Commission(addr(n), testSetup.Passcode)
	testkit.Eventually(t, "the CASE session alone", func() bool {
		return n.secureSessions(transport.AuthCASE) == 1 && n.secureSessions(transport.AuthPASE) == 0
	})

	removed := clk.Now()
	if f := cm.Invoke(admin, clusters.IDOperationalCredentials, 0x0A, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 1) }); len(f) == 0 || f[0] != uint64(0) {
		t.Fatalf("RemoveFabric: %v", f)
	}
	testkit.Eventually(t, "the node to wait 2 s before ending the fabric's sessions", func() bool {
		at, ok := clk.NextWait()
		return ok && at.Equal(removed.Add(2*time.Second))
	})
	clk.SetTime(removed.Add(2*time.Second - time.Nanosecond))
	if n.secureSessions(transport.AuthCASE) != 1 {
		t.Fatal("the fabric's session ended before the grace")
	}
	clk.Step(time.Nanosecond)
	testkit.Eventually(t, "the fabric's session to end after the grace", func() bool { return n.secureSessions(transport.AuthCASE) == 0 })
}
