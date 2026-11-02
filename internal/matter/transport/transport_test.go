package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

var echo = ProtocolID{Vendor: 0xFFF1, ID: 0x0042}

// echoOps are the opcodes that start an echo exchange.
var echoOps = []uint8{0, 1}

// fastMRP keeps the tests quick: retransmissions after about 55 ms, not 330, and still well
// after a standalone ack.
var fastMRP = MRPConfig{Idle: 50 * time.Millisecond, Active: 50 * time.Millisecond, StandaloneAckTimeout: 10 * time.Millisecond}

// faultyConn is a PacketConn that can drop or repeat what it sends, and records it.
type faultyConn struct {
	net.PacketConn
	mu     sync.Mutex
	sent   [][]byte
	fault  func(n int, pkt []byte) (sends int) // how many times to really send the nth packet
	writes atomic.Int32
}

func (c *faultyConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	n := len(c.sent)
	c.sent = append(c.sent, append([]byte(nil), p...))
	fault := c.fault
	c.mu.Unlock()
	c.writes.Add(1)
	times := 1
	if fault != nil {
		times = fault(n, p)
	}
	for range times {
		if _, err := c.PacketConn.WriteTo(p, addr); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (c *faultyConn) packets() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.sent...)
}

// logBuffer collects log lines.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		fmt.Fprintln(&l.b, prefix, args)
	}, funcr.Options{Verbosity: 1})
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type node struct {
	t    *Transport
	conn *faultyConn
	log  *logBuffer
}

func newNode(t *testing.T) *node {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &node{conn: &faultyConn{PacketConn: pc}, log: &logBuffer{}}
	n.t = New(n.conn, Options{Log: n.log.logger(), MRP: fastMRP})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.t.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return n
}

func (n *node) addr() netip.AddrPort { return n.t.Addr().(*net.UDPAddr).AddrPort() }

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// echoHandler answers each exchange's first message with its payload reversed, and counts the
// exchanges it served.
func echoHandler(calls *atomic.Int32) Handler {
	return func(ctx context.Context, ex *Exchange, m Message) {
		calls.Add(1)
		reply := bytes.Clone(m.Payload)
		for i, j := 0, len(reply)-1; i < j; i, j = i+1, j-1 {
			reply[i], reply[j] = reply[j], reply[i]
		}
		_ = ex.Send(ctx, Message{Protocol: echo, Opcode: m.Opcode + 1, Payload: reply})
		ex.Close()
	}
}

func TestExchangeRequestResponse(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	var calls atomic.Int32
	dev.t.Handle(echo, echoOps, echoHandler(&calls))

	ex, err := cli.t.NewUnsecuredSession(dev.addr()).NewExchange()
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Send(ctx(t), Message{Protocol: echo, Opcode: 1, Payload: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	m, err := ex.Recv(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if m.Opcode != 2 || string(m.Payload) != "cba" || m.Protocol != echo {
		t.Errorf("reply %+v", m)
	}
	// The reply carried the ack of the request.
	if err := ex.WaitAck(ctx(t)); err != nil {
		t.Errorf("WaitAck: %v", err)
	}
	ex.Close() // acknowledges the reply at once

	// The device's exchange goes away once the client's ack arrives.
	eventually(t, "the device to forget the exchange", func() bool {
		dev.t.mu.Lock()
		defer dev.t.mu.Unlock()
		for _, s := range dev.t.unsecured {
			if len(s.exchanges) > 0 {
				return false
			}
		}
		return true
	})
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times", calls.Load())
	}
	// Request, (piggybacked) reply, standalone ack: nothing retransmitted.
	if got := cli.conn.writes.Load() + dev.conn.writes.Load(); got != 3 {
		t.Errorf("%d datagrams, want 3", got)
	}
}

func TestStandaloneAck(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	release := make(chan struct{})
	dev.t.Handle(echo, echoOps, func(ctx context.Context, ex *Exchange, _ Message) {
		<-release // no reply to piggyback on
		ex.Close()
	})
	defer close(release)

	ex, _ := cli.t.NewUnsecuredSession(dev.addr()).NewExchange()
	if err := ex.Send(ctx(t), Message{Protocol: echo}); err != nil {
		t.Fatal(err)
	}
	if err := ex.WaitAck(ctx(t)); err != nil {
		t.Fatalf("WaitAck: %v", err)
	}
	pkts := dev.conn.packets()
	if len(pkts) == 0 {
		t.Fatal("device sent nothing")
	}
	for _, p := range pkts {
		_, n, _ := ParseHeader(p)
		ph, _, _ := ParseProtocolHeader(p[n:])
		if ph.Protocol != SecureChannel || ph.Opcode != OpStandaloneAck || !ph.Ack || ph.Reliable {
			t.Errorf("device sent %+v, want only standalone acks", ph)
		}
	}
}

func TestRetransmitsDroppedMessage(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	var calls atomic.Int32
	dev.t.Handle(echo, echoOps, echoHandler(&calls))
	cli.conn.fault = func(n int, _ []byte) int {
		if n == 0 {
			return 0 // lose the first request
		}
		return 1
	}

	ex, _ := cli.t.NewUnsecuredSession(dev.addr()).NewExchange()
	if err := ex.Send(ctx(t), Message{Protocol: echo, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	m, err := ex.Recv(ctx(t))
	if err != nil || string(m.Payload) != "ih" {
		t.Fatalf("Recv: %q, %v", m.Payload, err)
	}
	ex.Close()
	pkts := cli.conn.packets()
	if len(pkts) < 2 || !bytes.Equal(pkts[0], pkts[1]) {
		t.Errorf("client sent %d datagrams; the second must repeat the first, counter and all", len(pkts))
	}
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times", calls.Load())
	}
}

func TestDropsDuplicate(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	var calls atomic.Int32
	dev.t.Handle(echo, echoOps, echoHandler(&calls))
	cli.conn.fault = func(n int, _ []byte) int {
		if n == 0 {
			return 2 // the network repeats the request
		}
		return 1
	}

	ex, _ := cli.t.NewUnsecuredSession(dev.addr()).NewExchange()
	if err := ex.Send(ctx(t), Message{Protocol: echo}); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Recv(ctx(t)); err != nil {
		t.Fatal(err)
	}
	ex.Close()
	eventually(t, "the duplicate to be logged", func() bool {
		return strings.Contains(dev.log.String(), "Dropped duplicate message")
	})
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times, want once", calls.Load())
	}
}

func TestGivesUpAfterMaxTransmissions(t *testing.T) {
	cli := newNode(t)
	nobody, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nobody.Close() }()

	ex, _ := cli.t.NewUnsecuredSession(nobody.LocalAddr().(*net.UDPAddr).AddrPort()).NewExchange()
	if err := ex.Send(ctx(t), Message{Protocol: echo}); err != nil {
		t.Fatal(err)
	}
	if err := ex.WaitAck(ctx(t)); !errors.Is(err, ErrNoAck) {
		t.Fatalf("WaitAck: %v, want ErrNoAck", err)
	}
	if got := cli.conn.writes.Load(); got != defaultMaxTransmissions {
		t.Errorf("sent %d times, want %d", got, defaultMaxTransmissions)
	}
	if _, err := ex.Recv(ctx(t)); !errors.Is(err, ErrNoAck) {
		t.Errorf("Recv after giving up: %v, want ErrNoAck", err)
	}
}

func TestRetransmitBackoff(t *testing.T) {
	s := &Session{mrp: MRPConfig{Idle: 500 * time.Millisecond, Active: 300 * time.Millisecond, ActiveThreshold: 4 * time.Second}}
	for n, base := range []float64{1, 1, 1.6, 2.56, 4.096} {
		lo := time.Duration(500 * 1.1 * base * float64(time.Millisecond))
		hi := time.Duration(float64(lo) * 1.25)
		if got := s.retransmitAfter(n); got < lo || got > hi {
			t.Errorf("idle, transmission %d: %v, want %v to %v", n, got, lo, hi)
		}
	}
	s.heard = time.Now()
	if got := s.retransmitAfter(0); got < 330*time.Millisecond || got > 413*time.Millisecond {
		t.Errorf("active: %v, want 330ms to 412.5ms", got)
	}
}

func TestDropsUnknownSession(t *testing.T) {
	dev := newNode(t)
	raw, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dev.addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()

	h := Header{SessionID: 1234, Counter: 1}
	ph := ProtocolHeader{Initiator: true, Reliable: true, Protocol: echo}
	if _, err := raw.Write(ph.Append(h.Append(nil))); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the drop to be logged", func() bool {
		return strings.Contains(dev.log.String(), `"reason"="unknown session"`)
	})
	_ = raw.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := raw.Read(make([]byte, 64)); err == nil {
		t.Errorf("device answered with %d bytes", n)
	}
	if dev.conn.writes.Load() != 0 {
		t.Errorf("device sent %d datagrams", dev.conn.writes.Load())
	}
}

// xorSecurity stands in for AES-CCM: it hides the payload and checks a MAC over header and
// payload, so the tests see the session hook used both ways.
type xorSecurity struct{ key byte }

func (x xorSecurity) mac(hdr, payload []byte) []byte {
	sum := sha256.Sum256(append(append([]byte{x.key}, hdr...), payload...))
	return sum[:16]
}

func (x xorSecurity) Seal(_ *Header, hdr, payload []byte) ([]byte, error) {
	out := make([]byte, len(payload), len(payload)+16)
	for i, b := range payload {
		out[i] = b ^ x.key
	}
	return append(out, x.mac(hdr, payload)...), nil
}

func (x xorSecurity) Open(_ *Header, hdr, sealed []byte) ([]byte, error) {
	if len(sealed) < 16 {
		return nil, errors.New("short")
	}
	out := make([]byte, len(sealed)-16)
	for i, b := range sealed[:len(out)] {
		out[i] = b ^ x.key
	}
	if !bytes.Equal(x.mac(hdr, out), sealed[len(out):]) {
		return nil, errors.New("bad MAC")
	}
	return out, nil
}

// securePair adds a secure session between cli and dev, and returns the client's end.
func securePair(t *testing.T, cli, dev *node) *Session {
	t.Helper()
	cid, err := cli.t.AllocateSessionID()
	if err != nil {
		t.Fatal(err)
	}
	did, err := dev.t.AllocateSessionID()
	if err != nil {
		t.Fatal(err)
	}
	sec := xorSecurity{key: 0x5A}
	dev.t.AddSecureSession(SecureSessionConfig{LocalSessionID: did, PeerSessionID: cid, Peer: cli.addr(), Security: sec})
	return cli.t.AddSecureSession(SecureSessionConfig{LocalSessionID: cid, PeerSessionID: did, Peer: dev.addr(), Security: sec})
}

func TestSecureSession(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	var calls atomic.Int32
	dev.t.Handle(echo, echoOps, echoHandler(&calls))
	s := securePair(t, cli, dev)

	ex, _ := s.NewExchange()
	if err := ex.Send(ctx(t), Message{Protocol: echo, Payload: []byte("secret")}); err != nil {
		t.Fatal(err)
	}
	m, err := ex.Recv(ctx(t))
	if err != nil || string(m.Payload) != "terces" {
		t.Fatalf("Recv: %q, %v", m.Payload, err)
	}
	ex.Close()
	for _, p := range cli.conn.packets() {
		if bytes.Contains(p, []byte("secret")) {
			t.Errorf("payload sent in the clear: % x", p)
		}
	}
}

func TestDropsTamperedSecureMessage(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	var calls atomic.Int32
	dev.t.Handle(echo, echoOps, echoHandler(&calls))
	s := securePair(t, cli, dev)
	cli.conn.fault = func(_ int, p []byte) int {
		p[len(p)-1] ^= 1 // corrupt every send
		return 1
	}
	ex, _ := s.NewExchange()
	_ = ex.Send(ctx(t), Message{Protocol: echo})
	eventually(t, "the failed authentication to be logged", func() bool {
		return strings.Contains(dev.log.String(), "failed authentication")
	})
	if calls.Load() != 0 || dev.conn.writes.Load() != 0 {
		t.Errorf("device handled %d and sent %d", calls.Load(), dev.conn.writes.Load())
	}
}

// TestSecureCounterWindow replays old messages of a secure session: one inside the window is
// acknowledged again but not delivered, and one behind it is dropped without an answer.
func TestSecureCounterWindow(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	var calls atomic.Int32
	dev.t.Handle(echo, echoOps, echoHandler(&calls))
	s := securePair(t, cli, dev)

	request := func() []byte { // one exchange; returns the request's datagram
		before := len(cli.conn.packets())
		ex, _ := s.NewExchange()
		if err := ex.Send(ctx(t), Message{Protocol: echo}); err != nil {
			t.Fatal(err)
		}
		if _, err := ex.Recv(ctx(t)); err != nil {
			t.Fatal(err)
		}
		ex.Close()
		return cli.conn.packets()[before]
	}
	old := request()
	// Each exchange uses two counters: the request and the closing ack.
	for range windowSize / 2 {
		request()
	}
	recent := request()
	eventually(t, "every exchange to finish", func() bool { return calls.Load() == windowSize/2+2 })

	raw, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	devAddr := net.UDPAddrFromAddrPort(dev.addr())

	writes := dev.conn.writes.Load()
	if _, err := raw.WriteTo(old, devAddr); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the old message to be dropped", func() bool {
		return strings.Contains(dev.log.String(), "message counter behind the window")
	})
	if got := dev.conn.writes.Load(); got != writes {
		t.Errorf("device answered a message behind the window (%d datagrams)", got-writes)
	}

	if _, err := raw.WriteTo(recent, devAddr); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the recent duplicate to be acknowledged", func() bool { return dev.conn.writes.Load() == writes+1 })
	if calls.Load() != windowSize/2+2 {
		t.Errorf("a replayed message reached the handler")
	}
}

func TestAllocateSessionID(t *testing.T) {
	tr := New(nil, Options{})
	seen := map[uint16]bool{}
	for range 100 {
		id, err := tr.AllocateSessionID()
		if err != nil || id == 0 || seen[id] {
			t.Fatalf("AllocateSessionID() = %d, %v", id, err)
		}
		seen[id] = true
	}
	for id := range seen {
		tr.ReleaseSessionID(id)
	}
	if len(tr.secure) != 0 {
		t.Errorf("%d IDs left after releasing them all", len(tr.secure))
	}
}

// TestDualStack serves one socket to clients over IPv6 and IPv4, as a node listening on
// "[::]:5540" does.
func TestDualStack(t *testing.T) {
	dev, err := Listen("[::]:0", Options{MRP: fastMRP})
	if err != nil {
		t.Skipf("no IPv6: %v", err)
	}
	var calls atomic.Int32
	dev.Handle(echo, echoOps, echoHandler(&calls))
	c, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dev.Serve(c) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	port := dev.Addr().(*net.UDPAddr).AddrPort().Port()

	for _, ip := range []string{"::1", "127.0.0.1"} {
		pc, err := net.ListenPacket("udp", net.JoinHostPort(ip, "0"))
		if err != nil {
			t.Logf("no %s: %v", ip, err)
			continue
		}
		cli := New(pc, Options{MRP: fastMRP})
		cc, stop := context.WithCancel(context.Background())
		served := make(chan struct{})
		go func() { _ = cli.Serve(cc); close(served) }()

		ex, _ := cli.NewUnsecuredSession(netip.AddrPortFrom(netip.MustParseAddr(ip), port)).NewExchange()
		if err := ex.Send(ctx(t), Message{Protocol: echo, Payload: []byte("ok")}); err != nil {
			t.Fatal(err)
		}
		if m, err := ex.Recv(ctx(t)); err != nil || string(m.Payload) != "ko" {
			t.Errorf("over %s: %q, %v", ip, m.Payload, err)
		}
		ex.Close()
		stop()
		<-served
	}
	if calls.Load() == 0 {
		t.Skip("neither loopback address was available")
	}
}

func TestLateMessageDoesNotStartAnExchange(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	var calls atomic.Int32
	dev.t.Handle(echo, echoOps, echoHandler(&calls))

	ex, _ := cli.t.NewUnsecuredSession(dev.addr()).NewExchange()
	if err := ex.Send(ctx(t), Message{Protocol: echo, Opcode: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Recv(ctx(t)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the device to close its exchange", func() bool {
		dev.t.mu.Lock()
		defer dev.t.mu.Unlock()
		for _, s := range dev.t.unsecured {
			return len(s.exchanges) == 0
		}
		return false
	})
	// A follow-up on the ended exchange, with an opcode that doesn't start one.
	if err := ex.Send(ctx(t), Message{Protocol: echo, Opcode: 9}); err != nil {
		t.Fatal(err)
	}
	if err := ex.WaitAck(ctx(t)); err != nil {
		t.Errorf("the late message wasn't acknowledged: %v", err)
	}
	ex.Close()
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times, want once", calls.Load())
	}
}

func TestFullInboxIsRetransmittedLater(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	drain := make(chan struct{})
	got := make(chan []byte, 2*maxInbox)
	dev.t.Handle(echo, echoOps, func(ctx context.Context, ex *Exchange, m Message) {
		got <- m.Payload
		<-drain
		for range maxInbox + 1 {
			m, err := ex.Recv(ctx)
			if err != nil {
				return
			}
			got <- m.Payload
		}
		ex.Close()
	})

	ex, _ := cli.t.NewUnsecuredSession(dev.addr()).NewExchange()
	// The first starts the exchange, maxInbox fill its inbox, and one more doesn't fit.
	for i := range maxInbox + 2 {
		if err := ex.Send(ctx(t), Message{Protocol: echo, Payload: []byte{byte(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the inbox to fill", func() bool { return strings.Contains(dev.log.String(), "exchange inbox full") })
	close(drain)
	if err := ex.WaitAck(ctx(t)); err != nil {
		t.Fatalf("the last message was never acknowledged: %v", err)
	}
	ex.Close()
	for i := range maxInbox + 2 {
		select {
		case p := <-got:
			if p[0] != byte(i) {
				t.Fatalf("message %d arrived as %d", i, p[0])
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("message %d never arrived", i)
		}
	}
}

func TestBusyUnsecuredSessionsAreNotEvicted(t *testing.T) {
	dev := newNode(t)
	release := make(chan struct{})
	var calls atomic.Int32
	dev.t.Handle(echo, echoOps, func(ctx context.Context, ex *Exchange, _ Message) {
		calls.Add(1)
		<-release // a PASE in progress, say
		ex.Close()
	})
	raw, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dev.addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	start := func(node uint64) {
		h := Header{Counter: 1, HasSource: true, Source: node}
		ph := ProtocolHeader{Initiator: true, Reliable: true, Protocol: echo, ExchangeID: 1}
		if _, err := raw.Write(ph.Append(h.Append(nil))); err != nil {
			t.Fatal(err)
		}
	}
	for i := range maxUnsecuredSessions {
		start(uint64(i + 1))
	}
	eventually(t, "every session to start", func() bool { return calls.Load() == maxUnsecuredSessions })
	start(1000)
	eventually(t, "the extra session to be refused", func() bool {
		return strings.Contains(dev.log.String(), "too many unsecured sessions")
	})
	if calls.Load() != maxUnsecuredSessions {
		t.Errorf("a busy session was evicted for a new one")
	}

	close(release) // every exchange ends, so their sessions become idle
	eventually(t, "the sessions to go idle", func() bool {
		dev.t.mu.Lock()
		defer dev.t.mu.Unlock()
		for _, s := range dev.t.unsecured {
			if len(s.exchanges) > 0 {
				return false
			}
		}
		return true
	})
	start(1001)
	eventually(t, "an idle session to make room", func() bool { return calls.Load() == maxUnsecuredSessions+1 })
}

func TestAddSecureSessionReplaces(t *testing.T) {
	dev, cli := newNode(t), newNode(t)
	first := securePair(t, cli, dev)
	ex, _ := first.NewExchange()
	second := cli.t.AddSecureSession(SecureSessionConfig{LocalSessionID: first.LocalSessionID(), PeerSessionID: 1, Peer: dev.addr(), Security: xorSecurity{}})
	if _, err := ex.Recv(ctx(t)); !errors.Is(err, ErrClosed) {
		t.Errorf("an exchange on the replaced session: %v, want ErrClosed", err)
	}
	if _, err := first.NewExchange(); !errors.Is(err, ErrClosed) {
		t.Errorf("the replaced session still takes exchanges: %v", err)
	}
	if _, err := second.NewExchange(); err != nil {
		t.Errorf("the new session: %v", err)
	}
}

// TestServeStopsHandlersOnSocketFailure closes the socket under a running Handler: Serve must
// cancel the Handler's context and return, not wait for it forever.
func TestServeStopsHandlersOnSocketFailure(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dev := New(pc, Options{MRP: fastMRP})
	started := make(chan struct{})
	dev.Handle(echo, echoOps, func(ctx context.Context, _ *Exchange, _ Message) {
		close(started)
		<-ctx.Done() // blocked on something other than its exchange
	})
	done := make(chan error, 1)
	go func() { done <- dev.Serve(context.Background()) }()

	cli := newNode(t)
	ex, _ := cli.t.NewUnsecuredSession(pc.LocalAddr().(*net.UDPAddr).AddrPort()).NewExchange()
	if err := ex.Send(ctx(t), Message{Protocol: echo}); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = pc.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve returned no error for a failed socket")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve didn't return")
	}
}
