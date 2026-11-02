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

	"github.com/go-logr/logr/testr"

	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/setup"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// fakeMDNS records the services the node advertises.
type fakeMDNS struct {
	mu       sync.Mutex
	services map[string]mdns.Service
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

func (f *fakeMDNS) Close() error { return nil }

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

func freePort(t *testing.T) uint16 {
	t.Helper()
	pc, err := net.ListenPacket("udp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
}

// start runs a node in dir until the test ends.
func start(t *testing.T, dir string) (*Node, *fakeMDNS) {
	t.Helper()
	n, err := New(Config{DataDir: dir, Port: freePort(t), Log: testr.New(t), Info: clusters.Info{VendorName: "Oikade"}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMDNS{services: map[string]mdns.Service{}}
	n.listenMDNS = func(mdns.Options) (advertiser, error) { return fake, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx, false) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && ctx.Err() == nil {
			t.Error(err)
		}
	})
	waitFor(t, "the window to open", func() bool { return fake.commissionable() != nil })
	return n, fake
}

func client(t *testing.T) *transport.Transport {
	t.Helper()
	tr, err := transport.Listen("[::1]:0", transport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tr.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return tr
}

func pase(t *testing.T, cli *transport.Transport, n *Node, passcode uint32) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := securechannel.EstablishPASE(ctx, cli, netip.AddrPortFrom(netip.IPv6Loopback(), n.cfg.Port), passcode, nil)
	return err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSetupPersists keeps the setup code across restarts, so the pairing code doesn't change.
func TestSetupPersists(t *testing.T) {
	dir := t.TempDir()
	a, err := loadSetup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !setup.ValidPasscode(a.Passcode) || a.Discriminator > 0xFFF {
		t.Errorf("invalid setup %+v", a)
	}
	b, err := loadSetup(dir)
	if err != nil || b != a {
		t.Errorf("reloaded %+v, %v; want %+v", b, err, a)
	}
}

// TestWindow opens the initial window on a node on no fabric, answers PASE with the node's
// passcode while it's open, refuses PASE once it's closed, and ends PASE sessions on EndPASE.
func TestWindow(t *testing.T) {
	n, fake := start(t, t.TempDir())
	if txt := fake.commissionable(); !slices.Contains(txt, "CM=1") || !slices.ContainsFunc(txt, func(s string) bool {
		return s == "D="+strconv.Itoa(int(n.setup.Discriminator))
	}) {
		t.Errorf("commissionable TXT %v", txt)
	}
	cli := client(t)
	if err := pase(t, cli, n, n.setup.Passcode); err != nil {
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
	waitFor(t, "the node to record the PASE session", func() bool { return paseSessions() == 1 })
	n.endPASE()
	waitFor(t, "EndPASE to end the PASE session", func() bool { return paseSessions() == 0 })

	n.closeWindow()
	if txt := fake.commissionable(); txt != nil {
		t.Errorf("still advertising %v after the window closed", txt)
	}
	if err := pase(t, cli, n, n.setup.Passcode); err == nil {
		t.Error("PASE succeeded with the window closed")
	}

	// An administrator opens an enhanced window with its own passcode.
	const passcode = 34567890
	salt := []byte("an enhanced window's salt")
	v, err := mcrypto.NewVerifier(passcode, salt, 2000)
	if err != nil {
		t.Fatal(err)
	}
	n.openWindowHook(clusters.Window{Verifier: v, Salt: salt, Iterations: 2000, Discriminator: 0x123, Timeout: 3 * time.Minute})
	if txt := fake.commissionable(); !slices.Contains(txt, "CM=2") || !slices.Contains(txt, "D=291") {
		t.Errorf("enhanced window TXT %v", txt)
	}
	if err := pase(t, cli, n, passcode); err != nil {
		t.Errorf("PASE in the enhanced window: %v", err)
	}
}

func TestPayload(t *testing.T) {
	n, err := New(Config{DataDir: t.TempDir(), Log: testr.New(t)})
	if err != nil {
		t.Fatal(err)
	}
	p := n.Payload()
	qr, err := p.QRCode()
	if err != nil || !strings.HasPrefix(qr, "MT:") {
		t.Errorf("QR %q, %v", qr, err)
	}
	if p.Discovery != setup.DiscoveryOnNetwork || p.Passcode != n.setup.Passcode {
		t.Errorf("payload %+v", p)
	}
}
