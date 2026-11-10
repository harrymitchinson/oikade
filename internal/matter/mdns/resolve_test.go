package mdns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/harrymitchinson/oikade/internal/logging"
)

// hub is the controller the tests look for, on another host.
var (
	hubHost = mustName("HUBHOST0001.local.")
	hubOp   = Operational{CompressedFabricID: 0x2906C908D115D362, NodeID: 0x20317E3F, Port: 5540}
)

// hubResponse packs the hub's answer to a query for its SRV: the SRV, and its addresses as
// additional records unless addrs is empty.
func hubResponse(t *testing.T, ttl int64, addrs ...netip.Addr) []byte {
	t.Helper()
	svc := hubOp.Service()
	recs, err := svc.records(hubHost)
	if err != nil {
		t.Fatal(err)
	}
	m := dnsmessage.Message{Response: true, Authoritative: true}
	for _, rec := range recs {
		if rec.typ == dnsmessage.TypeSRV {
			m.Answers = append(m.Answers, rec.resource(uint32(max(ttl, 0)), true))
		}
	}
	for _, rec := range addressRecords(hubHost, addrs) {
		m.Additionals = append(m.Additionals, rec.resource(uint32(max(ttl, 0)), true))
	}
	return pack(t, m)
}

// resolveAsync runs ResolveOperational for the hub, and returns its result on a channel.
func resolveAsync(ctx context.Context, r *Responder) <-chan result {
	done := make(chan result, 1)
	go func() {
		got, err := r.ResolveOperational(ctx, hubOp.CompressedFabricID, hubOp.NodeID)
		done <- result{got, err}
	}()
	return done
}

type result struct {
	Resolved
	err error
}

func shortRequery(t *testing.T, d time.Duration) {
	old := firstRequery
	firstRequery = d
	t.Cleanup(func() { firstRequery = old })
}

// questions returns what a sent query asks, as "name type".
func questions(p sentPacket) []string {
	out := make([]string, 0, len(p.msg.Questions))
	for _, q := range p.msg.Questions {
		out = append(out, q.Name.String()+" "+q.Type.String())
	}
	return out
}

// TestResolveOperational: the lookup asks for the instance's SRV over multicast, and an answer
// with the target's addresses resolves it, a link-local one with its interface's zone.
func TestResolveOperational(t *testing.T) {
	r, c := newTestResponder(t)
	r.zone = func(int) string { return "en0" }
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := resolveAsync(ctx, r)

	q := c.next()
	want := []string{"2906C908D115D362-0000000020317E3F._matter._tcp.local. TypeSRV"}
	if q.dst != nil || !slices.Equal(questions(q), want) {
		t.Fatalf("sent %v to %v, want a multicast query for %v", questions(q), q.dst, want)
	}
	c.in <- hubResponse(t, hostTTL, netip.MustParseAddr("192.0.2.15"), netip.MustParseAddr("fe80::15"))
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	wantAddrs := []netip.AddrPort{netip.MustParseAddrPort("192.0.2.15:5540"), netip.MustParseAddrPort("[fe80::15%en0]:5540")}
	if got.Host != "HUBHOST0001.local." || got.Port != 5540 || !slices.Equal(got.Addrs, wantAddrs) {
		t.Errorf("resolved %+v, want host HUBHOST0001.local., port 5540, %v", got.Resolved, wantAddrs)
	}
}

// TestResolveAsksForAddresses: an SRV answer without addresses makes the next query ask for the
// target's A and AAAA.
func TestResolveAsksForAddresses(t *testing.T) {
	shortRequery(t, 20*time.Millisecond)
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := resolveAsync(ctx, r)
	c.next() // the SRV query
	c.in <- hubResponse(t, hostTTL)
	want := []string{"HUBHOST0001.local. TypeA", "HUBHOST0001.local. TypeAAAA"}
	for q := c.next(); !slices.Equal(questions(q), want); q = c.next() {
		t.Logf("sent %v", questions(q)) // the SRV query again, if the requery came first
	}
	resp := dnsmessage.Message{Response: true, Authoritative: true}
	for _, rec := range addressRecords(hubHost, []netip.Addr{netip.MustParseAddr("192.0.2.15")}) {
		resp.Answers = append(resp.Answers, rec.resource(rec.ttl, true))
	}
	c.in <- pack(t, resp)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if !slices.Equal(got.Addrs, []netip.AddrPort{netip.MustParseAddrPort("192.0.2.15:5540")}) {
		t.Errorf("resolved %v", got.Addrs)
	}
}

// TestResolveNotFound: nothing answers, or only another instance and a goodbye do, so the lookup
// asks again with a growing interval and gives up when its context ends.
func TestResolveNotFound(t *testing.T) {
	shortRequery(t, 20*time.Millisecond)
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	done := resolveAsync(ctx, r)
	c.next()
	c.in <- hubResponse(t, 0, netip.MustParseAddr("192.0.2.15")) // a goodbye
	other := Operational{CompressedFabricID: hubOp.CompressedFabricID, NodeID: 1}.Service()
	recs, _ := other.records(hubHost)
	b, err := response(slices.Concat(recs, addressRecords(hubHost, []netip.Addr{netip.MustParseAddr("192.0.2.16")})), -1)
	if err != nil {
		t.Fatal(err)
	}
	c.in <- b
	err = (<-done).err
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("gave up after %v, with a 200 ms timeout", d)
	}
	n := 1
	for len(c.sent) > 0 {
		if p := <-c.sent; len(p.msg.Questions) > 0 {
			n++
		}
	}
	if n < 3 || n > 5 { // at 0, 20, 60 and 140 ms
		t.Errorf("sent %d queries in 200 ms, want about 4", n)
	}
}

// TestResolveFromAnotherResponder resolves an operational service that another Responder on this
// host advertises, over a real interface.
func TestResolveFromAnotherResponder(t *testing.T) {
	ifi := multicastInterface(t)
	opts := func(host string) Options {
		return Options{Log: logging.Test(t), Interfaces: []net.Interface{ifi}, Host: host + randomHex(4)}
	}
	node, err := Listen(opts("oikade-node-"))
	if err != nil {
		t.Skipf("Could not listen for mDNS on %s: %v", ifi.Name, err)
	}
	defer func() { _ = node.Close() }()
	ctrl, err := Listen(opts("oikade-ctrl-"))
	if err != nil {
		t.Skipf("Could not listen for mDNS on %s: %v", ifi.Name, err)
	}
	defer func() { _ = ctrl.Close() }()
	op := Operational{CompressedFabricID: 0x1122334455667788, NodeID: 0xA000 + uint64(time.Now().UnixNano()%0x1000), Port: 5541}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := node.Add(ctx, op.Service()); err != nil {
		t.Fatal(err)
	}
	got, err := ctrl.ResolveOperational(ctx, op.CompressedFabricID, op.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != node.Host() || got.Port != 5541 || len(got.Addrs) == 0 {
		t.Errorf("resolved %+v, want host %s, port 5541 and an address", got, node.Host())
	}
	for _, a := range got.Addrs {
		if !slices.Contains(interfaceAddrs(ifi.Index), a.Addr().WithZone("")) {
			t.Errorf("resolved %v, not an address of %s", a, ifi.Name)
		}
	}
}
