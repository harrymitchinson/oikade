package mdns

import (
	"errors"
	mrand "math/rand/v2"
	"net"
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
			r.log.Debug("Could not read mDNS packet", "err", err)
			select { // don't spin on an error that persists
			case <-r.done:
				return
			case <-time.After(100 * time.Millisecond):
			}
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
		r.log.Debug("Ignored malformed mDNS packet", "source", src.String(), "err", err)
		return
	}
	// Messages with another opcode or rcode are ignored (RFC 6762, 18.3 and 18.11).
	if m.OpCode != 0 || m.RCode != dnsmessage.RCodeSuccess {
		return
	}
	if m.Response {
		// Responses from a port other than 5353 aren't mDNS (6).
		if src.Port == Port {
			r.checkConflicts(&m)
		}
		return
	}
	r.answer(c, ifIndex, src, &m)
}

// answer replies to the query m, if we hold any of the records it asks for.
func (r *Responder) answer(c packetConn, ifIndex int, src *net.UDPAddr, m *dnsmessage.Message) {
	if !r.asksForUs(m.Questions) {
		return // most queries on a link are for other hosts: don't look up our addresses for them
	}
	// Hold sendMu from reading the records to sending them, so that the reply can't follow
	// a goodbye to one of them.
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
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
			r.log.Error("Failed to build mDNS reply", "err", err)
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
			r.log.Error("Failed to build mDNS reply", "err", err)
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
			r.log.Error("Failed to build mDNS reply", "err", err)
			return
		}
		r.write(c.WriteMulticast(b, ifIndex), src)
	})
}

func (r *Responder) write(err error, to *net.UDPAddr) {
	if err != nil && !errors.Is(err, net.ErrClosed) {
		r.log.Debug("Could not send mDNS reply", "querier", to.String(), "err", err)
	}
}

// asksForUs reports whether a question names one of our records or the host, so that answering it
// is worth looking up the host's addresses.
func (r *Responder) asksForUs(qs []dnsmessage.Question) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range qs {
		if r.hostReady && nameEqual(q.Name, r.host) {
			return true
		}
		for _, e := range r.services {
			if e.ready && slices.ContainsFunc(e.records, func(rec record) bool { return nameEqual(q.Name, rec.name) }) {
				return true
			}
		}
	}
	return false
}

// answerable returns the records the Responder answers with on an interface: those of its
// announced services, and the host's addresses there once the host name has been probed. It
// returns none once the Responder is closing.
func (r *Responder) answerable(ifIndex int) []record {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
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
// interfaces on the same link bring back, hold only records identical to ours, so they aren't
// conflicts; another responder on this host that claims one of our names with other data is.
func (r *Responder) checkConflicts(m *dnsmessage.Message) {
	r.mu.Lock()
	if len(r.probes) == 0 {
		r.mu.Unlock()
		return
	}
	probes := make([]*probe, 0, len(r.probes))
	for p := range r.probes {
		probes = append(probes, p)
	}
	var ours []record
	for _, e := range r.services {
		ours = append(ours, e.records...)
	}
	r.mu.Unlock()
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
