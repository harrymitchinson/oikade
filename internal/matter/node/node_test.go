package node

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/setup"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// fakeMDNS records the services the node advertises.
type fakeMDNS struct {
	mu       sync.Mutex
	services map[string]mdns.Service
	// closing, if set, runs as the responder closes, which is when it says goodbye.
	closing func()
	// resolve, if set, answers ResolveOperational. Unset, it finds nothing.
	resolve func(compressedFabricID, nodeID uint64) (mdns.Resolved, error)
	// lost is the node's Options.Lost, which a test calls as a responder would.
	lost func(mdns.Service)
}

// loseName stops advertising svc and tells the node, as a responder does when another host takes
// its name.
func (f *fakeMDNS) loseName(svc mdns.Service) {
	f.mu.Lock()
	delete(f.services, svc.Instance+"."+svc.Type)
	lost := f.lost
	f.mu.Unlock()
	lost(svc)
}

// commissionableService returns the advertised commissionable service, if there's one.
func (f *fakeMDNS) commissionableService() (mdns.Service, bool) {
	return f.service(mdns.CommissionableType)
}

// operationalService returns an advertised operational service, if there's one.
func (f *fakeMDNS) operationalService() (mdns.Service, bool) { return f.service(mdns.OperationalType) }

func (f *fakeMDNS) service(typ string) (mdns.Service, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, svc := range f.services {
		if svc.Type == typ {
			return svc, true
		}
	}
	return mdns.Service{}, false
}

func (f *fakeMDNS) Add(_ context.Context, svc mdns.Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[svc.Instance+"."+svc.Type] = svc
	return nil
}

func (f *fakeMDNS) Remove(svc mdns.Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.services, svc.Instance+"."+svc.Type)
	return nil
}

// ResolveOperational answers with resolve, or else finds nothing, so the node reaches controllers at
// their last address.
func (f *fakeMDNS) ResolveOperational(_ context.Context, compressedFabricID, nodeID uint64) (mdns.Resolved, error) {
	f.mu.Lock()
	resolve := f.resolve
	f.mu.Unlock()
	if resolve == nil {
		return mdns.Resolved{}, mdns.ErrNotFound
	}
	return resolve(compressedFabricID, nodeID)
}

func (f *fakeMDNS) Close() error {
	f.mu.Lock()
	closing := f.closing
	f.mu.Unlock()
	if closing != nil {
		closing()
	}
	return nil
}

// commissionable returns the advertised commissionable service's TXT strings, or nil.
func (f *fakeMDNS) commissionable() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, svc := range f.services {
		if svc.Type == mdns.CommissionableType {
			return svc.Text
		}
	}
	return nil
}

// held are the sockets freePort keeps open, by port, until their test ends.
var held sync.Map

// freePort binds a UDP port and keeps it until the test ends, so no other test can be handed it,
// even between a node's runs. A node's listenUDP is listenHeld, which takes the socket over.
func freePort(t *testing.T) uint16 {
	t.Helper()
	pc, err := net.ListenPacket("udp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	held.Store(port, pc)
	t.Cleanup(func() {
		held.Delete(port)
		_ = pc.Close()
	})
	return port
}

// listenHeld hands a node the socket freePort keeps for addr's port, or listens as a node does.
func listenHeld(addr string) (net.PacketConn, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, err
	}
	pc, ok := held.Load(ap.Port())
	if !ok {
		return listenUDP(addr)
	}
	c := heldConn{pc.(net.PacketConn)}
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return c, nil
}

// heldConn is a held socket that a node's Close leaves open: it ends the node's reads, and the
// port stays the test's for the node's next run.
type heldConn struct{ net.PacketConn }

func (c heldConn) Close() error { return c.SetReadDeadline(time.Now()) }

// testSetup is the setup code of the nodes the tests start.
var testSetup = Setup{Passcode: 20202021, Discriminator: 3840}

// start runs a node in dir until the test ends, or until it calls the returned stop.
func start(t *testing.T, cfg Config) (*Node, *fakeMDNS, func()) {
	t.Helper()
	fake := &fakeMDNS{services: map[string]mdns.Service{}}
	n, stop := startWith(t, cfg, fake)
	return n, fake, stop
}

// startWith is start, with fake as the node's mDNS responder from the moment it runs.
func startWith(t *testing.T, cfg Config, fake *fakeMDNS) (*Node, func()) {
	t.Helper()
	if cfg.Port == 0 {
		cfg.Port = freePort(t)
	}
	if cfg.Log == nil {
		cfg.Log = logging.Test(t)
	}
	cfg.Setup, cfg.Info = testSetup, clusters.Info{VendorName: "Oikade"}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	n.listenMDNS = func(o mdns.Options) (advertiser, error) {
		fake.mu.Lock()
		fake.lost = o.Lost
		fake.mu.Unlock()
		return fake, nil
	}
	n.listenUDP = listenHeld
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil && ctx.Err() == nil {
			t.Error(err)
		}
	}
	t.Cleanup(stop)
	return n, stop
}

func client(t *testing.T) *transport.Transport {
	t.Helper()
	tr, err := transport.Listen("[::1]:0", transport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = tr.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return tr
}

func pase(t *testing.T, cli *transport.Transport, n *Node, passcode uint32) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := securechannel.EstablishPASE(ctx, cli, netip.AddrPortFrom(netip.IPv6Loopback(), n.cfg.Port), passcode, nil)
	return err
}

func TestNeedsSetup(t *testing.T) {
	if _, err := New(Config{DataDir: t.TempDir(), Setup: Setup{Passcode: 12345678}}); err == nil {
		t.Error("New took a trivial passcode")
	}
}

// TestWindow opens the initial window on a node on no fabric, answers PASE with the node's
// passcode while it's open, ends PASE sessions on EndPASE, and refuses PASE once the window times
// out.
func TestWindow(t *testing.T) {
	n, fake, _ := start(t, Config{DataDir: t.TempDir(), WindowTimeout: time.Second})
	testkit.Eventually(t, "the window to open", func() bool { return fake.commissionable() != nil })
	if txt := fake.commissionable(); !slices.Contains(txt, "CM=1") || !slices.ContainsFunc(txt, func(s string) bool {
		return s == "D="+strconv.Itoa(int(testSetup.Discriminator))
	}) {
		t.Errorf("commissionable TXT %v", txt)
	}
	cli := client(t)
	if err := pase(t, cli, n, testSetup.Passcode); err != nil {
		t.Fatalf("PASE in the initial window: %v", err)
	}
	paseSessions := func() int {
		count := 0
		for _, s := range n.sessions.list() {
			if s.AuthMode() == transport.AuthPASE {
				count++
			}
		}
		return count
	}
	testkit.Eventually(t, "the node to record the PASE session", func() bool { return paseSessions() == 1 })
	n.endPASE()
	testkit.Eventually(t, "EndPASE to end the PASE session", func() bool { return paseSessions() == 0 })

	testkit.Eventually(t, "the window to time out", func() bool { return fake.commissionable() == nil && !n.WindowOpen() })
	if err := pase(t, cli, n, testSetup.Passcode); err == nil {
		t.Error("PASE succeeded with the window closed")
	}

	// An administrator opens an enhanced window with its own passcode.
	const passcode = 34567890
	salt := []byte("an enhanced window's salt")
	v, err := mcrypto.NewVerifier(passcode, salt, 2000)
	if err != nil {
		t.Fatal(err)
	}
	n.openWindow(clusters.Window{Verifier: v, Salt: salt, Iterations: 2000, Discriminator: 0x123, Timeout: 3 * time.Minute})
	if txt := fake.commissionable(); !slices.Contains(txt, "CM=2") || !slices.Contains(txt, "D=291") {
		t.Errorf("enhanced window TXT %v", txt)
	}
	if err := pase(t, cli, n, passcode); err != nil {
		t.Errorf("PASE in the enhanced window: %v", err)
	}
}

func TestPayload(t *testing.T) {
	n, err := New(Config{DataDir: t.TempDir(), Setup: testSetup, Log: logging.Test(t)})
	if err != nil {
		t.Fatal(err)
	}
	p := n.Payload()
	qr, err := p.QRCode()
	if err != nil || !strings.HasPrefix(qr, "MT:") {
		t.Errorf("QR %q, %v", qr, err)
	}
	if p.Discovery != setup.DiscoveryOnNetwork || p.Passcode != testSetup.Passcode || p.Discriminator != testSetup.Discriminator {
		t.Errorf("payload %+v", p)
	}
}
