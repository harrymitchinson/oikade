package cert

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// Size limits (Core spec, 6.5): the largest certificate Matter sends, and the largest X.509
// form of one.
const (
	MaxTLVLength = 400
	MaxDERLength = 600
)

// Context tags of the TLV form (Core spec, 6.5).
const (
	tagSerialNumber     = 1
	tagSignatureAlgo    = 2
	tagIssuer           = 3
	tagNotBefore        = 4
	tagNotAfter         = 5
	tagSubject          = 6
	tagPublicKeyAlgo    = 7
	tagCurve            = 8
	tagPublicKey        = 9
	tagExtensions       = 10
	tagSignature        = 11
	tagBasicConstraints = 1
	tagKeyUsage         = 2
	tagExtKeyUsage      = 3
	tagSubjectKeyID     = 4
	tagAuthorityKeyID   = 5
	tagFutureExtension  = 6
	tagIsCA             = 1
	tagPathLen          = 2
)

// The algorithm numbers Matter allows: ECDSA with SHA-256, an EC public key and P-256.
const (
	sigAlgoECDSAWithSHA256 = 1
	pubKeyAlgoEC           = 1
	curvePrime256v1        = 1
)

const (
	keyIDLength     = 20
	publicKeyLength = 65
	signatureLength = 64
	maxSerialLength = 20
	// keyUsageMask is the key usages X.509 defines, through decipherOnly.
	keyUsageMask = 0x1FF
)

// matterEpoch is the start of Matter's epoch time: 2000-01-01T00:00:00Z.
var matterEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// toEpoch converts a validity time to Matter epoch seconds. The zero time is epoch 0, which means
// no expiry.
func toEpoch(t time.Time) (uint32, error) {
	if t.IsZero() {
		return 0, nil
	}
	s := t.Unix() - matterEpoch.Unix()
	if s < 0 || s > math.MaxUint32 || t.Nanosecond() != 0 {
		return 0, invalid("validity time %s isn't a whole second from 2000 to 2136", t)
	}
	return uint32(s), nil
}

func fromEpoch(s uint32) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return matterEpoch.Add(time.Duration(s) * time.Second)
}

// Decode decodes a certificate from its Matter TLV form. It checks the form is well formed and
// within Matter's profile, but not what the certificate says: Verify does that.
func Decode(b []byte) (*Certificate, error) {
	if len(b) > MaxTLVLength {
		return nil, invalid("%d bytes, more than %d", len(b), MaxTLVLength)
	}
	c, err := decode(tlv.NewReader(b))
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return c, nil
}

func decode(r *tlv.Reader) (*Certificate, error) {
	if err := r.Next(); err != nil {
		return nil, err
	}
	if r.Type() != tlv.Structure || r.Tag() != tlv.Anonymous {
		return nil, invalid("not an anonymous structure")
	}
	if err := r.Enter(); err != nil {
		return nil, err
	}
	c := &Certificate{}
	var err error
	if c.SerialNumber, err = getBytes(r, tagSerialNumber); err != nil {
		return nil, err
	}
	if len(c.SerialNumber) == 0 || len(c.SerialNumber) > maxSerialLength {
		return nil, invalid("serial number of %d bytes", len(c.SerialNumber))
	}
	if err := expectUint(r, tagSignatureAlgo, sigAlgoECDSAWithSHA256); err != nil {
		return nil, err
	}
	if c.Issuer, err = decodeDN(r, tagIssuer); err != nil {
		return nil, err
	}
	notBefore, err := getUint(r, tagNotBefore, math.MaxUint32)
	if err != nil {
		return nil, err
	}
	notAfter, err := getUint(r, tagNotAfter, math.MaxUint32)
	if err != nil {
		return nil, err
	}
	if notAfter != 0 && notBefore >= notAfter {
		return nil, invalid("validity period ends before it starts")
	}
	c.NotBefore, c.NotAfter = fromEpoch(uint32(notBefore)), fromEpoch(uint32(notAfter))
	if c.Subject, err = decodeDN(r, tagSubject); err != nil {
		return nil, err
	}
	if err := expectUint(r, tagPublicKeyAlgo, pubKeyAlgoEC); err != nil {
		return nil, err
	}
	if err := expectUint(r, tagCurve, curvePrime256v1); err != nil {
		return nil, err
	}
	if c.PublicKey, err = getBytes(r, tagPublicKey); err != nil {
		return nil, err
	}
	if _, err := parsePublicKey(c.PublicKey); err != nil {
		return nil, err
	}
	if err := decodeExtensions(r, c); err != nil {
		return nil, err
	}
	if c.Signature, err = getBytes(r, tagSignature); err != nil {
		return nil, err
	}
	if len(c.Signature) != signatureLength {
		return nil, invalid("signature of %d bytes", len(c.Signature))
	}
	if err := r.Next(); !errors.Is(err, io.EOF) {
		return nil, invalid("unexpected element after the signature")
	}
	if err := r.Exit(); err != nil {
		return nil, err
	}
	if err := r.Next(); !errors.Is(err, io.EOF) {
		return nil, invalid("unexpected data after the certificate")
	}
	return c, nil
}

// next moves to the next element, which must have the context tag n.
func next(r *tlv.Reader, n uint8) error {
	if err := r.Next(); err != nil {
		if errors.Is(err, io.EOF) {
			return invalid("field %d missing", n)
		}
		return err
	}
	if r.Tag() != tlv.Context(n) {
		return invalid("field %s, want %d", r.Tag(), n)
	}
	return nil
}

func getBytes(r *tlv.Reader, n uint8) ([]byte, error) {
	if err := next(r, n); err != nil {
		return nil, err
	}
	b, err := r.Bytes()
	return bytes.Clone(b), err
}

func getUint(r *tlv.Reader, n uint8, limit uint64) (uint64, error) {
	if err := next(r, n); err != nil {
		return 0, err
	}
	return readUint(r, limit)
}

func readUint(r *tlv.Reader, limit uint64) (uint64, error) {
	v, err := r.Uint()
	if err != nil {
		return 0, err
	}
	if v > limit {
		return 0, invalid("value %d out of range", v)
	}
	return v, nil
}

func expectUint(r *tlv.Reader, n uint8, want uint64) error {
	v, err := getUint(r, n, math.MaxUint64)
	if err != nil {
		return err
	}
	if v != want {
		return invalid("field %d is %d; only %d is supported", n, v, want)
	}
	return nil
}

func decodeDN(r *tlv.Reader, n uint8) (DN, error) {
	if err := next(r, n); err != nil {
		return nil, err
	}
	if r.Type() != tlv.List {
		return nil, invalid("distinguished name is a %s", r.Type())
	}
	if err := r.Enter(); err != nil {
		return nil, err
	}
	dn := DN{}
	for {
		err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		tag := r.Tag()
		if tag.Kind != tlv.ContextTag {
			return nil, invalid("distinguished name attribute tagged %s", tag)
		}
		a := Attribute{Type: AttributeType(tag.Number &^ printableFlag), Printable: tag.Number&printableFlag != 0}
		if !a.Type.valid() {
			return nil, invalid("attribute type %d", a.Type)
		}
		if a.Type.IsMatter() {
			limit := uint64(math.MaxUint64)
			if a.Type == AttrCASEAuthTag {
				limit = math.MaxUint32
			}
			a.Value, err = readUint(r, limit)
		} else {
			a.Text, err = r.UTF8()
		}
		if err != nil {
			return nil, err
		}
		if err := a.check(); err != nil {
			return nil, err
		}
		dn = append(dn, a)
	}
	return dn, r.Exit()
}

func decodeExtensions(r *tlv.Reader, c *Certificate) error {
	if err := next(r, tagExtensions); err != nil {
		return err
	}
	if r.Type() != tlv.List {
		return invalid("extensions are a %s", r.Type())
	}
	if err := r.Enter(); err != nil {
		return err
	}
	c.order = []uint8{}
	for {
		err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		tag := r.Tag()
		if tag.Kind != tlv.ContextTag || tag.Number < tagBasicConstraints || tag.Number > tagFutureExtension {
			return invalid("extension tagged %s", tag)
		}
		n := uint8(tag.Number)
		if n != tagFutureExtension && slices.Contains(c.order, n) {
			return invalid("extension %d twice", n)
		}
		if err := decodeExtension(r, c, n); err != nil {
			return err
		}
		c.order = append(c.order, n)
	}
	return r.Exit()
}

func decodeExtension(r *tlv.Reader, c *Certificate, n uint8) error {
	switch n {
	case tagBasicConstraints:
		return decodeBasicConstraints(r, c)
	case tagKeyUsage:
		ku, err := readUint(r, keyUsageMask)
		if err != nil {
			return err
		}
		if ku == 0 {
			return invalid("empty key usage")
		}
		c.KeyUsage = x509.KeyUsage(ku)
	case tagExtKeyUsage:
		if r.Type() != tlv.Array {
			return invalid("extended key usage is a %s", r.Type())
		}
		if err := r.Enter(); err != nil {
			return err
		}
		c.ExtKeyUsage = []KeyPurpose{}
		for {
			err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			p, err := readUint(r, uint64(PurposeOCSPSigning))
			if err != nil {
				return err
			}
			if p == 0 {
				return invalid("key purpose 0")
			}
			c.ExtKeyUsage = append(c.ExtKeyUsage, KeyPurpose(p))
		}
		return r.Exit()
	case tagSubjectKeyID, tagAuthorityKeyID:
		b, err := r.Bytes()
		if err != nil {
			return err
		}
		if len(b) != keyIDLength {
			return invalid("key identifier of %d bytes", len(b))
		}
		if n == tagSubjectKeyID {
			c.SubjectKeyID = bytes.Clone(b)
		} else {
			c.AuthorityKeyID = bytes.Clone(b)
		}
	case tagFutureExtension:
		b, err := r.Bytes()
		if err != nil {
			return err
		}
		if _, err := futureCritical(b); err != nil {
			return err
		}
		c.FutureExtensions = append(c.FutureExtensions, bytes.Clone(b))
	}
	return nil
}

func decodeBasicConstraints(r *tlv.Reader, c *Certificate) error {
	if r.Type() != tlv.Structure {
		return invalid("basic constraints are a %s", r.Type())
	}
	if err := r.Enter(); err != nil {
		return err
	}
	if err := next(r, tagIsCA); err != nil {
		return err
	}
	isCA, err := r.Bool()
	if err != nil {
		return err
	}
	bc := &BasicConstraints{IsCA: isCA}
	err = r.Next()
	if err == nil {
		if r.Tag() != tlv.Context(tagPathLen) {
			return invalid("basic constraints field %s", r.Tag())
		}
		v, rerr := readUint(r, math.MaxUint8)
		if rerr != nil {
			return rerr
		}
		n := uint8(v)
		bc.MaxPathLen = &n
		err = r.Next()
	}
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return invalid("unexpected basic constraints field %s", r.Tag())
		}
		return err
	}
	c.BasicConstraints = bc
	return r.Exit()
}

// Encode returns the certificate's Matter TLV form.
func (c *Certificate) Encode() ([]byte, error) {
	notBefore, err := toEpoch(c.NotBefore)
	if err != nil {
		return nil, err
	}
	notAfter, err := toEpoch(c.NotAfter)
	if err != nil {
		return nil, err
	}
	if err := c.checkFields(true); err != nil {
		return nil, err
	}
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(tagSerialNumber), c.SerialNumber)
	w.PutUint(tlv.Context(tagSignatureAlgo), sigAlgoECDSAWithSHA256)
	encodeDN(&w, tagIssuer, c.Issuer)
	w.PutUint(tlv.Context(tagNotBefore), uint64(notBefore))
	w.PutUint(tlv.Context(tagNotAfter), uint64(notAfter))
	encodeDN(&w, tagSubject, c.Subject)
	w.PutUint(tlv.Context(tagPublicKeyAlgo), pubKeyAlgoEC)
	w.PutUint(tlv.Context(tagCurve), curvePrime256v1)
	w.PutBytes(tlv.Context(tagPublicKey), c.PublicKey)
	w.StartList(tlv.Context(tagExtensions))
	future := 0
	for _, n := range c.extensionOrder() {
		switch n {
		case tagBasicConstraints:
			w.StartStructure(tlv.Context(tagBasicConstraints))
			w.PutBool(tlv.Context(tagIsCA), c.BasicConstraints.IsCA)
			if c.BasicConstraints.MaxPathLen != nil {
				w.PutUint(tlv.Context(tagPathLen), uint64(*c.BasicConstraints.MaxPathLen))
			}
			w.End()
		case tagKeyUsage:
			w.PutUint(tlv.Context(tagKeyUsage), uint64(c.KeyUsage))
		case tagExtKeyUsage:
			w.StartArray(tlv.Context(tagExtKeyUsage))
			for _, p := range c.ExtKeyUsage {
				w.PutUint(tlv.Anonymous, uint64(p))
			}
			w.End()
		case tagSubjectKeyID:
			w.PutBytes(tlv.Context(tagSubjectKeyID), c.SubjectKeyID)
		case tagAuthorityKeyID:
			w.PutBytes(tlv.Context(tagAuthorityKeyID), c.AuthorityKeyID)
		case tagFutureExtension:
			w.PutBytes(tlv.Context(tagFutureExtension), c.FutureExtensions[future])
			future++
		}
	}
	w.End()
	w.PutBytes(tlv.Context(tagSignature), c.Signature)
	w.End()
	b := w.Bytes()
	if len(b) > MaxTLVLength {
		return nil, invalid("%d bytes, more than %d", len(b), MaxTLVLength)
	}
	return b, nil
}

func encodeDN(w *tlv.Writer, n uint8, dn DN) {
	w.StartList(tlv.Context(n))
	for _, a := range dn {
		tag := uint8(a.Type)
		if a.Printable {
			tag |= printableFlag
		}
		if a.Type.IsMatter() {
			w.PutUint(tlv.Context(tag), a.Value)
		} else {
			w.PutUTF8(tlv.Context(tag), a.Text)
		}
	}
	w.End()
}

// checkFields checks what both encodings need of the fields, and the signature too if withSig.
func (c *Certificate) checkFields(withSig bool) error {
	if len(c.SerialNumber) == 0 || len(c.SerialNumber) > maxSerialLength {
		return invalid("serial number of %d bytes", len(c.SerialNumber))
	}
	for _, dn := range []DN{c.Issuer, c.Subject} {
		for _, a := range dn {
			if err := a.check(); err != nil {
				return err
			}
		}
	}
	if _, err := parsePublicKey(c.PublicKey); err != nil {
		return err
	}
	if c.KeyUsage&^keyUsageMask != 0 {
		return invalid("key usage %#x", c.KeyUsage)
	}
	for _, p := range c.ExtKeyUsage {
		if keyPurposeOIDs[p] == nil {
			return invalid("key purpose %d", p)
		}
	}
	if c.SubjectKeyID != nil && len(c.SubjectKeyID) != keyIDLength ||
		c.AuthorityKeyID != nil && len(c.AuthorityKeyID) != keyIDLength {
		return invalid("key identifier isn't %d bytes", keyIDLength)
	}
	for _, ext := range c.FutureExtensions {
		if _, err := futureCritical(ext); err != nil {
			return err
		}
	}
	if withSig && len(c.Signature) != signatureLength {
		return invalid("signature of %d bytes", len(c.Signature))
	}
	return nil
}

// extensionOrder returns the tags of the extensions the certificate has, in the order they were
// decoded, then any others in tag order.
func (c *Certificate) extensionOrder() []uint8 {
	var have [tagFutureExtension + 1]int
	if c.BasicConstraints != nil {
		have[tagBasicConstraints] = 1
	}
	if c.KeyUsage != 0 {
		have[tagKeyUsage] = 1
	}
	if c.ExtKeyUsage != nil {
		have[tagExtKeyUsage] = 1
	}
	if c.SubjectKeyID != nil {
		have[tagSubjectKeyID] = 1
	}
	if c.AuthorityKeyID != nil {
		have[tagAuthorityKeyID] = 1
	}
	have[tagFutureExtension] = len(c.FutureExtensions)
	var order []uint8
	for _, n := range c.order {
		if have[n] > 0 {
			order = append(order, n)
			have[n]--
		}
	}
	for n := uint8(tagBasicConstraints); n <= tagFutureExtension; n++ {
		for ; have[n] > 0; have[n]-- {
			order = append(order, n)
		}
	}
	return order
}
