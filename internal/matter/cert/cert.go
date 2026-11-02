// Package cert handles Matter operational certificates (Matter Core Specification, 6.5): the root
// CA certificate (RCAC), an optional intermediate (ICAC) and the node operational certificate
// (NOC).
//
// Matter sends certificates in a compact TLV form, but each one is an X.509 certificate underneath:
// its signature is over the X.509 DER encoding of its TBSCertificate. Decode and Encode read and
// write the TLV form, and X509 and FromX509 convert between the two exactly, byte for byte, as
// connectedhomeip does.
//
// Verify checks an RCAC → [ICAC] → NOC chain. NewRCAC, NewICAC and NewNOC make certificates, and
// NewCSR builds the certificate signing request a node returns to CSRRequest.
package cert

import (
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"time"
)

// Errors. Every error this package returns wraps one of them.
var (
	// ErrInvalid is returned for a certificate that is malformed, or outside Matter's profile.
	ErrInvalid = errors.New("matter: invalid certificate")
	// ErrExpired is returned for a certificate used outside its validity period.
	ErrExpired = errors.New("matter: certificate expired or not yet valid")
	// ErrSignature is returned when a certificate's signature doesn't verify.
	ErrSignature = errors.New("matter: certificate signature invalid")
	// ErrChain is returned for a chain whose certificates don't fit together as Matter requires.
	ErrChain = errors.New("matter: invalid certificate chain")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Certificate is a Matter certificate. A nil or zero extension field is an extension the
// certificate doesn't have.
type Certificate struct {
	// SerialNumber is the contents of the DER INTEGER, as the TLV form holds it.
	SerialNumber []byte
	Issuer       DN
	// NotBefore and NotAfter bound the validity period, to the second. A zero NotAfter means the
	// certificate has no expiry (X.509's 99991231235959Z, Matter's epoch time 0).
	NotBefore time.Time
	NotAfter  time.Time
	Subject   DN
	// PublicKey is the subject's P-256 key in uncompressed form: 0x04, X, Y.
	PublicKey []byte

	BasicConstraints *BasicConstraints
	KeyUsage         x509.KeyUsage
	ExtKeyUsage      []KeyPurpose
	SubjectKeyID     []byte
	AuthorityKeyID   []byte
	// FutureExtensions are any other X.509 extensions, each a whole DER Extension SEQUENCE.
	FutureExtensions [][]byte

	// Signature is the ECDSA P-256 signature over the TBSCertificate's DER encoding, as r then s,
	// 32 bytes each.
	Signature []byte

	// order is the extension tags in the order they were decoded. The signature covers the
	// extensions in that order, so encoding keeps it.
	order []uint8
}

// BasicConstraints is the X.509 basic constraints extension.
type BasicConstraints struct {
	IsCA bool
	// MaxPathLen is the path length constraint, or nil for none.
	MaxPathLen *uint8
}

// KeyPurpose is an extended key usage, numbered as in Matter's TLV form.
type KeyPurpose uint8

// Key purposes.
const (
	PurposeServerAuth KeyPurpose = 1 + iota
	PurposeClientAuth
	PurposeCodeSigning
	PurposeEmailProtection
	PurposeTimeStamping
	PurposeOCSPSigning
)

// Kind is what a certificate is for, from the Matter attributes in its subject.
type Kind uint8

// Certificate kinds.
const (
	KindUnknown Kind = iota
	KindRoot
	KindICA
	KindNode
	KindFirmwareSigning
	KindVIDVerificationSigner
)

func (k Kind) String() string {
	switch k {
	case KindRoot:
		return "root"
	case KindICA:
		return "intermediate"
	case KindNode:
		return "node"
	case KindFirmwareSigning:
		return "firmware signing"
	case KindVIDVerificationSigner:
		return "VID verification signer"
	}
	return "unknown"
}

// Limits on Matter identifiers (Core spec, 2.5).
const (
	maxOperationalNodeID = 0xFFFF_FFEF_FFFF_FFFF
	// MaxCATs is how many CASE Authenticated Tags a NOC may carry.
	MaxCATs = 3
)

// Kind returns what the certificate is for. It returns an error wrapping ErrInvalid if the subject
// mixes kinds or carries an invalid Matter attribute, as connectedhomeip's GetCertType does.
func (c *Certificate) Kind() (Kind, error) {
	kind := KindUnknown
	set := func(k Kind) error {
		if kind != KindUnknown {
			return invalid("subject is both %s and %s", kind, k)
		}
		kind = k
		return nil
	}
	fabric, cats := false, 0
	for _, a := range c.Subject {
		var err error
		switch a.Type {
		case AttrRCACID:
			err = set(KindRoot)
		case AttrICACID:
			err = set(KindICA)
		case AttrVIDVerificationSignerID:
			err = set(KindVIDVerificationSigner)
		case AttrFirmwareSigningID:
			err = set(KindFirmwareSigning)
		case AttrNodeID:
			if a.Value == 0 || a.Value > maxOperationalNodeID {
				return KindUnknown, invalid("node ID %#016x isn't an operational node ID", a.Value)
			}
			err = set(KindNode)
		case AttrFabricID:
			if fabric {
				return KindUnknown, invalid("subject has two fabric IDs")
			}
			if a.Value == 0 {
				return KindUnknown, invalid("fabric ID 0")
			}
			fabric = true
		case AttrCASEAuthTag:
			if a.Value&0xFFFF == 0 {
				return KindUnknown, invalid("CASE Authenticated Tag %#08x has version 0", a.Value)
			}
			cats++
		}
		if err != nil {
			return KindUnknown, err
		}
	}
	switch {
	case kind == KindNode && !fabric:
		return KindUnknown, invalid("node certificate has no fabric ID")
	case kind != KindNode && cats > 0:
		return KindUnknown, invalid("%s certificate has CASE Authenticated Tags", kind)
	case kind == KindVIDVerificationSigner && fabric:
		return KindUnknown, invalid("VID verification signer certificate has a fabric ID")
	}
	return kind, nil
}

// NodeID returns the node ID of a NOC. It returns an error if the certificate isn't a valid NOC.
func (c *Certificate) NodeID() (uint64, error) {
	kind, err := c.Kind()
	if err != nil {
		return 0, err
	}
	if kind != KindNode {
		return 0, invalid("%s certificate has no node ID", kind)
	}
	id, _ := c.Subject.Get(AttrNodeID)
	return id, nil
}

// FabricID returns the fabric ID in the subject, and false if it has none. A NOC always has one;
// an RCAC or ICAC may.
func (c *Certificate) FabricID() (uint64, bool) { return c.Subject.Get(AttrFabricID) }

// CATs returns the CASE Authenticated Tags in the subject, in order.
func (c *Certificate) CATs() []uint32 {
	var cats []uint32
	for _, a := range c.Subject {
		if a.Type == AttrCASEAuthTag {
			cats = append(cats, uint32(a.Value))
		}
	}
	return cats
}

// ECDSAPublicKey returns the subject's public key.
func (c *Certificate) ECDSAPublicKey() (*ecdsa.PublicKey, error) { return parsePublicKey(c.PublicKey) }

// criticalFuture reports whether the certificate has a future extension marked critical, which
// Matter doesn't allow in a chain it validates.
func (c *Certificate) criticalFuture() bool {
	for _, ext := range c.FutureExtensions {
		if critical, err := futureCritical(ext); err != nil || critical {
			return true
		}
	}
	return false
}
