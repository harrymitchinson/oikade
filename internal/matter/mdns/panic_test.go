package mdns

import (
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/contain"
)

// TestPanicCostsOnlyThePacket: a panic answering one query is contained and counted, and the
// responder answers the next.
func TestPanicCostsOnlyThePacket(t *testing.T) {
	c := newFakeConn(t)
	var boom atomic.Bool
	addrs := func(int) []netip.Addr {
		if boom.Load() {
			panic("injected")
		}
		return []netip.Addr{testAddr4, testAddr6}
	}
	var mu sync.Mutex
	var sites []string
	r, err := newResponder([]packetConn{c}, []int{testIf}, "DCA632A1B2C3", addrs, logging.Test(t), func(site string) {
		mu.Lock()
		defer mu.Unlock()
		sites = append(sites, site)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	addReady(t, r, testService())

	boom.Store(true)
	c.in <- query(t, testHost, dnsmessage.TypeA, false) // answering it looks up the addresses
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		got := slices.Clone(sites)
		mu.Unlock()
		if len(got) > 0 {
			if !slices.Equal(got, []string{contain.MDNS}) {
				t.Fatalf("panics counted at %v, want one at %s", got, contain.MDNS)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no panic counted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.none()

	boom.Store(false)
	c.in <- query(t, testHost, dnsmessage.TypeA, false)
	if p := c.next(); len(p.msg.Answers) != 1 {
		t.Errorf("after the panic: %+v, want the A record", p.msg)
	}
}
