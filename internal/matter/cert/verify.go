package cert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"math/big"
	"slices"
	"time"
)

func parsePublicKey(b []byte) (*ecdsa.PublicKey, error) {
	if len(b) != publicKeyLength {
		return nil, invalid("public key of %d bytes", len(b))
	}
	k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b)
	if err != nil {
		return nil, fmt.Errorf("%w: public key: %w", ErrInvalid, err)
	}
	return k, nil
}

// CheckSignatureFrom checks that parent's key made c's signature.
func (c *Certificate) CheckSignatureFrom(parent *Certificate) error {
	pub, err := parent.ECDSAPublicKey()
	if err != nil {
		return err
	}
	if len(c.Signature) != signatureLength {
		return invalid("signature of %d bytes", len(c.Signature))
	}
	tbs, err := c.tbs()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(tbs)
	r := new(big.Int).SetBytes(c.Signature[:signatureLength/2])
	s := new(big.Int).SetBytes(c.Signature[signatureLength/2:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return ErrSignature
	}
	return nil
}

// Verify checks that noc is a valid node operational certificate issued by rcac, through icac if
// it isn't nil (Core spec, 6.4 and 6.5). It checks each certificate's kind, basic constraints, key
// usage and signature, that their names and key identifiers link up, that the fabric IDs agree,
// and that no certificate has a critical extension Matter doesn't know.
//
// It checks validity periods at now. A zero now skips them, for a node that doesn't know the
// time yet; the caller decides whether that's acceptable.
//
// The trust decision is the caller's: Verify checks the chain up to rcac, not whether rcac is one
// the caller trusts.
func Verify(rcac, icac, noc *Certificate, now time.Time) error {
	if rcac == nil || noc == nil {
		return fmt.Errorf("%w: root or node certificate missing", ErrChain)
	}
	if err := checkRoot(rcac); err != nil {
		return err
	}
	chain := []*Certificate{rcac}
	if icac != nil {
		if err := checkIssued(icac, rcac, KindICA); err != nil {
			return fmt.Errorf("intermediate: %w", err)
		}
		if p := rcac.BasicConstraints.MaxPathLen; p != nil && *p < 1 {
			return fmt.Errorf("%w: root's path length constraint allows no intermediate", ErrChain)
		}
		chain = append(chain, icac)
	}
	if err := checkIssued(noc, chain[len(chain)-1], KindNode); err != nil {
		return fmt.Errorf("node: %w", err)
	}
	chain = append(chain, noc)

	fabric, _ := noc.FabricID()
	for _, c := range chain {
		if id, ok := c.FabricID(); ok && id != fabric {
			return fmt.Errorf("%w: fabric ID %#016x doesn't match the node's %#016x", ErrChain, id, fabric)
		}
		if c.criticalFuture() {
			return fmt.Errorf("%w: certificate has a critical extension Matter doesn't know", ErrChain)
		}
		if err := checkValidity(c, now); err != nil {
			return err
		}
	}
	return nil
}

// checkRoot checks rcac is a self-signed root CA certificate.
func checkRoot(rcac *Certificate) error {
	if err := checkKind(rcac, KindRoot); err != nil {
		return fmt.Errorf("root: %w", err)
	}
	if !rcac.Issuer.Equal(rcac.Subject) || !bytes.Equal(rcac.AuthorityKeyID, rcac.SubjectKeyID) {
		return fmt.Errorf("%w: root isn't self-issued", ErrChain)
	}
	// As connectedhomeip's ValidateChipRCAC: a Matter chain has at most one intermediate.
	if p := rcac.BasicConstraints.MaxPathLen; p != nil && *p > 1 {
		return fmt.Errorf("%w: root's path length constraint is %d, more than 1", ErrChain, *p)
	}
	if err := rcac.CheckSignatureFrom(rcac); err != nil {
		return fmt.Errorf("root: %w", err)
	}
	return nil
}

// checkIssued checks c is a certificate of the given kind that parent issued.
func checkIssued(c, parent *Certificate, kind Kind) error {
	if err := checkKind(c, kind); err != nil {
		return err
	}
	if !c.Issuer.Equal(parent.Subject) {
		return fmt.Errorf("%w: issuer isn't the parent's subject", ErrChain)
	}
	if !bytes.Equal(c.AuthorityKeyID, parent.SubjectKeyID) {
		return fmt.Errorf("%w: authority key ID isn't the parent's subject key ID", ErrChain)
	}
	return c.CheckSignatureFrom(parent)
}

// checkKind checks c's subject, basic constraints, key usage and key identifiers suit kind.
func checkKind(c *Certificate, want Kind) error {
	kind, err := c.Kind()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrChain, err)
	}
	if kind != want {
		return fmt.Errorf("%w: %s certificate, want %s", ErrChain, kind, want)
	}
	if c.BasicConstraints == nil {
		return fmt.Errorf("%w: no basic constraints", ErrChain)
	}
	if len(c.SubjectKeyID) == 0 || len(c.AuthorityKeyID) == 0 {
		return fmt.Errorf("%w: key identifiers missing", ErrChain)
	}
	if want != KindNode {
		if !c.BasicConstraints.IsCA || c.KeyUsage&x509.KeyUsageCertSign == 0 {
			return fmt.Errorf("%w: %s certificate isn't a CA that signs certificates", ErrChain, kind)
		}
		return nil
	}
	if c.BasicConstraints.IsCA {
		return fmt.Errorf("%w: node certificate is a CA", ErrChain)
	}
	if c.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return fmt.Errorf("%w: node certificate can't sign", ErrChain)
	}
	if !slices.Contains(c.ExtKeyUsage, PurposeClientAuth) || !slices.Contains(c.ExtKeyUsage, PurposeServerAuth) {
		return fmt.Errorf("%w: node certificate isn't for client and server authentication", ErrChain)
	}
	cats := c.CATs()
	if len(cats) > MaxCATs {
		return fmt.Errorf("%w: %d CASE Authenticated Tags, more than %d", ErrChain, len(cats), MaxCATs)
	}
	for i, a := range cats {
		for _, b := range cats[:i] {
			if a>>16 == b>>16 {
				return fmt.Errorf("%w: two CASE Authenticated Tags with identifier %#04x", ErrChain, a>>16)
			}
		}
	}
	return nil
}

func checkValidity(c *Certificate, now time.Time) error {
	if now.IsZero() {
		return nil
	}
	if now.Before(c.NotBefore) || !c.NotAfter.IsZero() && now.After(c.NotAfter) {
		return fmt.Errorf("%w: valid from %s to %s, not at %s", ErrExpired, c.NotBefore, c.NotAfter, now)
	}
	return nil
}

// CompressedFabricID returns the compressed fabric identifier for a root public key, in
// uncompressed form, and a fabric ID (Core spec, operational discovery). DNS-SD names a node by
// it.
func CompressedFabricID(rootPublicKey []byte, fabricID uint64) (uint64, error) {
	if _, err := parsePublicKey(rootPublicKey); err != nil {
		return 0, err
	}
	salt := binary.BigEndian.AppendUint64(nil, fabricID)
	out, err := hkdf.Key(sha256.New, rootPublicKey[1:], salt, "CompressedFabric", 8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(out), nil
}
