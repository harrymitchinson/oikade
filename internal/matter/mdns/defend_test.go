package mdns

import (
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// otherSRV is another host's response claiming testService's instance, with ttl.
func otherSRV(t *testing.T, ttl uint32) []byte {
	t.Helper()
	return pack(t, dnsmessage.Message{
		Response: true, Authoritative: true,
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: mustName(testInstance), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET | cacheFlush, TTL: ttl},
			Body:   &dnsmessage.SRVResource{Port: 1234, Target: mustName("other.local.")},
		}},
	})
}

// newDefendingResponder returns a test Responder that has announced testService, and the services
// it reports lost.
func newDefendingResponder(t *testing.T) (*Responder, *fakeConn, chan Service) {
	t.Helper()
	r, c := newTestResponder(t)
	t.Cleanup(func() { _ = r.Close() })
	r.timing.tieWait = 20 * time.Millisecond
	lost := make(chan Service, 1)
	r.lostFn = func(svc Service) { lost <- svc }
	addReady(t, r, testService())
	return r, c, lost
}

// probes reads n probes for name from c, failing t on anything else.
func probes(t *testing.T, c *fakeConn, name string, n int) {
	t.Helper()
	for i := range n {
		p := c.next(t)
		if p.msg.Response || len(p.msg.Questions) != 1 || p.msg.Questions[0].Name.String() != name {
			t.Fatalf("sent %+v, want probe %d for %s", p.msg, i+1, name)
		}
	}
}

func TestANameThatsKeptIsAnnouncedAgain(t *testing.T) {
	r, c, lost := newDefendingResponder(t)

	r.handle(c, testIf, peer, otherSRV(t, hostTTL))

	probes(t, c, testInstance, 3)
	for range 2 {
		if p := c.next(t); !p.msg.Response {
			t.Fatalf("sent %+v, want an announcement", p.msg)
		}
	}
	select {
	case svc := <-lost:
		t.Fatalf("reported %s lost, though nobody answered", svc.Instance)
	default:
	}
	r.handle(c, testIf, peer, query(t, testInstance, dnsmessage.TypeSRV, false))
	if p := c.next(t); len(p.msg.Answers) != 1 {
		t.Errorf("answers %v after keeping the name", summarise(p.msg.Answers))
	}
}

func TestANameThatsLostIsReportedAndNotAnswered(t *testing.T) {
	r, c, lost := newDefendingResponder(t)

	r.handle(c, testIf, peer, otherSRV(t, hostTTL))
	probes(t, c, testInstance, 1)
	// While we probe again, we don't answer for the name.
	r.handle(c, testIf, peer, query(t, testInstance, dnsmessage.TypeSRV, false))
	// The other host has the name, and answers our probe.
	r.handle(c, testIf, peer, otherSRV(t, hostTTL))

	select {
	case svc := <-lost:
		if svc.Instance != testService().Instance {
			t.Errorf("reported %s lost", svc.Instance)
		}
	case <-time.After(time.Second):
		t.Fatal("didn't report the name lost")
	}
	for len(c.sent) > 0 {
		if p := <-c.sent; p.msg.Response {
			t.Errorf("sent %v, though it lost the name", summarise(p.msg.Answers))
		}
	}
	r.handle(c, testIf, peer, query(t, testInstance, dnsmessage.TypeSRV, false))
	c.none(t)
	if err := r.Remove(testService()); err == nil {
		t.Error("removed a service it had given up")
	}
}

func TestNoConflictFromOurOwnRecordsOrAGoodbye(t *testing.T) {
	r, c, _ := newDefendingResponder(t)
	ours := r.answerable(testIf)
	m := dnsmessage.Message{Response: true}
	for _, rec := range ours {
		m.Answers = append(m.Answers, rec.resource(rec.ttl, true))
	}
	r.handle(c, testIf, peer, pack(t, m)) // our own announcement, looped back
	r.handle(c, testIf, peer, otherSRV(t, 0))
	c.none(t)
}

func TestRemovedWhileProbedAgain(t *testing.T) {
	r, c, lost := newDefendingResponder(t)
	r.handle(c, testIf, peer, otherSRV(t, hostTTL))
	probes(t, c, testInstance, 1)

	if err := r.Remove(testService()); err != nil {
		t.Fatalf("Remove while probing the name again: %v", err)
	}
	bye := c.next(t)
	if !bye.msg.Response || bye.msg.Answers[0].Header.TTL != 0 {
		t.Fatalf("sent %+v, want a goodbye", bye.msg)
	}
	// The probe may send its last two, but nothing is announced, and nothing is lost.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case p := <-c.sent:
			if p.msg.Response {
				t.Fatalf("announced %v after Remove", summarise(p.msg.Answers))
			}
		case svc := <-lost:
			t.Fatalf("reported %s lost after Remove", svc.Instance)
		case <-deadline:
			return
		}
	}
}

func TestAConflictOnTheHostNameIsProbedAndAnnounced(t *testing.T) {
	r, c, _ := newDefendingResponder(t)
	r.handle(c, testIf, peer, pack(t, dnsmessage.Message{
		Response: true,
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: r.host, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET | cacheFlush, TTL: hostTTL},
			Body:   &dnsmessage.AResource{A: [4]byte{198, 51, 100, 9}},
		}},
	}))
	probes(t, c, testHost, 3)
	p := c.next(t)
	if got := addrsIn(p.msg.Answers); len(got) != 2 {
		t.Errorf("announced addresses %v, want the host's two", got)
	}
}

func TestConflictsThatComeTooFastSlowProbing(t *testing.T) {
	r, c := newTestResponder(t)
	defer func() { _ = r.Close() }()
	r.timing.slowProbe, r.timing.conflictWindow = 150*time.Millisecond, time.Minute
	for range maxConflicts - 1 {
		r.noteConflict()
	}
	if r.slowConflicts() {
		t.Fatal("slow after fourteen conflicts")
	}
	r.noteConflict()
	if !r.slowConflicts() {
		t.Fatal("not slow after fifteen conflicts")
	}
	start := time.Now()
	go func() { _ = r.Add(t.Context(), testService()) }()
	c.next(t)
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Errorf("first probe after %v, want at least the slow wait", waited)
	}
}

func TestOldConflictsDontCount(t *testing.T) {
	r, _ := newTestResponder(t)
	defer func() { _ = r.Close() }()
	r.timing.conflictWindow = 10 * time.Millisecond
	for range maxConflicts {
		r.noteConflict()
	}
	time.Sleep(20 * time.Millisecond) // the window is real time
	if r.slowConflicts() {
		t.Error("counts conflicts from before the window")
	}
}

func TestTheHostIsntAnsweredWhileProbedAgain(t *testing.T) {
	r, c, _ := newDefendingResponder(t)
	r.mu.Lock()
	r.hostDefending = true
	r.mu.Unlock()
	r.handle(c, testIf, peer, query(t, testHost, dnsmessage.TypeA, false))
	c.none(t)
	r.handle(c, testIf, peer, query(t, testInstance, dnsmessage.TypeSRV, false))
	p := c.next(t)
	if got := addrsIn(slices.Concat(p.msg.Answers, p.msg.Additionals)); len(got) != 0 {
		t.Errorf("sent addresses %v while the host name is probed again", got)
	}
}

func TestOurOwnLatePacketAfterAnUpdateIsntAConflict(t *testing.T) {
	r, c, _ := newDefendingResponder(t)
	before := r.answerable(testIf)
	changed := testService()
	changed.Text = []string{"D=3840", "CM=0"}
	if err := r.Update(changed); err != nil {
		t.Fatal(err)
	}
	c.next(t) // the goodbye
	c.next(t) // the first announcement
	// A reply built before the Update, with the old TXT, comes back after it.
	m := dnsmessage.Message{Response: true}
	for _, rec := range before {
		m.Answers = append(m.Answers, rec.resource(rec.ttl, true))
	}
	r.handle(c, testIf, peer, pack(t, m))
	p := c.next(t) // the second announcement, and no probe
	if !p.msg.Response {
		t.Fatalf("sent %+v: took our own old TXT for a conflict", p.msg)
	}
	c.none(t)
}

func TestAConflictCountsOnce(t *testing.T) {
	r, c, lost := newDefendingResponder(t)
	r.timing.conflictWindow = time.Minute
	for range 4 { // the other host announces twice, on two interfaces
		r.handle(c, testIf, peer, otherSRV(t, hostTTL))
	}
	select { // what came after the first claimed it while we probed it again
	case <-lost:
	case <-time.After(time.Second):
		t.Fatal("didn't lose the name")
	}
	r.mu.Lock()
	n := len(r.conflicts)
	r.mu.Unlock()
	if n != 1 {
		t.Errorf("counted %d conflicts, want 1", n)
	}
}

func TestAnUpdateWhileProbedAgainIsWhatItProposes(t *testing.T) {
	r, c, _ := newDefendingResponder(t)
	r.handle(c, testIf, peer, otherSRV(t, hostTTL))
	probes(t, c, testInstance, 1)
	changed := testService()
	changed.Text = []string{"D=3840", "CM=0"}
	if err := r.Update(changed); err != nil {
		t.Fatal(err)
	}
	for {
		p := c.next(t)
		if p.msg.Response {
			continue // the Update's goodbye
		}
		for _, a := range p.msg.Authorities {
			if txt, ok := a.Body.(*dnsmessage.TXTResource); ok && !slices.Equal(txt.TXT, changed.Text) {
				t.Errorf("probe proposes TXT %q, want the updated %q", txt.TXT, changed.Text)
			}
		}
		return
	}
}
