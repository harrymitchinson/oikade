package securechannel

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/contain"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// panickingConn panics, once, writing the unsecured Secure Channel message with an opcode.
type panickingConn struct {
	net.PacketConn
	op atomic.Int32 // -1 for none
}

func (c *panickingConn) WriteTo(p []byte, a net.Addr) (int, error) {
	if op := c.op.Load(); op >= 0 {
		if h, n, err := transport.ParseHeader(p); err == nil && h.SessionID == 0 {
			if ph, _, err := transport.ParseProtocolHeader(p[n:]); err == nil && ph.Protocol == transport.SecureChannel &&
				int32(ph.Opcode) == op && c.op.CompareAndSwap(op, -1) {
				panic("injected")
			}
		}
	}
	return c.PacketConn.WriteTo(p, a)
}

// panickingDevice is a device whose transport contains panics, and says where.
type panickingDevice struct {
	t    *transport.Transport
	conn *panickingConn
	mu   sync.Mutex
	at   []string
}

func newPanickingDevice(t *testing.T) *panickingDevice {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &panickingDevice{conn: &panickingConn{PacketConn: pc}}
	d.conn.op.Store(-1)
	d.t = transport.New(d.conn, transport.Options{Log: logging.Test(t), Panicked: func(site string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.at = append(d.at, site)
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.t.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return d
}

func (d *panickingDevice) addr() netip.AddrPort { return d.t.Addr().(*net.UDPAddr).AddrPort() }

// contained waits for the device to have contained one panic, in a handler, and to have only the
// secure sessions it handed to the node: known says how many. After the panic, the peer's
// retransmission may start the handshake again, and a CASE then succeeds.
func (d *panickingDevice) contained(t *testing.T, known func() int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.mu.Lock()
		at := slices.Clone(d.at)
		d.mu.Unlock()
		if len(at) > 0 && d.t.SecureSessions() == known() {
			if !slices.Equal(at, []string{contain.Handler}) {
				t.Errorf("panics contained at %v, want one at %s", at, contain.Handler)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("panics contained at %v, and %d secure sessions; want one panic and %d", at, d.t.SecureSessions(), known())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPASEPanicRemovesTheSession: a panic once PASE has added its session, telling the
// commissioner it succeeded or handing the session to the node, takes the session out again, so
// no secure session is left that the node doesn't know of.
func TestPASEPanicRemovesTheSession(t *testing.T) {
	v, err := mcrypto.NewVerifier(testPasscode, testSalt, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		op         int32
		panicAfter bool // in OnSession
	}{
		{"telling the commissioner", int32(OpStatusReport), false},
		{"handing the session over", -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, cli := newPanickingDevice(t), newNode(t)
			(&Responder{
				Transport: dev.t, Verifier: v, Salt: testSalt, Iterations: 1000, Log: logging.Test(t),
				OnSession: func(*transport.Session) {
					if tc.panicAfter {
						panic("injected")
					}
				},
			}).Register()
			dev.conn.op.Store(tc.op)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = EstablishPASE(ctx, cli.t, dev.addr(), testPasscode, nil)
			dev.contained(t, func() int { return 0 })
		})
	}
}

// TestCASEPanicRemovesTheSession is TestPASEPanicRemovesTheSession for CASE, resumed or not.
func TestCASEPanicRemovesTheSession(t *testing.T) {
	ca := newCA(t, false)
	devFabric := ca.fabric(t, 1, deviceNode, nil, time.Time{})
	ctlFabric := ca.fabric(t, 1, controllerNode, nil, time.Time{})
	for _, tc := range []struct {
		name       string
		resume     bool
		op         int32
		panicAfter bool // in OnSession
	}{
		{"telling the initiator", false, int32(OpStatusReport), false},
		{"handing the session over", false, -1, true},
		{"sending Sigma2_Resume", true, int32(OpSigma2Resume), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, ctl := newPanickingDevice(t), newCASENode(t)
			var panicNow atomic.Bool
			var known atomic.Int32
			sessions := make(chan *transport.Session, 4)
			(&CASEResponder{
				Transport: dev.t, Fabrics: fabrics{devFabric}, Resumption: NewResumptionStore(0), Log: logging.Test(t),
				OnSession: func(s *transport.Session) {
					if panicNow.Load() {
						panic("injected")
					}
					known.Add(1)
					sessions <- s
				},
			}).Register()
			cfg := CASEConfig{Resumption: NewResumptionStore(0)}
			if tc.resume {
				if _, err := EstablishCASE(timeout(t), ctl.t, dev.addr(), ctlFabric, deviceNode, cfg); err != nil {
					t.Fatalf("first CASE: %v", err)
				}
				select {
				case s := <-sessions:
					dev.t.RemoveSession(s)
					known.Add(-1)
				case <-time.After(3 * time.Second):
					t.Fatal("the device reported no session")
				}
			}
			panicNow.Store(tc.panicAfter)
			dev.conn.op.Store(tc.op)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = EstablishCASE(ctx, ctl.t, dev.addr(), ctlFabric, deviceNode, cfg)
			dev.contained(t, func() int { return int(known.Load()) })
		})
	}
}
