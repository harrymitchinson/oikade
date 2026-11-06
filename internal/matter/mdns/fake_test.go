package mdns

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/harrymitchinson/oikade/internal/logging"
)

const testIf = 1

var (
	testAddr4 = netip.MustParseAddr("192.0.2.1")
	testAddr6 = netip.MustParseAddr("fe80::1")
	peer      = &net.UDPAddr{IP: net.ParseIP("192.0.2.99"), Port: Port}
)

// sentPacket is a packet that a fakeConn sent: multicast on an interface, or to dst.
type sentPacket struct {
	msg     dnsmessage.Message
	ifIndex int
	dst     *net.UDPAddr // nil for multicast
}

// fakeConn is a packetConn that tests feed and watch.
type fakeConn struct {
	t      testing.TB
	in     chan []byte
	sent   chan sentPacket
	closed chan struct{}
	once   sync.Once
}

func newFakeConn(t testing.TB) *fakeConn {
	return &fakeConn{t: t, in: make(chan []byte), sent: make(chan sentPacket, 1024), closed: make(chan struct{})}
}

func (c *fakeConn) ReadPacket(b []byte) (int, int, *net.UDPAddr, error) {
	select {
	case p := <-c.in:
		return copy(b, p), testIf, peer, nil
	case <-c.closed:
		return 0, 0, nil, net.ErrClosed
	}
}

func (c *fakeConn) record(b []byte, ifIndex int, dst *net.UDPAddr) error {
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		c.t.Errorf("sent a packet that doesn't parse: %v", err)
	}
	select {
	case c.sent <- sentPacket{msg: m, ifIndex: ifIndex, dst: dst}:
	default: // full: the fuzz test doesn't read them
	}
	return nil
}

func (c *fakeConn) WriteMulticast(b []byte, ifIndex int) error { return c.record(b, ifIndex, nil) }
func (c *fakeConn) WriteUnicast(b []byte, dst *net.UDPAddr) error {
	return c.record(b, 0, dst)
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// next returns the next packet sent, or fails the test after a second.
func (c *fakeConn) next() sentPacket {
	c.t.Helper()
	select {
	case p := <-c.sent:
		return p
	case <-time.After(time.Second):
		c.t.Fatal("nothing sent")
		return sentPacket{}
	}
}

// none checks that nothing more is sent for a while.
func (c *fakeConn) none() {
	c.t.Helper()
	select {
	case p := <-c.sent:
		c.t.Fatalf("sent %+v", p.msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// newTestResponder returns a Responder on a fakeConn, with short delays.
func newTestResponder(t testing.TB) (*Responder, *fakeConn) {
	c := newFakeConn(t)
	addrs := func(int) []netip.Addr { return []netip.Addr{testAddr4, testAddr6} }
	r, err := newResponder([]packetConn{c}, []int{testIf}, "DCA632A1B2C3", addrs, logging.Test(t))
	if err != nil {
		t.Fatal(err)
	}
	r.timing = timing{probeInterval: 10 * time.Millisecond, announceGap: 30 * time.Millisecond}
	return r, c
}

// addReady adds svc as if it had been probed, without sending anything.
func addReady(t testing.TB, r *Responder, svc Service) {
	recs, err := svc.records(r.host)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := svc.instanceName()
	r.mu.Lock()
	r.services[strings.ToLower(name.String())] = &entry{svc: svc, name: name, records: recs, ready: true}
	r.hostReady = true
	r.mu.Unlock()
}

func pack(t testing.TB, m dnsmessage.Message) []byte {
	t.Helper()
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
