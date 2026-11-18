package mdns

import (
	"net/netip"
	"slices"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// nsecTypes reads an NSEC record's next name and the types its bitmap lists, failing t if it isn't
// mDNS's restricted form.
func nsecTypes(t *testing.T, res dnsmessage.Resource) (string, []dnsmessage.Type) {
	t.Helper()
	u, ok := res.Body.(*dnsmessage.UnknownResource)
	if !ok || res.Header.Type != typeNSEC {
		t.Fatalf("%v isn't an NSEC record: %T", res.Header, res.Body)
	}
	d := u.Data
	var labels []string
	for len(d) > 0 && d[0] != 0 {
		n := int(d[0])
		if n+1 > len(d) {
			t.Fatalf("next name runs past the data: %x", u.Data)
		}
		labels = append(labels, string(d[1:1+n]))
		d = d[1+n:]
	}
	if len(d) < 3 || d[1] != 0 || int(d[2]) != len(d)-3 {
		t.Fatalf("not one window-0 bitmap after the name: %x", u.Data)
	}
	var types []dnsmessage.Type
	for i, b := range d[3:] {
		for bit := range 8 {
			if b&(0x80>>bit) != 0 {
				types = append(types, dnsmessage.Type(i*8+bit))
			}
		}
	}
	name := ""
	for _, l := range labels {
		name += l + "."
	}
	return name, types
}

func TestAnIPv4OnlyHostAnswersAAAAWithNSEC(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	r.addrs = func(int) []netip.Addr { return []netip.Addr{testAddr4} }
	addReady(t, r, testService())

	r.handle(c, testIf, peer, query(t, testHost, dnsmessage.TypeAAAA, false))

	p := c.next(t)
	if len(p.msg.Answers) != 1 {
		t.Fatalf("answers %v, want one NSEC", summarise(p.msg.Answers))
	}
	a := p.msg.Answers[0]
	next, types := nsecTypes(t, a)
	if !nameEqual(a.Header.Name, mustName(testHost)) || !nameEqual(mustName(next), mustName(testHost)) {
		t.Errorf("NSEC for %v with next name %s, want %s for both", a.Header.Name, next, testHost)
	}
	if !slices.Equal(types, []dnsmessage.Type{dnsmessage.TypeA}) {
		t.Errorf("NSEC lists %v, want only A", types)
	}
	if a.Header.Class != dnsmessage.ClassINET|cacheFlush || a.Header.TTL != hostTTL {
		t.Errorf("NSEC has class %v and TTL %d", a.Header.Class, a.Header.TTL)
	}
	if len(p.msg.Additionals) != 0 {
		t.Errorf("additionals %v, want none", summarise(p.msg.Additionals))
	}
	c.none(t)
}

func TestAnInstancesNSECListsItsTypes(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	addReady(t, r, testService())

	r.handle(c, testIf, peer, query(t, testInstance, dnsmessage.TypeSRV, false))

	p := c.next(t)
	found := map[string][]dnsmessage.Type{}
	for _, a := range p.msg.Additionals {
		if a.Header.Type == typeNSEC {
			_, types := nsecTypes(t, a)
			found[a.Header.Name.String()] = types
		}
	}
	if got := found[testInstance]; !slices.Equal(got, []dnsmessage.Type{dnsmessage.TypeTXT, dnsmessage.TypeSRV}) {
		t.Errorf("instance's NSEC lists %v, want TXT and SRV", got)
	}
	if got := found[testHost]; !slices.Equal(got, []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA}) {
		t.Errorf("host's NSEC lists %v, want A and AAAA", got)
	}
}

func TestAKnownNSECIsntSentAgain(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	r.addrs = func(int) []netip.Addr { return []netip.Addr{testAddr4} }
	addReady(t, r, testService())
	n, ok := nsec(r.host, r.answerable(testIf))
	if !ok {
		t.Fatal("no NSEC for the host")
	}
	m := dnsmessage.Message{
		Questions: []dnsmessage.Question{{Name: r.host, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
		Answers:   []dnsmessage.Resource{n.resource(hostTTL, false)},
	}
	r.handle(c, testIf, peer, pack(t, m))
	c.none(t)
}

func TestNoNSECForANameThatIsntOurs(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	addReady(t, r, testService())
	r.handle(c, testIf, peer, query(t, "other.local.", dnsmessage.TypeAAAA, false))
	// Shared names, such as a service type, have no NSEC either: other hosts' records share them.
	r.handle(c, testIf, peer, query(t, "_matterc._udp.local.", dnsmessage.TypeSRV, false))
	c.none(t)
}
