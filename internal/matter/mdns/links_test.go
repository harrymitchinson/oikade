package mdns

import (
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// testLinks is the interfaces and addresses a test's host has, which it changes.
type testLinks struct {
	mu     sync.Mutex
	ifaces []net.Interface
	addrs  map[int][]netip.Addr
}

func (l *testLinks) list() ([]net.Interface, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ifaces), nil
}

func (l *testLinks) of(ifIndex int) []netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.addrs[ifIndex])
}

func (l *testLinks) set(ifIndex int, addrs ...netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addrs[ifIndex] = addrs
}

func (l *testLinks) up(ifi net.Interface, addrs ...netip.Addr) {
	l.mu.Lock()
	l.ifaces = append(l.ifaces, ifi)
	l.mu.Unlock()
	l.set(ifi.Index, addrs...)
}

func (l *testLinks) down(ifIndex int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ifaces = slices.DeleteFunc(l.ifaces, func(ifi net.Interface) bool { return ifi.Index == ifIndex })
	delete(l.addrs, ifIndex)
}

// newLinkedResponder returns a test Responder whose interfaces come from links, which start as
// testIf with testAddr4 and testAddr6, and that has announced testService.
func newLinkedResponder(t *testing.T) (*Responder, *fakeConn, *testLinks) {
	t.Helper()
	links := &testLinks{
		ifaces: []net.Interface{{Index: testIf, Name: "en0"}},
		addrs:  map[int][]netip.Addr{testIf: {testAddr4, testAddr6}},
	}
	r, c := newTestResponder(t)
	t.Cleanup(func() { _ = r.Close() })
	r.addrs, r.interfaces = links.of, links.list
	addReady(t, r, testService())
	return r, c, links
}

// addrs returns the A and AAAA records among rs, as name and address.
func addrsIn(rs []dnsmessage.Resource) (out []string) {
	for _, r := range rs {
		switch b := r.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(b.A).String())
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(b.AAAA).String())
		}
	}
	slices.Sort(out)
	return out
}

func TestUnchangedInterfacesSendNothing(t *testing.T) {
	r, c, _ := newLinkedResponder(t)
	r.checkLinks()
	c.none(t)
}

func TestAChangedAddressIsAnnouncedAndTheOldOneSaidGoodbyeTo(t *testing.T) {
	r, c, links := newLinkedResponder(t)
	moved := netip.MustParseAddr("192.0.2.2")
	links.set(testIf, moved, testAddr6) // a new DHCP lease

	r.checkLinks()

	bye := c.next(t)
	if !bye.msg.Response || bye.ifIndex != testIf {
		t.Fatalf("first sent %+v, want a goodbye on the interface", bye)
	}
	if got := addrsIn(bye.msg.Answers); !slices.Equal(got, []string{testAddr4.String()}) {
		t.Errorf("goodbye to %v, want the old address", got)
	}
	for _, a := range bye.msg.Answers {
		if a.Header.TTL != 0 {
			t.Errorf("goodbye's %v has TTL %d", a.Header.Name, a.Header.TTL)
		}
	}
	for i := range 2 {
		p := c.next(t)
		if !p.msg.Response || p.ifIndex != testIf {
			t.Fatalf("announcement %d: %+v", i, p)
		}
		// Every address the host has there, as the cache-flush bit drops the ones left out.
		if got, want := addrsIn(p.msg.Answers), []string{moved.String(), testAddr6.String()}; !slices.Equal(got, want) {
			t.Errorf("announcement %d has addresses %v, want %v", i, got, want)
		}
		if got := summarise(p.msg.Answers); !sameSet(got, announced(-1)) {
			t.Errorf("announcement %d: %v, want the service with the addresses", i, got)
		}
	}
	c.none(t)
	if !c.isJoined(testIf) {
		t.Error("didn't try to join the groups on the interface again")
	}

	// An answer carries the new address, and the next look finds nothing new.
	if got := addressRecords(r.host, r.addrs(testIf)); len(got) != 2 {
		t.Errorf("answers with %d addresses", len(got))
	}
	r.checkLinks()
	c.none(t)
}

func TestAnInterfaceThatComesUpIsJoinedAndAnnouncedOn(t *testing.T) {
	r, c, links := newLinkedResponder(t)
	const wifi = testIf + 1
	addr := netip.MustParseAddr("198.51.100.7")
	links.up(net.Interface{Index: wifi, Name: "wlan0"}, addr)

	r.checkLinks()

	if !c.isJoined(wifi) {
		t.Fatal("didn't join the groups on the new interface")
	}
	if !slices.Equal(r.served(), []int{testIf, wifi}) {
		t.Errorf("serves %v", r.served())
	}
	if r.zoneNames()[wifi] != "wlan0" {
		t.Errorf("zones %v", r.zoneNames())
	}
	for i := range 2 {
		p := c.next(t)
		if !p.msg.Response || p.ifIndex != wifi {
			t.Fatalf("announcement %d went to interface %d: %+v", i, p.ifIndex, p)
		}
		if got := addrsIn(p.msg.Answers); !slices.Equal(got, []string{addr.String()}) {
			t.Errorf("announcement %d has addresses %v, want the new interface's", i, got)
		}
	}
	c.none(t)

	links.down(wifi)
	r.checkLinks()
	if c.isJoined(wifi) {
		t.Error("didn't leave the groups on the interface that went")
	}
	if !slices.Equal(r.served(), []int{testIf}) {
		t.Errorf("serves %v after the interface went", r.served())
	}
	c.none(t) // nothing can be sent on an interface that's gone
}

func TestNothingIsAnnouncedOnANewInterfaceBeforeTheHostIsProbed(t *testing.T) {
	links := &testLinks{
		ifaces: []net.Interface{{Index: testIf, Name: "en0"}},
		addrs:  map[int][]netip.Addr{testIf: {testAddr4}},
	}
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	r.addrs, r.interfaces = links.of, links.list
	links.up(net.Interface{Index: 2, Name: "wlan0"}, netip.MustParseAddr("198.51.100.7"))
	links.set(testIf, netip.MustParseAddr("192.0.2.2"))

	r.checkLinks()

	if !c.isJoined(2) {
		t.Error("didn't join the groups on the new interface")
	}
	c.none(t)
}

func TestFixedInterfacesFollowOnlyThoseGiven(t *testing.T) {
	all, err := net.Interfaces()
	if err != nil || len(all) == 0 {
		t.Skip("no interfaces to look at")
	}
	given := []net.Interface{all[0], {Index: 1 << 20, Name: "gone0"}}
	got, err := fixedInterfaces(given)()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifi := range got {
		if ifi.Index != all[0].Index {
			t.Errorf("lists %s, which wasn't given or doesn't exist", ifi.Name)
		}
	}
	if all[0].Flags&net.FlagUp != 0 && len(got) != 1 {
		t.Errorf("lists %v, want %s", got, all[0].Name)
	}
}
