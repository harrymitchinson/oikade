package mdns

import (
	"net/netip"
	"slices"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// TTLs (RFC 6762, 10): 120 seconds for records that name a host or hold one in their data, such as
// A, AAAA and SRV, and 75 minutes for the rest. A reply to a legacy unicast query carries no more
// than 10 seconds (6.7).
const (
	hostTTL   = 120
	otherTTL  = 4500
	legacyTTL = 10
)

// Bits in the class field of mDNS questions and records.
const (
	// unicastResponse in a question asks for a unicast reply (RFC 6762, 5.4).
	unicastResponse = 1 << 15
	// cacheFlush on a record says it replaces what caches hold for its name and type (10.2).
	cacheFlush = 1 << 15
)

// record is a resource record that the responder owns.
type record struct {
	name dnsmessage.Name
	typ  dnsmessage.Type
	// unique records belong to this host alone and are sent with the cache-flush bit. The others,
	// the PTR records, are shared with other hosts' services.
	unique bool
	ttl    uint32
	body   dnsmessage.ResourceBody
}

// resource returns rec for a message. flush sets the cache-flush bit on unique records.
func (rec record) resource(ttl uint32, flush bool) dnsmessage.Resource {
	class := dnsmessage.ClassINET
	if flush && rec.unique {
		class |= cacheFlush
	}
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: rec.name, Type: rec.typ, Class: class, TTL: ttl},
		Body:   rec.body,
	}
}

// same reports whether rec and the resource r are the same record: name, type and data.
func (rec record) same(name dnsmessage.Name, typ dnsmessage.Type, body dnsmessage.ResourceBody) bool {
	return rec.typ == typ && nameEqual(rec.name, name) && sameBody(rec.body, body)
}

func (rec record) equal(o record) bool { return rec.same(o.name, o.typ, o.body) }

// nameEqual compares names as DNS does, ignoring ASCII case.
func nameEqual(a, b dnsmessage.Name) bool {
	return strings.EqualFold(a.String(), b.String())
}

// sameBody reports whether a and b hold the same data. It only knows the types the responder
// sends, so any other type is never the same as one of ours.
func sameBody(a, b dnsmessage.ResourceBody) bool {
	switch x := a.(type) {
	case *dnsmessage.AResource:
		y, ok := b.(*dnsmessage.AResource)
		return ok && x.A == y.A
	case *dnsmessage.AAAAResource:
		y, ok := b.(*dnsmessage.AAAAResource)
		return ok && x.AAAA == y.AAAA
	case *dnsmessage.PTRResource:
		y, ok := b.(*dnsmessage.PTRResource)
		return ok && nameEqual(x.PTR, y.PTR)
	case *dnsmessage.SRVResource:
		y, ok := b.(*dnsmessage.SRVResource)
		return ok && x.Priority == y.Priority && x.Weight == y.Weight && x.Port == y.Port &&
			nameEqual(x.Target, y.Target)
	case *dnsmessage.TXTResource:
		y, ok := b.(*dnsmessage.TXTResource)
		return ok && slices.Equal(x.TXT, y.TXT)
	}
	return false
}

// addressRecords returns the A and AAAA records that map host to addrs.
func addressRecords(host dnsmessage.Name, addrs []netip.Addr) []record {
	recs := make([]record, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if a.Is4() {
			recs = append(recs, record{name: host, typ: dnsmessage.TypeA, unique: true, ttl: hostTTL,
				body: &dnsmessage.AResource{A: a.As4()}})
		} else {
			recs = append(recs, record{name: host, typ: dnsmessage.TypeAAAA, unique: true, ttl: hostTTL,
				body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
		}
	}
	return recs
}

// appendNew appends the records of add that recs doesn't hold yet.
func appendNew(recs []record, add ...record) []record {
	for _, a := range add {
		if !slices.ContainsFunc(recs, a.equal) {
			recs = append(recs, a)
		}
	}
	return recs
}

// without returns the records of recs that aren't in drop.
func without(recs, drop []record) []record {
	var out []record
	for _, r := range recs {
		if !slices.ContainsFunc(drop, r.equal) {
			out = append(out, r)
		}
	}
	return out
}
