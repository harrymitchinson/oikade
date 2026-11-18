package mdns

import (
	"bytes"
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
// sends, so any other type is never the same as one of ours. NSEC, which dnsmessage doesn't
// know, is an UnknownResource, compared byte for byte.
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
	case *dnsmessage.UnknownResource:
		y, ok := b.(*dnsmessage.UnknownResource)
		if !ok || x.Type != y.Type {
			return false
		}
		if x.Type == typeNSEC {
			xb, xok := nsecBitmap(x.Data)
			yb, yok := nsecBitmap(y.Data)
			return xok && yok && bytes.Equal(xb, yb)
		}
		return bytes.Equal(x.Data, y.Data)
	}
	return false
}

// typeNSEC is the NSEC record's type (RFC 4034, 4), which dnsmessage has no name for.
const typeNSEC dnsmessage.Type = 47

// withOwnNSEC returns recs with an NSEC record for each name that has unique records among them,
// saying which types the name has and so that it has no others (RFC 6762, 6.1). A querier that
// asks for a type a name hasn't, such as an AAAA on an IPv4-only link, then knows at once.
func withOwnNSEC(recs []record) []record {
	out := slices.Clone(recs)
	var names []dnsmessage.Name
	for _, rec := range recs {
		if rec.unique && rec.typ != typeNSEC && !slices.ContainsFunc(names, func(n dnsmessage.Name) bool { return nameEqual(n, rec.name) }) {
			names = append(names, rec.name)
		}
	}
	for _, n := range names {
		if rec, ok := nsec(n, recs); ok {
			out = append(out, rec)
		}
	}
	return out
}

// nsec returns the NSEC record for name, from the unique records of recs that have it, or false if
// none has. It's mDNS's restricted form: the next name is name itself, and the bitmap is window 0
// alone, since every type the responder sends is below 256. The bitmap leaves out NSEC itself, as
// Apple's responder does. Its TTL is the longest of the name's records, which a negative answer
// for another type stands in for.
func nsec(name dnsmessage.Name, recs []record) (record, bool) {
	var types []dnsmessage.Type
	var ttl uint32
	for _, rec := range recs {
		if rec.unique && rec.typ != typeNSEC && rec.typ < 256 && nameEqual(rec.name, name) {
			name = rec.name // ours, in our case
			ttl = max(ttl, rec.ttl)
			if !slices.Contains(types, rec.typ) {
				types = append(types, rec.typ)
			}
		}
	}
	if len(types) == 0 {
		return record{}, false
	}
	bitmap := make([]byte, int(slices.Max(types))/8+1)
	for _, t := range types {
		bitmap[t/8] |= 0x80 >> (t % 8)
	}
	// The next name goes uncompressed, which any reader can follow.
	data := slices.Concat(wireName(name), []byte{0, byte(len(bitmap))}, bitmap)
	return record{
		name: name, typ: typeNSEC, unique: true, ttl: ttl,
		body: &dnsmessage.UnknownResource{Type: typeNSEC, Data: data},
	}, true
}

// nsecOf returns the NSEC record among recs for name.
func nsecOf(recs []record, name dnsmessage.Name) (record, bool) {
	for _, rec := range recs {
		if rec.typ == typeNSEC && nameEqual(rec.name, name) {
			return rec, true
		}
	}
	return record{}, false
}

// withNSEC returns additionals with the NSEC record from recs for each name that has unique
// records among answers and additionals, unless answers already carry it.
func withNSEC(answers, additionals, recs []record) []record {
	for _, rec := range slices.Concat(answers, additionals) {
		if !rec.unique || rec.typ == typeNSEC {
			continue
		}
		if n, ok := nsecOf(recs, rec.name); ok && !slices.ContainsFunc(answers, n.equal) {
			additionals = appendNew(additionals, n)
		}
	}
	return additionals
}

// nsecBitmap returns the type bitmaps of an NSEC record's data, after its next name. The next name
// may be compressed (RFC 6762, 18.14), which a pointer into the rest of the message ends, so it's
// skipped rather than read: two NSEC records with the same owner name hold the same data when
// their bitmaps match, as mDNS's next name is the owner name.
func nsecBitmap(data []byte) ([]byte, bool) {
	for i := 0; i < len(data); {
		switch n := int(data[i]); {
		case n == 0:
			return data[i+1:], true
		case n&0xC0 == 0xC0:
			if i+2 > len(data) {
				return nil, false
			}
			return data[i+2:], true
		default:
			i += 1 + n
		}
	}
	return nil, false
}

// wireName returns name in DNS wire format, uncompressed. dnsmessage treats every dot as a label's
// end, so splitting on them reads a name as it does.
func wireName(name dnsmessage.Name) []byte {
	var b []byte
	for label := range strings.SplitSeq(strings.TrimSuffix(name.String(), "."), ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
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
