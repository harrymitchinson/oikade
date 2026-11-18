package mdns

import (
	"net"
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

func TestKnownAnswersGetNoNSECInstead(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	addReady(t, r, testService())
	recs := r.answerable(testIf)
	var aaaa record
	for _, rec := range recs {
		if rec.typ == dnsmessage.TypeAAAA {
			aaaa = rec
		}
	}
	// The querier already has our AAAA: nothing is missing, so there's nothing to say.
	m := dnsmessage.Message{
		Questions: []dnsmessage.Question{{Name: r.host, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
		Answers:   []dnsmessage.Resource{aaaa.resource(hostTTL, false)},
	}
	r.handle(c, testIf, peer, pack(t, m))
	c.none(t)
}

func TestAQueryInAnotherCaseGetsOneNSEC(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	r.addrs = func(int) []netip.Addr { return []netip.Addr{testAddr4} }
	addReady(t, r, testService())
	other := mustName("dca632a1b2c3.local.")
	m := dnsmessage.Message{Questions: []dnsmessage.Question{
		{Name: other, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		{Name: other, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET},
	}}
	r.handle(c, testIf, peer, pack(t, m))
	p := c.next(t)
	n := 0
	for _, a := range slices.Concat(p.msg.Answers, p.msg.Additionals) {
		if a.Header.Type == typeNSEC {
			n++
			if a.Header.Name.String() != testHost {
				t.Errorf("NSEC names %v, want our own %s", a.Header.Name, testHost)
			}
		}
	}
	if n != 1 {
		t.Errorf("sent %d NSEC records, want 1", n)
	}
}

func TestACompressedKnownNSECCounts(t *testing.T) {
	ours, ok := nsec(mustName(testHost), addressRecords(mustName(testHost), []netip.Addr{testAddr4}))
	if !ok {
		t.Fatal("no NSEC")
	}
	bitmap, _ := nsecBitmap(ours.body.(*dnsmessage.UnknownResource).Data)
	// The querier's copy points at the owner name, at offset 12, rather than spelling it out.
	theirs := &dnsmessage.UnknownResource{Type: typeNSEC, Data: slices.Concat([]byte{0xC0, 12}, bitmap)}
	if !ours.same(ours.name, typeNSEC, theirs) {
		t.Error("an NSEC with a compressed next name isn't the same record")
	}
	other := &dnsmessage.UnknownResource{Type: typeNSEC, Data: []byte{0xC0, 12, 0, 4, 0x40, 0, 0, 8}}
	if ours.same(ours.name, typeNSEC, other) {
		t.Error("an NSEC with other types is the same record")
	}
}

func TestALegacyQueryGetsNoNSEC(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	r.addrs = func(int) []netip.Addr { return []netip.Addr{testAddr4} }
	addReady(t, r, testService())
	legacy := &net.UDPAddr{IP: peer.IP, Port: 49152}

	r.handle(c, testIf, legacy, query(t, testHost, dnsmessage.TypeAAAA, false))
	p := c.next(t)
	if p.dst != legacy || len(p.msg.Answers) != 0 || len(p.msg.Additionals) != 0 {
		t.Errorf("got %+v, want an empty answer: the host has no AAAA", p.msg)
	}

	r.handle(c, testIf, legacy, query(t, testHost, dnsmessage.TypeA, false))
	p = c.next(t)
	for _, a := range slices.Concat(p.msg.Answers, p.msg.Additionals) {
		if a.Header.Type == typeNSEC {
			t.Errorf("a legacy reply carries %v", a.Header)
		}
	}
}

func TestAnnouncementsCarryNSEC(t *testing.T) {
	r, c, links := newLinkedResponder(t)
	links.set(testIf, testAddr4) // the IPv6 address goes
	r.checkLinks()
	c.next(t) // the goodbye to it
	p := c.next(t)
	for _, a := range p.msg.Answers {
		if a.Header.Type == typeNSEC && a.Header.Name.String() == testHost {
			if _, types := nsecTypes(t, a); !slices.Equal(types, []dnsmessage.Type{dnsmessage.TypeA}) {
				t.Errorf("host's NSEC lists %v, want only A now", types)
			}
			return
		}
	}
	t.Errorf("announcement %v has no NSEC for the host", summarise(p.msg.Answers))
}
