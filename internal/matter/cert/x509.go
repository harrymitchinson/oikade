package cert

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"math/bits"
	"strconv"
	"time"
)

// noExpiry is X.509's "no well-defined expiration date", which Matter writes as epoch time 0.
var noExpiry = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// X509 returns the certificate's X.509 DER encoding, as connectedhomeip's
// ConvertChipCertToX509Cert writes it.
func (c *Certificate) X509() ([]byte, error) {
	if len(c.Signature) != signatureLength {
		return nil, invalid("signature of %d bytes", len(c.Signature))
	}
	tbs, err := c.tbs()
	if err != nil {
		return nil, err
	}
	return der(tagSequence,
		tbs,
		der(tagSequence, oidECDSAWithSHA256),
		der(tagBitString, []byte{0}, signatureDER(c.Signature)),
	), nil
}

// tbs returns the DER encoding of the TBSCertificate, which the signature covers.
func (c *Certificate) tbs() ([]byte, error) {
	notBefore, err := toEpoch(c.NotBefore)
	if err != nil {
		return nil, err
	}
	notAfter, err := toEpoch(c.NotAfter)
	if err != nil {
		return nil, err
	}
	// The signature isn't part of the TBSCertificate, so one that isn't there yet is fine.
	if err := c.checkFields(false); err != nil {
		return nil, err
	}
	var exts [][]byte
	future := 0
	for _, n := range c.extensionOrder() {
		var ext []byte
		switch n {
		case tagBasicConstraints:
			var fields []byte
			if c.BasicConstraints.IsCA {
				fields = append(fields, derTrue...)
			}
			if c.BasicConstraints.MaxPathLen != nil {
				fields = append(fields, derUint([]byte{*c.BasicConstraints.MaxPathLen})...)
			}
			ext = extension(oidBasicConstraints, true, der(tagSequence, fields))
		case tagKeyUsage:
			ext = extension(oidKeyUsage, true, der(tagBitString, keyUsageBits(c.KeyUsage)))
		case tagExtKeyUsage:
			var oids []byte
			for _, p := range c.ExtKeyUsage {
				oids = append(oids, keyPurposeOIDs[p]...)
			}
			ext = extension(oidExtKeyUsage, true, der(tagSequence, oids))
		case tagSubjectKeyID:
			ext = extension(oidSubjectKeyIdentifier, false, der(tagOctetString, c.SubjectKeyID))
		case tagAuthorityKeyID:
			ext = extension(oidAuthorityKeyIdentifier, false, der(tagSequence, der(tagContext0, c.AuthorityKeyID)))
		case tagFutureExtension:
			ext = c.FutureExtensions[future]
			future++
		}
		exts = append(exts, ext)
	}
	return der(tagSequence,
		der(tagContext0Explicit, derUint([]byte{2})), // version 3
		der(tagInteger, c.SerialNumber),
		der(tagSequence, oidECDSAWithSHA256),
		nameDER(c.Issuer),
		der(tagSequence, timeDER(notBefore), timeDER(notAfter)),
		nameDER(c.Subject),
		der(tagSequence, der(tagSequence, oidECPublicKey, oidPrime256v1), der(tagBitString, []byte{0}, c.PublicKey)),
		der(tagContext3Explicit, der(tagSequence, exts...)),
	), nil
}

func extension(oid []byte, critical bool, value []byte) []byte {
	if critical {
		return der(tagSequence, oid, derTrue, der(tagOctetString, value))
	}
	return der(tagSequence, oid, der(tagOctetString, value))
}

func nameDER(dn DN) []byte {
	rdns := make([][]byte, 0, len(dn))
	for _, a := range dn {
		tag, s := a.x509Value()
		rdns = append(rdns, der(tagSet, der(tagSequence, attributeOIDsDER[a.Type], der(tag, []byte(s)))))
	}
	return der(tagSequence, rdns...)
}

// timeDER encodes Matter epoch time as X.509 does: UTCTime from 1950 to 2049, GeneralizedTime
// otherwise.
func timeDER(epoch uint32) []byte {
	t := noExpiry
	if epoch != 0 {
		t = fromEpoch(epoch)
	}
	if t.Year() >= 1950 && t.Year() < 2050 {
		return der(tagUTCTime, []byte(t.Format("060102150405Z")))
	}
	return der(tagGeneralizedTime, []byte(t.Format("20060102150405Z")))
}

// keyUsageBits encodes key usage as a DER named-bit BIT STRING's contents: bit 0 is the first
// byte's top bit, and trailing zero bits are dropped.
func keyUsageBits(ku x509.KeyUsage) []byte {
	if ku == 0 {
		return []byte{0}
	}
	top := bits.Len16(uint16(ku)) - 1
	out := make([]byte, 1+top/8+1)
	out[0] = byte(7 - top%8)
	for i := 0; i <= top; i++ {
		if ku&(1<<i) != 0 {
			out[1+i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}

// signatureDER encodes a raw r||s signature as an ECDSA-Sig-Value.
func signatureDER(sig []byte) []byte {
	half := len(sig) / 2
	return der(tagSequence, derUint(sig[:half]), derUint(sig[half:]))
}

// FromX509 converts an X.509 DER certificate to Matter's form. The certificate must be within
// Matter's profile, fit Matter's size limit, and convert back to exactly the same DER; otherwise
// its signature couldn't be checked from the TLV form, and FromX509 returns an error wrapping
// ErrInvalid.
func FromX509(b []byte) (*Certificate, error) {
	if len(b) > MaxDERLength {
		return nil, invalid("%d bytes, more than %d", len(b), MaxDERLength)
	}
	c, err := fromX509(b)
	if err != nil {
		return nil, err
	}
	again, err := c.X509()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(again, b) {
		return nil, invalid("X.509 encoding isn't the one Matter's TLV form converts to")
	}
	if _, err := c.Encode(); err != nil { // too long for Matter's TLV form, say
		return nil, err
	}
	return c, nil
}

func fromX509(b []byte) (*Certificate, error) {
	top := derReader(b)
	cert, err := top.readInto(tagSequence)
	if err != nil {
		return nil, err
	}
	if err := top.end("the certificate"); err != nil {
		return nil, err
	}
	tbs, err := cert.readInto(tagSequence)
	if err != nil {
		return nil, err
	}
	c := &Certificate{}
	if err := parseTBS(&tbs, c); err != nil {
		return nil, err
	}
	if err := cert.expect(der(tagSequence, oidECDSAWithSHA256), "signature algorithm"); err != nil {
		return nil, err
	}
	sig, _, err := cert.read(tagBitString)
	if err != nil {
		return nil, err
	}
	if c.Signature, err = parseSignature(sig); err != nil {
		return nil, err
	}
	return c, cert.end("the signature")
}

func parseTBS(r *derReader, c *Certificate) error {
	if err := r.expect(der(tagContext0Explicit, derUint([]byte{2})), "version"); err != nil {
		return err
	}
	serial, _, err := r.read(tagInteger)
	if err != nil {
		return err
	}
	c.SerialNumber = bytes.Clone(serial)
	if err := r.expect(der(tagSequence, oidECDSAWithSHA256), "signature algorithm"); err != nil {
		return err
	}
	if c.Issuer, err = parseName(r); err != nil {
		return err
	}
	validity, err := r.readInto(tagSequence)
	if err != nil {
		return err
	}
	if c.NotBefore, err = parseTime(&validity); err != nil {
		return err
	}
	if c.NotAfter, err = parseTime(&validity); err != nil {
		return err
	}
	if err := validity.end("the validity period"); err != nil {
		return err
	}
	if !c.NotAfter.IsZero() && !c.NotBefore.Before(c.NotAfter) {
		return invalid("validity period ends before it starts")
	}
	if c.Subject, err = parseName(r); err != nil {
		return err
	}
	spki, err := r.readInto(tagSequence)
	if err != nil {
		return err
	}
	if err := spki.expect(der(tagSequence, oidECPublicKey, oidPrime256v1), "public key algorithm"); err != nil {
		return err
	}
	key, _, err := spki.read(tagBitString)
	if err != nil {
		return err
	}
	if len(key) != 1+publicKeyLength || key[0] != 0 {
		return invalid("public key of %d bytes", len(key))
	}
	c.PublicKey = bytes.Clone(key[1:])
	if err := spki.end("the public key"); err != nil {
		return err
	}
	exts, err := r.readInto(tagContext3Explicit)
	if err != nil {
		return err
	}
	list, err := exts.readInto(tagSequence)
	if err != nil {
		return err
	}
	if err := exts.end("the extensions"); err != nil {
		return err
	}
	c.order = []uint8{}
	for !list.empty() {
		if err := parseExtension(&list, c); err != nil {
			return err
		}
	}
	return r.end("the extensions")
}

func parseName(r *derReader) (DN, error) {
	rdns, err := r.readInto(tagSequence)
	if err != nil {
		return nil, err
	}
	dn := DN{}
	for !rdns.empty() {
		set, err := rdns.readInto(tagSet)
		if err != nil {
			return nil, err
		}
		atv, err := set.readInto(tagSequence)
		if err != nil {
			return nil, err
		}
		if err := set.end("an attribute: Matter allows one in each RDN"); err != nil {
			return nil, err
		}
		_, oid, err := atv.read(tagOID)
		if err != nil {
			return nil, err
		}
		a := Attribute{Type: attributeTypes[string(oid)]}
		if a.Type == 0 {
			return nil, invalid("unsupported distinguished name attribute")
		}
		tag := atv.peek()
		value, _, err := atv.read(tag)
		if err != nil {
			return nil, err
		}
		if err := atv.end("an attribute value"); err != nil {
			return nil, err
		}
		switch {
		case a.Type.IsMatter():
			v, ok := parseUpperHex(string(value), a.Type.hexDigits())
			if tag != tagUTF8String || !ok {
				return nil, invalid("attribute %s isn't %d upper-case hex digits", a.Type, a.Type.hexDigits())
			}
			a.Value = v
		case a.Type == AttrDomainComponent && tag != tagIA5String,
			a.Type != AttrDomainComponent && tag != tagUTF8String && tag != tagPrintableString:
			return nil, invalid("attribute %s has string type %#02x", a.Type, tag)
		default:
			a.Text = string(value)
			a.Printable = tag == tagPrintableString
			if err := a.check(); err != nil {
				return nil, err
			}
		}
		dn = append(dn, a)
	}
	return dn, nil
}

func parseTime(r *derReader) (time.Time, error) {
	var s []byte
	var err error
	var year int
	switch r.peek() {
	case tagUTCTime:
		if s, _, err = r.read(tagUTCTime); err != nil {
			return time.Time{}, err
		}
		if len(s) != 13 {
			return time.Time{}, invalid("UTCTime %q", s)
		}
		yy, err := strconv.Atoi(string(s[:2]))
		if err != nil {
			return time.Time{}, invalid("UTCTime %q", s)
		}
		year = 1900 + yy
		if yy < 50 {
			year = 2000 + yy
		}
		s = fmt.Appendf(nil, "%04d%s", year, s[2:])
	case tagGeneralizedTime:
		if s, _, err = r.read(tagGeneralizedTime); err != nil {
			return time.Time{}, err
		}
	default:
		return time.Time{}, invalid("validity time missing")
	}
	t, err := time.Parse("20060102150405Z", string(s))
	if err != nil {
		return time.Time{}, invalid("time %q", s)
	}
	if t.Equal(noExpiry) {
		return time.Time{}, nil
	}
	if _, err := toEpoch(t); err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func parseExtension(r *derReader, c *Certificate) error {
	ext, whole, err := r.read(tagSequence)
	if err != nil {
		return err
	}
	e := derReader(ext)
	_, oid, err := e.read(tagOID)
	if err != nil {
		return err
	}
	critical := false
	if e.peek() == tagBoolean {
		if err := e.expect(derTrue, "critical flag"); err != nil {
			return err
		}
		critical = true
	}
	value, err := e.readInto(tagOctetString)
	if err != nil {
		return err
	}
	if err := e.end("an extension"); err != nil {
		return err
	}
	n := uint8(tagFutureExtension)
	switch {
	case bytes.Equal(oid, oidBasicConstraints):
		n = tagBasicConstraints
		err = parseBasicConstraints(&value, c)
	case bytes.Equal(oid, oidKeyUsage):
		n = tagKeyUsage
		err = parseKeyUsage(&value, c)
	case bytes.Equal(oid, oidExtKeyUsage):
		n = tagExtKeyUsage
		err = parseExtKeyUsage(&value, c)
	case bytes.Equal(oid, oidSubjectKeyIdentifier):
		n = tagSubjectKeyID
		var id []byte
		if id, _, err = value.read(tagOctetString); err == nil {
			c.SubjectKeyID = bytes.Clone(id)
		}
	case bytes.Equal(oid, oidAuthorityKeyIdentifier):
		n = tagAuthorityKeyID
		var akid derReader
		if akid, err = value.readInto(tagSequence); err == nil {
			var id []byte
			if id, _, err = akid.read(tagContext0); err == nil {
				c.AuthorityKeyID = bytes.Clone(id)
				err = akid.end("the authority key identifier")
			}
		}
	default:
		c.FutureExtensions = append(c.FutureExtensions, bytes.Clone(whole))
		value = nil
	}
	if err != nil {
		return err
	}
	// Matter's TLV form marks exactly basic constraints, key usage and extended key usage critical.
	wantCritical := n == tagBasicConstraints || n == tagKeyUsage || n == tagExtKeyUsage
	if n != tagFutureExtension {
		if critical != wantCritical {
			return invalid("extension %d has the wrong critical flag", n)
		}
		if bytes.Contains(c.order, []byte{n}) {
			return invalid("extension %d twice", n)
		}
	}
	c.order = append(c.order, n)
	return value.end("an extension value")
}

func parseBasicConstraints(r *derReader, c *Certificate) error {
	bc, err := r.readInto(tagSequence)
	if err != nil {
		return err
	}
	c.BasicConstraints = &BasicConstraints{}
	if bc.peek() == tagBoolean {
		if err := bc.expect(derTrue, "CA flag"); err != nil {
			return err
		}
		c.BasicConstraints.IsCA = true
	}
	if bc.peek() == tagInteger {
		n, _, err := bc.read(tagInteger)
		if err != nil {
			return err
		}
		switch {
		case len(n) == 1 && n[0] < 0x80:
		case len(n) == 2 && n[0] == 0 && n[1] >= 0x80:
			n = n[1:]
		default:
			return invalid("path length constraint")
		}
		v := n[0]
		c.BasicConstraints.MaxPathLen = &v
	}
	return bc.end("the basic constraints")
}

func parseKeyUsage(r *derReader, c *Certificate) error {
	b, _, err := r.read(tagBitString)
	if err != nil {
		return err
	}
	if len(b) < 2 || len(b) > 3 || b[0] > 7 {
		return invalid("key usage")
	}
	var ku x509.KeyUsage
	for i := 0; i < (len(b)-1)*8; i++ {
		if b[1+i/8]&(0x80>>(i%8)) != 0 {
			ku |= 1 << i
		}
	}
	if ku == 0 || ku&^keyUsageMask != 0 {
		return invalid("key usage %#x", ku)
	}
	c.KeyUsage = ku
	return nil
}

func parseExtKeyUsage(r *derReader, c *Certificate) error {
	list, err := r.readInto(tagSequence)
	if err != nil {
		return err
	}
	c.ExtKeyUsage = []KeyPurpose{}
	for !list.empty() {
		_, oid, err := list.read(tagOID)
		if err != nil {
			return err
		}
		purpose := keyPurposes[string(oid)]
		if purpose == 0 {
			return invalid("unsupported key purpose")
		}
		c.ExtKeyUsage = append(c.ExtKeyUsage, purpose)
	}
	return nil
}

// parseSignature converts an X.509 signature BIT STRING's contents to raw r||s.
func parseSignature(b []byte) ([]byte, error) {
	if len(b) < 1 || b[0] != 0 {
		return nil, invalid("signature")
	}
	outer := derReader(b[1:])
	seq, err := outer.readInto(tagSequence)
	if err != nil {
		return nil, err
	}
	if err := outer.end("the signature"); err != nil {
		return nil, err
	}
	raw := make([]byte, signatureLength)
	for i := range 2 {
		n, _, err := seq.read(tagInteger)
		if err != nil {
			return nil, err
		}
		if len(n) > 1 && n[0] == 0 {
			n = n[1:]
		}
		if len(n) == 0 || len(n) > signatureLength/2 {
			return nil, invalid("signature integer of %d bytes", len(n))
		}
		copy(raw[(i+1)*signatureLength/2-len(n):], n)
	}
	return raw, seq.end("the signature")
}

// futureCritical parses a future extension, a whole DER Extension, and reports whether it is
// marked critical. Its object identifier mustn't be one of the extensions Matter's form names.
func futureCritical(ext []byte) (bool, error) {
	r := derReader(ext)
	e, err := r.readInto(tagSequence)
	if err != nil {
		return false, err
	}
	if err := r.end("a future extension"); err != nil {
		return false, err
	}
	_, oid, err := e.read(tagOID)
	if err != nil {
		return false, err
	}
	for _, known := range [][]byte{oidBasicConstraints, oidKeyUsage, oidExtKeyUsage, oidSubjectKeyIdentifier, oidAuthorityKeyIdentifier} {
		if bytes.Equal(oid, known) {
			return false, invalid("future extension is a known extension")
		}
	}
	critical := false
	if e.peek() == tagBoolean {
		v, _, err := e.read(tagBoolean)
		if err != nil {
			return false, err
		}
		critical = len(v) == 1 && v[0] != 0
	}
	if _, _, err := e.read(tagOctetString); err != nil {
		return false, err
	}
	return critical, e.end("a future extension")
}
