package node

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/harrymitchinson/oikade/internal/logging"
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

// TestRunAgain is a node that stopped serving, as when its socket went, and came back. It has a new
// transport and responder, so what it offered must be offered again.
func TestRunAgain(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), Port: freePort(t), Setup: testSetup, Log: logging.Test(t), Info: clusters.Info{VendorName: "Oikade"}}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMDNS{services: map[string]mdns.Service{}}
	n.listenMDNS = func(mdns.Options) (advertiser, error) { return fake, nil }
	run := func() func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- n.Run(ctx) }()
		return func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			clear(fake.services) // the next responder knows nothing of this one's
		}
	}
	stop := run()
	waitFor(t, "the initial window", func() bool { return fake.commissionable() != nil })
	stop()

	stop = run()
	waitFor(t, "the window to be offered again", func() bool { return fake.commissionable() != nil })
	cm := nodetest.New(t)
	cm.Commission(addr(n), testSetup.Passcode)
	waitFor(t, "the fabric", func() bool { return fake.commissionable() == nil && len(fake.operational()) == 1 })
	stop()

	stop = run()
	defer stop()
	waitFor(t, "the fabric to be advertised again", func() bool { return len(fake.operational()) == 1 })
	if fake.commissionable() != nil || n.WindowOpen() {
		t.Error("a commissioned node opened its window")
	}
	cm.CASE(addr(n))
}

// heard is an im.Observer that keeps what it hears.
type heard struct {
	mu       sync.Mutex
	wrote    []string
	invoked  []string
	reported int
}

func (h *heard) Wrote(cluster uint32, st im.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wrote = append(h.wrote, clusters.Name(cluster)+" "+st.Name())
}

func (h *heard) Invoked(cluster uint32, st im.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.invoked = append(h.invoked, clusters.Name(cluster)+" "+st.Name())
}

func (h *heard) Reported() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reported++
}

func (h *heard) reports() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reported
}

func TestTheObserverHearsWhatAControllerDoes(t *testing.T) {
	h := &heard{}
	n, fake, _ := start(t, Config{DataDir: t.TempDir(), Observer: h})
	waitFor(t, "the initial window", func() bool { return fake.commissionable() != nil })
	admin := nodetest.New(t).Commission(addr(n), testSetup.Passcode)
	waitFor(t, "the PASE session to end", func() bool { return n.secureSessions(transport.AuthPASE) == 0 })
	if n.Sessions() != 1 {
		t.Errorf("%d CASE sessions, want 1", n.Sessions())
	}
	h.mu.Lock()
	if !slices.Contains(h.invoked, "general_commissioning success") {
		t.Errorf("commissioning's invokes: %v", h.invoked)
	}
	h.mu.Unlock()

	label := im.AttributePath{Endpoint: 0, Cluster: clusters.IDBasicInformation, Attribute: 0x05}
	_, _, _, _, err := admin.Subscribe(nodetest.Context(t), 0, 60, label)
	if err != nil {
		t.Fatal(err)
	}
	if n.Subscriptions() != 1 {
		t.Errorf("%d subscriptions, want 1", n.Subscriptions())
	}
	st, err := admin.Write(nodetest.Context(t), label, func(w *tlv.Writer, tag tlv.Tag) { w.PutUTF8(tag, "hall") })
	if err != nil || st != im.StatusSuccess {
		t.Fatalf("writing NodeLabel: %v, %v", st, err)
	}
	waitFor(t, "the change to be reported", func() bool { return h.reports() > 0 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Equal(h.wrote, []string{"basic_information success"}) {
		t.Errorf("writes: %v", h.wrote)
	}
}
