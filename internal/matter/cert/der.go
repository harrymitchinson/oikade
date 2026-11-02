package cert

import (
	"bytes"
	"encoding/asn1"
)

// DER tags this package reads and writes.
const (
	tagBoolean          = 0x01
	tagInteger          = 0x02
	tagBitString        = 0x03
	tagOctetString      = 0x04
	tagOID              = 0x06
	tagUTF8String       = 0x0C
	tagPrintableString  = 0x13
	tagIA5String        = 0x16
	tagUTCTime          = 0x17
	tagGeneralizedTime  = 0x18
	tagSequence         = 0x30
	tagSet              = 0x31
	tagContext0         = 0x80 // [0] IMPLICIT, primitive
	tagContext0Explicit = 0xA0 // [0], constructed
	tagContext3Explicit = 0xA3 // [3], constructed
)

// der encodes one DER element: tag, length and the parts' contents.
func der(tag byte, parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n+4)
	out = append(out, tag)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	case n <= 0xFF:
		out = append(out, 0x81, byte(n))
	case n <= 0xFFFF:
		out = append(out, 0x82, byte(n>>8), byte(n))
	default:
		out = append(out, 0x84, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// derUint encodes a non-negative big-endian integer as a DER INTEGER, with no leading zeros but
// the one a set top bit needs.
func derUint(b []byte) []byte {
	b = bytes.TrimLeft(b, "\x00")
	if len(b) == 0 || b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return der(tagInteger, b)
}

func derOID(oid asn1.ObjectIdentifier) []byte {
	b, err := asn1.Marshal(oid)
	if err != nil {
		panic(err) // only for the constant OIDs below
	}
	return b
}

// Object identifiers, DER encoded.
var (
	oidECDSAWithSHA256 = derOID(asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2})
	oidECPublicKey     = derOID(asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1})
	oidPrime256v1      = derOID(asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7})

	oidBasicConstraints       = derOID(asn1.ObjectIdentifier{2, 5, 29, 19})
	oidKeyUsage               = derOID(asn1.ObjectIdentifier{2, 5, 29, 15})
	oidExtKeyUsage            = derOID(asn1.ObjectIdentifier{2, 5, 29, 37})
	oidSubjectKeyIdentifier   = derOID(asn1.ObjectIdentifier{2, 5, 29, 14})
	oidAuthorityKeyIdentifier = derOID(asn1.ObjectIdentifier{2, 5, 29, 35})

	keyPurposeOIDs = map[KeyPurpose][]byte{
		PurposeServerAuth:      derOID(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}),
		PurposeClientAuth:      derOID(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}),
		PurposeCodeSigning:     derOID(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 3}),
		PurposeEmailProtection: derOID(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 4}),
		PurposeTimeStamping:    derOID(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}),
		PurposeOCSPSigning:     derOID(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 9}),
	}

	attributeOIDsDER = func() map[AttributeType][]byte {
		m := make(map[AttributeType][]byte, len(attributeOIDs))
		for t, oid := range attributeOIDs {
			m[t] = derOID(oid)
		}
		return m
	}()

	derTrue = []byte{tagBoolean, 1, 0xFF}
)

// derReader reads DER elements in sequence. It accepts only definite, minimal lengths, and
// single-byte tags.
type derReader []byte

func (r *derReader) empty() bool { return len(*r) == 0 }

// peek returns the next element's tag, or 0 at the end.
func (r *derReader) peek() byte {
	if r.empty() {
		return 0
	}
	return (*r)[0]
}

// read reads the next element, which must have the given tag, and returns its contents and its
// whole encoding.
func (r *derReader) read(tag byte) (contents, whole []byte, err error) {
	b := *r
	if len(b) < 2 {
		return nil, nil, invalid("DER truncated")
	}
	if b[0] != tag {
		return nil, nil, invalid("DER tag %#02x, want %#02x", b[0], tag)
	}
	n, off := int(b[1]), 2
	switch {
	case b[1] < 0x80:
	case b[1] == 0x81 && len(b) >= 3 && b[2] >= 0x80:
		n, off = int(b[2]), 3
	case b[1] == 0x82 && len(b) >= 4 && b[2] != 0:
		n, off = int(b[2])<<8|int(b[3]), 4
	default:
		return nil, nil, invalid("DER length")
	}
	if len(b)-off < n {
		return nil, nil, invalid("DER truncated")
	}
	*r = b[off+n:]
	return b[off : off+n], b[:off+n], nil
}

// readInto reads the next element, which must have the given tag, and returns a reader over its
// contents.
func (r *derReader) readInto(tag byte) (derReader, error) {
	c, _, err := r.read(tag)
	return derReader(c), err
}

// expect reads the next element, whose whole encoding must be want.
func (r *derReader) expect(want []byte, what string) error {
	if len(want) == 0 || r.peek() != want[0] {
		return invalid("%s missing", what)
	}
	_, whole, err := r.read(want[0])
	if err != nil {
		return err
	}
	if !bytes.Equal(whole, want) {
		return invalid("%s unsupported", what)
	}
	return nil
}

func (r *derReader) end(what string) error {
	if !r.empty() {
		return invalid("unexpected data after %s", what)
	}
	return nil
}
