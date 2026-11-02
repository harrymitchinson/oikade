package mdns

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func testService() Service {
	return Service{
		Instance: "B7322C948581262F",
		Type:     "_matterc._udp",
		Subtypes: []string{"_L3840", "_S15", "_CM"},
		Port:     5540,
		Text:     []string{"D=3840", "CM=1"},
	}
}

const (
	testInstance = "B7322C948581262F._matterc._udp.local."
	testHost     = "DCA632A1B2C3.local."
)

// rr is a record in a test's expectations: name, type, TTL and whether the cache-flush bit is set.
type rr struct {
	name  string
	typ   dnsmessage.Type
	ttl   uint32
	flush bool
}

func summarise(rs []dnsmessage.Resource) []rr {
	out := make([]rr, len(rs))
	for i, r := range rs {
		out[i] = rr{r.Header.Name.String(), r.Header.Type, r.Header.TTL, r.Header.Class&cacheFlush != 0}
	}
	return out
}

func sameSet(got, want []rr) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			return false
		}
	}
	return true
}

// announced is what announcing testService sends, with ttl in place of every TTL if it's set.
func announced(ttl int64) []rr {
	want := []rr{
		{"_matterc._udp.local.", dnsmessage.TypePTR, otherTTL, false},
		{servicesName, dnsmessage.TypePTR, otherTTL, false},
		{"_L3840._sub._matterc._udp.local.", dnsmessage.TypePTR, otherTTL, false},
		{"_S15._sub._matterc._udp.local.", dnsmessage.TypePTR, otherTTL, false},
		{"_CM._sub._matterc._udp.local.", dnsmessage.TypePTR, otherTTL, false},
		{testInstance, dnsmessage.TypeSRV, hostTTL, true},
		{testInstance, dnsmessage.TypeTXT, otherTTL, true},
		{testHost, dnsmessage.TypeA, hostTTL, true},
		{testHost, dnsmessage.TypeAAAA, hostTTL, true},
	}
	if ttl >= 0 {
		for i := range want {
			want[i].ttl = uint32(ttl)
		}
	}
	return want
}

func TestAddProbesThenAnnounces(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	if err := r.Add(context.Background(), testService()); err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		p := c.next()
		if p.dst != nil || p.ifIndex != testIf || p.msg.Response {
			t.Fatalf("probe %d: not a multicast query: %+v", i, p)
		}
		qs := make([]string, 0, len(p.msg.Questions))
		for _, q := range p.msg.Questions {
			if q.Type != dnsmessage.TypeALL || q.Class != dnsmessage.ClassINET|unicastResponse {
				t.Errorf("probe %d asks %v %v, want ANY with the QU bit", i, q.Type, q.Class)
			}
			qs = append(qs, q.Name.String())
		}
		if !slices.Equal(qs, []string{testInstance, testHost}) {
			t.Errorf("probe %d asks for %v", i, qs)
		}
		want := []rr{
			{testInstance, dnsmessage.TypeSRV, hostTTL, false},
			{testInstance, dnsmessage.TypeTXT, otherTTL, false},
			{testHost, dnsmessage.TypeA, hostTTL, false},
			{testHost, dnsmessage.TypeAAAA, hostTTL, false},
		}
		if got := summarise(p.msg.Authorities); !sameSet(got, want) {
			t.Errorf("probe %d proposes %v, want %v", i, got, want)
		}
	}
	for i := range 2 {
		p := c.next()
		if p.dst != nil || !p.msg.Response || !p.msg.Authoritative {
			t.Fatalf("announcement %d: not a multicast response: %+v", i, p)
		}
		if got := summarise(p.msg.Answers); !sameSet(got, announced(-1)) {
			t.Errorf("announcement %d: %v, want %v", i, got, announced(-1))
		}
	}
	c.none()

	// A second service doesn't probe the host name again.
	svc := testService()
	svc.Instance = "0123456789ABCDEF"
	if err := r.Add(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if p := c.next(); len(p.msg.Questions) != 1 {
		t.Errorf("second probe asks %d questions, want only the instance", len(p.msg.Questions))
	}
	if err := r.Add(context.Background(), svc); err == nil {
		t.Error("added the same service twice")
	}
}

func TestAddConflict(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()

	errc := make(chan error, 1)
	go func() { errc <- r.Add(context.Background(), testService()) }()
	c.next() // the first probe

	// Our own records, as multicast loopback brings them back, aren't conflicts.
	ours := mustRecords(t, r.host, testService())
	self := &net.UDPAddr{IP: testAddr4.AsSlice(), Port: Port}
	r.handle(c, testIf, self, pack(t, dnsmessage.Message{
		Header:  dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{ours[len(ours)-1].resource(otherTTL, true)},
	}))
	select {
	case err := <-errc:
		t.Fatalf("Add returned %v before a conflict", err)
	case <-time.After(5 * time.Millisecond):
	}
	other := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: mustName(testInstance), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: 120},
			Body:   &dnsmessage.SRVResource{Port: 1234, Target: mustName("other.local.")},
		}},
	}
	// Another responder, even one on this host, that holds other data for the name is.
	r.handle(c, testIf, self, pack(t, other))
	if err := <-errc; !errors.Is(err, ErrConflict) {
		t.Fatalf("Add returned %v, want ErrConflict", err)
	}
	for len(c.sent) > 0 {
		<-c.sent
	}
	// The service is gone, so it isn't announced or answered for.
	r.handle(c, testIf, peer, query(t, "_matterc._udp.local.", dnsmessage.TypePTR, false))
	c.none()
}

func mustRecords(t testing.TB, host dnsmessage.Name, svc Service) []record {
	recs, err := svc.records(host)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestUpdateRemoveClose(t *testing.T) {
	r, c := newTestResponder(t)
	addReady(t, r, testService())

	// The commissioning window closes: _CM goes, and CM says 0.
	closed := testService()
	closed.Subtypes = []string{"_L3840", "_S15"}
	closed.Text = []string{"D=3840", "CM=0"}
	if err := r.Update(closed); err != nil {
		t.Fatal(err)
	}
	bye := c.next()
	want := []rr{
		{"_CM._sub._matterc._udp.local.", dnsmessage.TypePTR, 0, false},
		{testInstance, dnsmessage.TypeTXT, 0, true},
	}
	if got := summarise(bye.msg.Answers); !sameSet(got, want) {
		t.Errorf("goodbye on update: %v, want %v", got, want)
	}
	for range 2 {
		ann := c.next()
		var txt []string
		for _, a := range ann.msg.Answers {
			if a.Header.Type == dnsmessage.TypeTXT {
				txt = a.Body.(*dnsmessage.TXTResource).TXT
			}
			if strings.HasPrefix(a.Header.Name.String(), "_CM.") {
				t.Errorf("announced %v after the window closed", a.Header.Name)
			}
		}
		if !slices.Equal(txt, closed.Text) {
			t.Errorf("announced TXT %q, want %q", txt, closed.Text)
		}
	}

	// A second service of the same type keeps the services listing alive when the first goes.
	second := testService()
	second.Instance = "0123456789ABCDEF"
	addReady(t, r, second)
	if err := r.Remove(closed); err != nil {
		t.Fatal(err)
	}
	bye = c.next()
	for _, a := range bye.msg.Answers {
		if a.Header.TTL != 0 {
			t.Errorf("goodbye has TTL %d", a.Header.TTL)
		}
		if a.Header.Name.String() == servicesName {
			t.Error("said goodbye to the services listing while a service of the type is left")
		}
	}
	if len(bye.msg.Answers) != 5 { // the type PTR, two subtype PTRs, SRV and TXT
		t.Errorf("goodbye has %d records, want 5: %v", len(bye.msg.Answers), summarise(bye.msg.Answers))
	}
	if err := r.Remove(closed); err == nil {
		t.Error("removed a service twice")
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	svcBye, hostBye := c.next(), c.next()
	want = announced(0)
	for i := range want {
		want[i].name = strings.Replace(want[i].name, "B7322C948581262F", second.Instance, 1)
	}
	if got := summarise(slices.Concat(svcBye.msg.Answers, hostBye.msg.Answers)); !sameSet(got, want) {
		t.Errorf("goodbye on Close: %v, want %v", got, want)
	}
	if !errors.Is(r.Add(context.Background(), testService()), ErrClosed) {
		t.Error("Add after Close didn't return ErrClosed")
	}
}

func query(t testing.TB, name string, typ dnsmessage.Type, qu bool) []byte {
	class := dnsmessage.ClassINET
	if qu {
		class |= unicastResponse
	}
	return pack(t, dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 7},
		Questions: []dnsmessage.Question{{Name: mustName(name), Type: typ, Class: class}},
	})
}

func TestAnswer(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	addReady(t, r, testService())
	addresses := []rr{
		{testHost, dnsmessage.TypeA, hostTTL, true},
		{testHost, dnsmessage.TypeAAAA, hostTTL, true},
	}
	instance := append([]rr{
		{testInstance, dnsmessage.TypeSRV, hostTTL, true},
		{testInstance, dnsmessage.TypeTXT, otherTTL, true},
	}, addresses...)

	for _, tc := range []struct {
		name            string
		qname           string
		typ             dnsmessage.Type
		answers, extras []rr
	}{
		{"service type", "_matterc._udp.local.", dnsmessage.TypePTR,
			[]rr{{"_matterc._udp.local.", dnsmessage.TypePTR, otherTTL, false}}, instance},
		{"long discriminator", "_L3840._sub._matterc._udp.local.", dnsmessage.TypePTR,
			[]rr{{"_L3840._sub._matterc._udp.local.", dnsmessage.TypePTR, otherTTL, false}}, instance},
		{"commissioning mode, in another case", "_cm._sub._MATTERC._udp.local.", dnsmessage.TypePTR,
			[]rr{{"_CM._sub._matterc._udp.local.", dnsmessage.TypePTR, otherTTL, false}}, instance},
		{"services listing", servicesName, dnsmessage.TypePTR,
			[]rr{{servicesName, dnsmessage.TypePTR, otherTTL, false}}, nil},
		{"SRV", testInstance, dnsmessage.TypeSRV,
			[]rr{{testInstance, dnsmessage.TypeSRV, hostTTL, true}}, addresses},
		{"TXT", testInstance, dnsmessage.TypeTXT,
			[]rr{{testInstance, dnsmessage.TypeTXT, otherTTL, true}}, nil},
		{"ANY", testInstance, dnsmessage.TypeALL, instance[:2], addresses},
		{"A", testHost, dnsmessage.TypeA, addresses[:1], nil},
		{"AAAA", testHost, dnsmessage.TypeAAAA, addresses[1:], nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.handle(c, testIf, peer, query(t, tc.qname, tc.typ, false))
			p := c.next()
			if p.dst != nil || p.ifIndex != testIf {
				t.Errorf("answered to %v on %d, want multicast on %d", p.dst, p.ifIndex, testIf)
			}
			if got := summarise(p.msg.Answers); !sameSet(got, tc.answers) {
				t.Errorf("answers %v, want %v", got, tc.answers)
			}
			if got := summarise(p.msg.Additionals); !sameSet(got, tc.extras) {
				t.Errorf("additionals %v, want %v", got, tc.extras)
			}
			c.none()
		})
	}

	t.Run("unknown name", func(t *testing.T) {
		r.handle(c, testIf, peer, query(t, "_ipp._tcp.local.", dnsmessage.TypePTR, false))
		c.none()
	})

	t.Run("QU bit", func(t *testing.T) {
		r.handle(c, testIf, peer, query(t, testHost, dnsmessage.TypeA, true))
		if p := c.next(); p.dst != peer || p.msg.ID != 0 || len(p.msg.Answers) != 1 {
			t.Errorf("got %+v, want a unicast mDNS reply to %v", p, peer)
		}
	})

	t.Run("legacy unicast", func(t *testing.T) {
		legacy := &net.UDPAddr{IP: peer.IP, Port: 49152}
		r.handle(c, testIf, legacy, query(t, "_matterc._udp.local.", dnsmessage.TypePTR, false))
		p := c.next()
		if p.dst != legacy || p.msg.ID != 7 || len(p.msg.Questions) != 1 {
			t.Fatalf("got %+v, want a DNS reply to %v with the query's ID and question", p, legacy)
		}
		for _, a := range slices.Concat(p.msg.Answers, p.msg.Additionals) {
			if a.Header.TTL > legacyTTL || a.Header.Class != dnsmessage.ClassINET {
				t.Errorf("%v has TTL %d and class %v", a.Header.Name, a.Header.TTL, a.Header.Class)
			}
		}
	})

	t.Run("known answer", func(t *testing.T) {
		recs := mustRecords(t, r.host, testService())
		m := dnsmessage.Message{
			Questions: []dnsmessage.Question{{Name: mustName("_matterc._udp.local."), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}},
			Answers:   []dnsmessage.Resource{recs[0].resource(otherTTL/2, false)},
		}
		r.handle(c, testIf, peer, pack(t, m))
		c.none()
		m.Answers[0].Header.TTL = otherTTL/2 - 1 // less than half left: answer it
		r.handle(c, testIf, peer, pack(t, m))
		c.next()
	})
}

func TestNotAnsweredWhileProbing(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	r.timing.probeInterval = time.Hour
	go func() { _ = r.Add(context.Background(), testService()) }()
	c.next() // the first probe
	r.handle(c, testIf, peer, query(t, testInstance, dnsmessage.TypeALL, false))
	c.none()
	if err := r.Update(testService()); err == nil {
		t.Error("updated a service that is still being probed")
	}
}

func TestServiceValidation(t *testing.T) {
	for _, svc := range []Service{
		{Instance: "", Type: "_x._tcp", Port: 1},
		{Instance: "a.b", Type: "_x._tcp", Port: 1},
		{Instance: "a", Type: "x._tcp", Port: 1},
		{Instance: "a", Type: "_x._sctp", Port: 1},
		{Instance: "a", Type: "_x._tcp", Port: 0},
		{Instance: "a", Type: "_x._tcp", Port: 1, Subtypes: []string{"_a.b"}},
		{Instance: "a", Type: "_x._tcp", Port: 1, Text: []string{strings.Repeat("x", 256)}},
	} {
		if _, err := svc.records(mustName(testHost)); err == nil {
			t.Errorf("%+v is valid", svc)
		}
	}
}

// TestLateAnnouncement checks that the second announcement of a service sends the records it has
// then, not those it had when it was added, so that it can't undo a goodbye.
func TestLateAnnouncement(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	if err := r.Add(context.Background(), testService()); err != nil {
		t.Fatal(err)
	}
	for range 4 { // three probes and the first announcement
		c.next()
	}
	closed := testService()
	closed.Subtypes = []string{"_L3840", "_S15"}
	if err := r.Update(closed); err != nil {
		t.Fatal(err)
	}
	c.next() // the goodbye to _CM
	for range 3 {
		for _, a := range c.next().msg.Answers {
			if strings.HasPrefix(a.Header.Name.String(), "_CM.") {
				t.Fatalf("announced %v after its goodbye", a.Header.Name)
			}
		}
	}
	if err := r.Remove(closed); err != nil {
		t.Fatal(err)
	}
	c.next() // the goodbye
	c.none()
}

// TestNothingAfterClose checks that a multicast answer waiting out its delay isn't sent once Close
// has said goodbye.
func TestNothingAfterClose(t *testing.T) {
	r, c := newTestResponder(t)
	r.timing.minDelay, r.timing.maxDelay = 50*time.Millisecond, 50*time.Millisecond
	addReady(t, r, testService())
	r.handle(c, testIf, peer, query(t, "_matterc._udp.local.", dnsmessage.TypePTR, false))
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	c.next() // the services' goodbye
	c.next() // the host's
	c.none()
}

// TestRemoveWhileAnotherProbes checks that the services listing gets its goodbye when the only
// other service of the type hasn't been announced yet.
func TestRemoveWhileAnotherProbes(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	addReady(t, r, testService())
	probing := testService()
	probing.Instance = "0123456789ABCDEF"
	recs := mustRecords(t, r.host, probing)
	name, _ := probing.instanceName()
	r.mu.Lock()
	r.services[strings.ToLower(name.String())] = &entry{svc: probing, name: name, records: recs}
	r.mu.Unlock()
	if err := r.Remove(testService()); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.next().msg.Answers, func(a dnsmessage.Resource) bool { return a.Header.Name.String() == servicesName }) {
		t.Error("no goodbye to the services listing")
	}
}
