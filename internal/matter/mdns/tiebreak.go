package mdns

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"slices"

	"golang.org/x/net/dns/dnsmessage"
)

// Two hosts that probe the same name at once each see the other's probe, a query with the records
// it proposes in its authority section. Neither has the name yet, so neither answers, and RFC 6762,
// 8.2 breaks the tie: the host whose records are lexicographically later wins and goes on, and the
// other waits a second and probes again, when the winner, which has the name by then, answers it
// as a conflict.

// maxTieLosses is how many tie-breaks a probe loses before it gives the name up, so that a host
// that keeps probing with later records can't hold Add up for ever. A winner has the name, and
// answers our next probe, within a second, so a real tie is lost once or twice.
const maxTieLosses = 15

// tieBreak compares the records that a probe from another host, m, proposes for each name we're
// probing with ours, and tells each probe that loses.
func (r *Responder) tieBreak(m *dnsmessage.Message, ifIndex int) {
	probes, _ := r.probing()
	for _, p := range probes {
		for _, n := range p.names {
			var theirs []dnsmessage.Resource
			for _, rr := range m.Authorities {
				if nameEqual(rr.Header.Name, n) {
					theirs = append(theirs, rr)
				}
			}
			if len(theirs) == 0 || r.ourOwn(p, n, theirs) {
				continue
			}
			if compareSets(r.proposed(p, n, ifIndex), theirs) < 0 {
				select {
				case p.lost <- n:
				default: // one is enough
				}
			}
		}
	}
}

// ourOwn reports whether theirs is the set of records that p proposes for n on one of the
// interfaces we serve: our own probe, which multicast loopback, or another interface on the same
// link, brings back. A set that's only part of ours is another host's, which may sort later.
func (r *Responder) ourOwn(p *probe, n dnsmessage.Name, theirs []dnsmessage.Resource) bool {
	if !nameEqual(n, r.host) {
		return compareSets(r.proposed(p, n, 0), theirs) == 0 // the same on every interface
	}
	for _, ifIndex := range r.served() {
		if compareSets(r.proposed(p, n, ifIndex), theirs) == 0 {
			return true
		}
	}
	return false
}

// proposed returns the records that p proposes for name n in a probe sent on an interface: the
// service's unique records, or the host's addresses there.
func (r *Responder) proposed(p *probe, n dnsmessage.Name, ifIndex int) []dnsmessage.Resource {
	recs := p.records
	if nameEqual(n, r.host) {
		recs = addressRecords(r.host, r.addrs(ifIndex))
	}
	var out []dnsmessage.Resource
	for _, rec := range recs {
		if rec.unique && nameEqual(rec.name, n) {
			out = append(out, rec.resource(rec.ttl, false))
		}
	}
	return out
}

// compareSets compares two hosts' proposed records for a name as RFC 6762, 8.2.1 says: each set
// sorted, then record by record, and if one runs out first, the one with records left is later.
func compareSets(ours, theirs []dnsmessage.Resource) int {
	ours, theirs = slices.Clone(ours), slices.Clone(theirs)
	slices.SortFunc(ours, compareRecords)
	slices.SortFunc(theirs, compareRecords)
	for i := range min(len(ours), len(theirs)) {
		if c := compareRecords(ours[i], theirs[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(ours), len(theirs))
}

// compareRecords compares two records by class, without the cache-flush bit, then type, then
// their data as raw bytes, uncompressed (RFC 6762, 8.2).
func compareRecords(a, b dnsmessage.Resource) int {
	if c := cmp.Compare(a.Header.Class&^cacheFlush, b.Header.Class&^cacheFlush); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Header.Type, b.Header.Type); c != 0 {
		return c
	}
	return bytes.Compare(rdata(a.Body), rdata(b.Body))
}

// rdata returns a record's data in wire format, with names uncompressed. It knows the types the
// responder proposes, and any other type's data is nil: a record of another type differs from ours
// by type before its data is looked at.
func rdata(body dnsmessage.ResourceBody) []byte {
	switch b := body.(type) {
	case *dnsmessage.AResource:
		return b.A[:]
	case *dnsmessage.AAAAResource:
		return b.AAAA[:]
	case *dnsmessage.PTRResource:
		return wireName(b.PTR)
	case *dnsmessage.SRVResource:
		out := binary.BigEndian.AppendUint16(nil, b.Priority)
		out = binary.BigEndian.AppendUint16(out, b.Weight)
		out = binary.BigEndian.AppendUint16(out, b.Port)
		return append(out, wireName(b.Target)...)
	case *dnsmessage.TXTResource:
		var out []byte
		for _, s := range b.TXT {
			out = append(out, byte(len(s)))
			out = append(out, s...)
		}
		return out
	case *dnsmessage.UnknownResource:
		return b.Data
	}
	return nil
}
