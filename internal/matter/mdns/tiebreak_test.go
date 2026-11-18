package mdns

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/harrymitchinson/oikade/internal/logging"
)

func TestTypesComeBeforeData(t *testing.T) {
	// Another host that proposes only an SRV loses to our TXT and SRV, whatever its port: sorted,
	// our TXT (16) meets its SRV (33) first (RFC 6762, 8.2.1).
	r, c := newTestResponder(t)
	r.timing.tieWait = 20 * time.Millisecond
	defer func() { _ = r.Close() }()
	errc := make(chan error, 1)
	go func() { errc <- r.Add(t.Context(), testService()) }()
	c.next(t)
	name := mustName(testInstance)
	r.handle(c, testIf, peer, pack(t, dnsmessage.Message{
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET}},
		Authorities: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: hostTTL},
			Body:   &dnsmessage.SRVResource{Port: 1, Target: mustName("other.local.")},
		}},
	}))
	if n := probesBeforeAnnouncing(t, c); n != 3 {
		t.Errorf("sent %d probes, want 3 more after losing", n)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

// newHostResponder returns a test Responder named host with one address, on its own fakeConn.
func newHostResponder(t *testing.T, host string, addr netip.Addr) (*Responder, *fakeConn) {
	t.Helper()
	c := newFakeConn(t)
	r, err := newResponder([]packetConn{c}, []int{testIf}, host, func(int) []netip.Addr { return []netip.Addr{addr} },
		logging.Test(t).With("host", host), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.timing = timing{probeInterval: 10 * time.Millisecond, announceGap: 30 * time.Millisecond, tieWait: 20 * time.Millisecond}
	t.Cleanup(func() { _ = r.Close() })
	return r, c
}

// link delivers what each of two fakeConns sends to the other, as two hosts on one link hear each
// other, until the test ends.
func link(t *testing.T, a, b *fakeConn) {
	t.Helper()
	forward := func(from, to *fakeConn) {
		for {
			select {
			case p := <-from.sent:
				b, err := p.msg.Pack()
				if err != nil {
					t.Error(err)
					return
				}
				select {
				case to.in <- b:
				case <-to.closed:
					return
				}
			case <-from.closed:
				return
			}
		}
	}
	go forward(a, b)
	go forward(b, a)
}

func TestTheLaterOfTwoSimultaneousProbesWins(t *testing.T) {
	r1, c1 := newHostResponder(t, "HOSTONE", netip.MustParseAddr("192.0.2.1"))
	r2, c2 := newHostResponder(t, "HOSTTWO", netip.MustParseAddr("192.0.2.2"))
	link(t, c1, c2)
	earlier, later := testService(), testService()
	later.Port++ // the SRV's data is later

	errs := make(chan error, 2)
	won := make(chan error, 1)
	go func() { errs <- r1.Add(t.Context(), earlier) }()
	go func() { err := r2.Add(t.Context(), later); won <- err; errs <- err }()

	if err := <-won; err != nil {
		t.Fatalf("the later records' Add: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("the earlier records' Add: %v, want ErrConflict", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the earlier records' Add didn't return")
	}
}

// probesBeforeAnnouncing counts the probes c sends before the first announcement.
func probesBeforeAnnouncing(t *testing.T, c *fakeConn) int {
	t.Helper()
	n := 0
	for {
		p := c.next(t)
		if p.msg.Response {
			return n
		}
		n++
	}
}

// probeFor returns another host's probe for testService's instance, proposing its TXT and an SRV
// with port, so that the SRV decides the tie-break.
func probeFor(t *testing.T, port uint16) []byte {
	t.Helper()
	name := mustName(testInstance)
	return pack(t, dnsmessage.Message{
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET | unicastResponse}},
		Authorities: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: hostTTL},
			Body:   &dnsmessage.SRVResource{Port: port, Target: mustName("other.local.")},
		}, {
			Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: otherTTL},
			Body:   &dnsmessage.TXTResource{TXT: testService().Text},
		}},
	})
}

func TestAProbeThatLosesProbesAgain(t *testing.T) {
	r, c := newTestResponder(t)
	r.timing.tieWait = 20 * time.Millisecond
	defer func() { _ = r.Close() }()
	errc := make(chan error, 1)
	go func() { errc <- r.Add(t.Context(), testService()) }()
	c.next(t) // the first probe

	r.handle(c, testIf, peer, probeFor(t, testService().Port+1))

	// It starts over: three more probes, as nobody answers them.
	if n := probesBeforeAnnouncing(t, c); n != 3 {
		t.Errorf("sent %d probes after losing, want 3", n)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestAProbeThatWinsGoesOn(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	errc := make(chan error, 1)
	go func() { errc <- r.Add(t.Context(), testService()) }()
	c.next(t)

	r.handle(c, testIf, peer, probeFor(t, testService().Port-1))

	if n := probesBeforeAnnouncing(t, c); n != 2 {
		t.Errorf("sent %d more probes, want the other 2", n)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestOurOwnProbeIsntATie(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	errc := make(chan error, 1)
	go func() { errc <- r.Add(t.Context(), testService()) }()
	first := c.next(t)

	// Multicast loopback brings our probe back, host addresses and all.
	r.handle(c, testIf, peer, pack(t, first.msg))

	if n := probesBeforeAnnouncing(t, c); n != 2 {
		t.Errorf("sent %d more probes, want the other 2", n)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestAProbeThatKeepsLosingGivesUp(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	errc := make(chan error, 1)
	go func() { errc <- r.Add(t.Context(), testService()) }()
	for {
		select {
		case err := <-errc:
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("Add: %v, want ErrConflict", err)
			}
			return
		case p := <-c.sent:
			if p.msg.Response {
				t.Fatal("announced, though every probe lost")
			}
			r.handle(c, testIf, peer, probeFor(t, testService().Port+1))
		case <-time.After(2 * time.Second):
			t.Fatal("Add didn't return")
		}
	}
}

func TestCompareSets(t *testing.T) {
	a := record{name: mustName(testHost), typ: dnsmessage.TypeA, unique: true, ttl: hostTTL,
		body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}}}
	b := record{name: mustName(testHost), typ: dnsmessage.TypeA, unique: true, ttl: hostTTL,
		body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 2}}}
	aaaa := record{name: mustName(testHost), typ: dnsmessage.TypeAAAA, unique: true, ttl: hostTTL,
		body: &dnsmessage.AAAAResource{AAAA: testAddr6.As16()}}
	res := func(recs ...record) []dnsmessage.Resource {
		out := make([]dnsmessage.Resource, 0, len(recs))
		for _, r := range recs {
			out = append(out, r.resource(r.ttl, false))
		}
		return out
	}
	for _, tc := range []struct {
		name         string
		ours, theirs []dnsmessage.Resource
		want         int
	}{
		{"later data wins", res(b), res(a), 1},
		{"earlier data loses", res(a), res(b), -1},
		{"the same", res(a, aaaa), res(aaaa, a), 0},
		{"a later type wins over data", res(aaaa), res(b), 1},
		{"more records win", res(a, aaaa), res(a), 1},
		{"in any order", res(aaaa, a), res(b), -1},
	} {
		if got := compareSets(tc.ours, tc.theirs); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// The cache-flush bit isn't part of the class that's compared.
	flushed := res(a)
	flushed[0].Header.Class |= cacheFlush
	if compareSets(flushed, res(a)) != 0 {
		t.Error("the cache-flush bit counted")
	}
}
