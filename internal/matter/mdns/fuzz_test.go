package mdns

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/harrymitchinson/oikade/internal/logging"
)

// FuzzHandle checks that no packet, as a query or a response and from an mDNS or a legacy port,
// makes the handler panic.
func FuzzHandle(f *testing.F) {
	seed := func(m dnsmessage.Message) {
		f.Add(pack(f, m), true)
		f.Add(pack(f, m), false)
	}
	recs := mustRecords(f, mustName(testHost), testService())
	seed(dnsmessage.Message{Questions: []dnsmessage.Question{
		{Name: mustName("_matterc._udp.local."), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET},
		{Name: mustName(testHost), Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET | unicastResponse},
	}})
	seed(dnsmessage.Message{
		Questions: []dnsmessage.Question{{Name: mustName(testInstance), Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET}},
		Answers:   []dnsmessage.Resource{recs[0].resource(otherTTL, false)},
	})
	resp := dnsmessage.Message{Response: true}
	for _, rec := range recs {
		resp.Answers = append(resp.Answers, rec.resource(rec.ttl, true))
	}
	seed(resp)
	f.Add([]byte{}, true)
	f.Add([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 0x0c}, true) // a name that points at itself

	f.Fuzz(func(t *testing.T, b []byte, mdnsPort bool) {
		r, c := newTestResponder(t)
		r.log = logging.Discard()
		addReady(t, r, testService())
		// A probe in progress, so that responses are checked for conflicts.
		r.probes[&probe{names: []dnsmessage.Name{mustName("0123456789ABCDEF._matterc._udp.local.")}, conflict: make(chan dnsmessage.Name, 1)}] = struct{}{}
		src := &net.UDPAddr{IP: peer.IP, Port: 49152}
		if mdnsPort {
			src.Port = Port
		}
		r.handle(c, testIf, src, b)
		_ = r.Close()
	})
}
