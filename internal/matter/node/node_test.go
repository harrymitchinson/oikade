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
)

// fakeMDNS records the services the node advertises.
type fakeMDNS struct {
	mu       sync.Mutex
	services map[string]mdns.Service
	// closing, if set, runs as the responder closes, which is when it says goodbye.
	closing func()
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

func freePort(t *testing.T) uint16 {
	t.Helper()
	pc, err := net.ListenPacket("udp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
}

// testSetup is the setup code of the nodes the tests start.
var testSetup = Setup{Passcode: 20202021, Discriminator: 3840}

// start runs a node in dir until the test ends, or until it calls the returned stop.
func start(t *testing.T, cfg Config) (*Node, *fakeMDNS, func()) {
	t.Helper()
	if cfg.Port == 0 {
		cfg.Port = freePort(t)
	}
	cfg.Setup, cfg.Log, cfg.Info = testSetup, logging.Test(t), clusters.Info{VendorName: "Oikade"}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMDNS{services: map[string]mdns.Service{}}
	n.listenMDNS = func(mdns.Options) (advertiser, error) { return fake, nil }
	ctx, cancel := context.WithCancel(context.Background())
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
	return n, fake, stop
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
	waitFor(t, "the window to open", func() bool { return fake.commissionable() != nil })
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
	waitFor(t, "the node to record the PASE session", func() bool { return paseSessions() == 1 })
	n.endPASE()
	waitFor(t, "EndPASE to end the PASE session", func() bool { return paseSessions() == 0 })

	waitFor(t, "the window to time out", func() bool { return fake.commissionable() == nil && !n.WindowOpen() })
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
