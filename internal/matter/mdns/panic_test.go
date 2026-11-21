package mdns

import (
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"oikade.dev/internal/logging"
	"oikade.dev/internal/matter/contain"
	"oikade.dev/internal/testkit"
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
	counted := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(sites)
	}
	testkit.Eventually(t, "a panic to be counted", func() bool { return len(counted()) > 0 })
	if got := counted(); !slices.Equal(got, []string{contain.MDNS}) {
		t.Fatalf("panics counted at %v, want one at %s", got, contain.MDNS)
	}
	c.none(t)

	boom.Store(false)
	c.in <- query(t, testHost, dnsmessage.TypeA, false)
	if p := c.next(t); len(p.msg.Answers) != 1 {
		t.Errorf("after the panic: %+v, want the A record", p.msg)
	}
}
