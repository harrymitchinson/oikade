package mdns

import (
	"errors"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// serve reads packets from c until it's closed.
func (r *Responder) serve(c packetConn) {
	b := make([]byte, maxPacket)
	for {
		n, ifIndex, src, err := c.ReadPacket(b)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.log.V(1).Info("Could not read mDNS packet", "error", err.Error())
			continue
		}
		if !slices.Contains(r.ifaces, ifIndex) {
			continue // another socket joined the group on an interface we don't serve
		}
		r.handle(c, ifIndex, src, b[:n])
	}
}

// handle answers a query, or checks a response for conflicts with names being probed. It
// ignores what it can't parse.
func (r *Responder) handle(c packetConn, ifIndex int, src *net.UDPAddr, b []byte) {
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		r.log.V(2).Info("Ignored malformed mDNS packet", "source", src.String(), "error", err.Error())
		return
	}
	// Messages with another opcode or rcode are ignored (RFC 6762, 18.3 and 18.11).
	if m.OpCode != 0 || m.RCode != dnsmessage.RCodeSuccess {
		return
	}
	if m.Response {
		// Responses from a port other than 5353 aren't mDNS (6).
		if src.Port == Port {
			r.checkConflicts(src, &m)
		}
		return
	}
	r.answer(c, ifIndex, src, &m)
}

// answer replies to the query m, if we hold any of the records it asks for.
func (r *Responder) answer(c packetConn, ifIndex int, src *net.UDPAddr, m *dnsmessage.Message) {
	recs := r.answerable(ifIndex)
	if len(recs) == 0 {
		return
	}
	var answers []record
	unicast := true // every question that we answer asks for a unicast reply
	for _, q := range m.Questions {
		class := q.Class &^ unicastResponse
		if class != dnsmessage.ClassINET && class != dnsmessage.ClassANY {
			continue
		}
		matched := false
		for _, rec := range recs {
			if (q.Type != dnsmessage.TypeALL && q.Type != rec.typ) || !nameEqual(q.Name, rec.name) {
				continue
			}
			if knownAnswer(m.Answers, rec) {
				continue
			}
			answers = appendNew(answers, rec)
			matched = true
		}
		if matched && q.Class&unicastResponse == 0 {
			unicast = false
		}
	}
	if len(answers) == 0 {
		return
	}
	additionals := without(additional(answers, recs), answers)

	// A query from a port other than 5353 is a legacy unicast query, from a resolver that
	// isn't an mDNS querier: it gets a conventional DNS reply (RFC 6762, 6.7).
	if src.Port != Port {
		b, err := legacyReply(m, answers, additionals)
		if err != nil {
			r.log.Error(err, "Failed to build mDNS reply")
			return
		}
		r.write(c.WriteUnicast(b, src), src)
		return
	}
	// A multicast answer that holds shared records waits 20 to 120 ms, so that the answers
	// of the hosts that share them don't collide (6).
	if unicast || !slices.ContainsFunc(answers, func(rec record) bool { return !rec.unique }) {
		b, err := reply(answers, additionals)
		if err != nil {
			r.log.Error(err, "Failed to build mDNS reply")
			return
		}
		if unicast {
			r.write(c.WriteUnicast(b, src), src)
		} else {
			r.write(c.WriteMulticast(b, ifIndex), src)
		}
		return
	}
	spread := int64(r.timing.maxDelay - r.timing.minDelay)
	delay := r.timing.minDelay + time.Duration(mrand.Int64N(spread+1))
	r.after(delay, func() {
		r.sendMu.Lock()
		defer r.sendMu.Unlock()
		// Leave out what was withdrawn in the meantime.
		now := r.answerable(ifIndex)
		still := func(rec record) bool { return slices.ContainsFunc(now, rec.equal) }
		answers = slices.DeleteFunc(answers, func(rec record) bool { return !still(rec) })
		additionals = slices.DeleteFunc(additionals, func(rec record) bool { return !still(rec) })
		if len(answers) == 0 {
			return
		}
		b, err := reply(answers, additionals)
		if err != nil {
			r.log.Error(err, "Failed to build mDNS reply")
			return
		}
		r.write(c.WriteMulticast(b, ifIndex), src)
	})
}

func (r *Responder) write(err error, to *net.UDPAddr) {
	if err != nil && !errors.Is(err, net.ErrClosed) {
		r.log.V(1).Info("Could not send mDNS reply", "querier", to.String(), "error", err.Error())
	}
}

// answerable returns the records the Responder answers with on an interface: those of its
// announced services, and the host's addresses there once the host name has been probed.
func (r *Responder) answerable(ifIndex int) []record {
	r.mu.Lock()
	var recs []record
	for _, e := range r.services {
		if e.ready {
			recs = append(recs, e.records...)
		}
	}
	hostReady := r.hostReady
	r.mu.Unlock()
	if hostReady {
		recs = append(recs, addressRecords(r.host, r.addrs(ifIndex))...)
	}
	return recs
}

// additional returns the records that a querier will want next (RFC 6763, 12): the SRV and TXT
// of each service instance that a PTR answer points at, and the addresses of each SRV's target.
func additional(answers, recs []record) []record {
	var out []record
	for _, a := range answers {
		if ptr, ok := a.body.(*dnsmessage.PTRResource); ok {
			for _, rec := range recs {
				if (rec.typ == dnsmessage.TypeSRV || rec.typ == dnsmessage.TypeTXT) && nameEqual(rec.name, ptr.PTR) {
					out = appendNew(out, rec)
				}
			}
		}
	}
	for _, a := range slices.Concat(answers, out) {
		if srv, ok := a.body.(*dnsmessage.SRVResource); ok {
			for _, rec := range recs {
				if (rec.typ == dnsmessage.TypeA || rec.typ == dnsmessage.TypeAAAA) && nameEqual(rec.name, srv.Target) {
					out = appendNew(out, rec)
				}
			}
		}
	}
	return out
}

// knownAnswer reports whether the querier listed rec among the answers it already holds, with at
// least half its TTL left, so that it needn't be sent again (RFC 6762, 7.1).
func knownAnswer(known []dnsmessage.Resource, rec record) bool {
	for _, k := range known {
		if k.Header.TTL >= rec.ttl/2 && rec.same(k.Header.Name, k.Header.Type, k.Body) {
			return true
		}
	}
	return false
}

// reply packs an mDNS response.
func reply(answers, additionals []record) ([]byte, error) {
	m := dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}}
	for _, rec := range answers {
		m.Answers = append(m.Answers, rec.resource(rec.ttl, true))
	}
	for _, rec := range additionals {
		m.Additionals = append(m.Additionals, rec.resource(rec.ttl, true))
	}
	return m.Pack()
}

// legacyReply packs a reply to a legacy unicast query: its ID and questions, short TTLs and no
// cache-flush bits (RFC 6762, 6.7).
func legacyReply(q *dnsmessage.Message, answers, additionals []record) ([]byte, error) {
	m := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: q.ID, Response: true, Authoritative: true},
		Questions: q.Questions,
	}
	for i := range m.Questions {
		m.Questions[i].Class &^= unicastResponse
	}
	for _, rec := range answers {
		m.Answers = append(m.Answers, rec.resource(min(rec.ttl, legacyTTL), false))
	}
	for _, rec := range additionals {
		m.Additionals = append(m.Additionals, rec.resource(min(rec.ttl, legacyTTL), false))
	}
	return m.Pack()
}

// checkConflicts reports to each probe a record in the response m that has a name it's probing
// but isn't one of ours (RFC 6762, 9). Our own packets, which multicast loopback and other
// interfaces on the same link bring back, aren't conflicts.
func (r *Responder) checkConflicts(src *net.UDPAddr, m *dnsmessage.Message) {
	r.mu.Lock()
	probes := make([]*probe, 0, len(r.probes))
	for p := range r.probes {
		probes = append(probes, p)
	}
	var ours []record
	for _, e := range r.services {
		ours = append(ours, e.records...)
	}
	r.mu.Unlock()
	if len(probes) == 0 || r.isOwn(src) {
		return
	}
	for _, ifIndex := range r.ifaces {
		ours = append(ours, addressRecords(r.host, r.addrs(ifIndex))...)
	}
	for _, rr := range slices.Concat(m.Answers, m.Authorities, m.Additionals) {
		if slices.ContainsFunc(ours, func(rec record) bool { return rec.same(rr.Header.Name, rr.Header.Type, rr.Body) }) {
			continue
		}
		for _, p := range probes {
			for _, n := range p.names {
				if nameEqual(n, rr.Header.Name) {
					select {
					case p.conflict <- n:
					default: // one is enough
					}
				}
			}
		}
	}
}

// isOwn reports whether src is one of the host's addresses.
func (r *Responder) isOwn(src *net.UDPAddr) bool {
	ip, ok := netip.AddrFromSlice(src.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	for _, ifIndex := range r.ifaces {
		if slices.Contains(r.addrs(ifIndex), ip) {
			return true
		}
	}
	return false
}
