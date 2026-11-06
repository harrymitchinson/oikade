package node

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

func addr(n *Node) netip.AddrPort { return nodetest.Addr(n.cfg.Port) }

func (f *fakeMDNS) operational() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, svc := range f.services {
		if svc.Type == mdns.OperationalType {
			names = append(names, svc.Instance)
		}
	}
	return names
}

func (n *Node) secureSessions(mode transport.AuthMode) int {
	count := 0
	for _, s := range n.sessions.list() {
		if s.AuthMode() == mode {
			count++
		}
	}
	return count
}

// TestCommissionAndRestart commissions a node in process, restarts it on the same data, reaches
// it again over CASE, and removes the fabric, which opens the initial window again.
func TestCommissionAndRestart(t *testing.T) {
	dir := t.TempDir()
	n, fake, stop := start(t, Config{DataDir: dir})
	waitFor(t, "the initial window", func() bool { return fake.commissionable() != nil })
	cm := nodetest.New(t)
	admin := cm.Commission(addr(n), testSetup.Passcode)

	waitFor(t, "the window to close and the fabric to be advertised", func() bool {
		return fake.commissionable() == nil && !n.WindowOpen() && len(fake.operational()) == 1
	})
	if name := fake.operational()[0]; !strings.HasSuffix(name, "-0000000000000A0B") {
		t.Errorf("operational instance %q", name)
	}
	waitFor(t, "EndPASE to close the PASE session", func() bool { return n.secureSessions(transport.AuthPASE) == 0 })

	// A peer's CloseSession ends its session.
	ex, err := admin.Session.NewExchange()
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Send(nodetest.Context(t), securechannel.StatusReport{
		General: securechannel.GeneralSuccess, Protocol: transport.SecureChannel, Code: securechannel.CodeCloseSession,
	}.Message()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the node to close the CASE session", func() bool { return n.secureSessions(transport.AuthCASE) == 0 })

	// After a restart the node is still on the fabric: it advertises it, doesn't open a window,
	// and answers CASE.
	port := n.cfg.Port
	stop()
	n, fake, _ = start(t, Config{DataDir: dir, Port: port})
	waitFor(t, "the fabric to be advertised after the restart", func() bool { return len(fake.operational()) == 1 })
	if fake.commissionable() != nil || n.WindowOpen() {
		t.Error("a commissioned node opened its window after a restart")
	}
	admin = cm.CASE(addr(n))
	reports, err := admin.Read(nodetest.Context(t), im.AttributePath{Endpoint: 0, Cluster: clusters.IDOperationalCredentials, Attribute: 0x0005})
	if err != nil || len(reports) != 1 {
		t.Fatalf("read CurrentFabricIndex: %+v, %v", reports, err)
	}

	// Removing the last fabric withdraws it and opens the initial window again.
	f := cm.Invoke(admin, clusters.IDOperationalCredentials, 0x0A, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 1) })
	if f[0] != uint64(0) {
		t.Fatalf("RemoveFabric: %v", f)
	}
	waitFor(t, "the window to reopen", func() bool {
		return len(fake.operational()) == 0 && fake.commissionable() != nil && n.WindowOpen()
	})
}
