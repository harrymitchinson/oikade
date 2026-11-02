package cert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 5280's key identifier method, not a security property
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"math/big"
	"time"
)

// Sign fills in c's issuer, key identifiers and serial number, and signs it with key. An empty
// serial number becomes a random one, and an empty subject key ID the SHA-1 of the public key, as
// RFC 5280 suggests and connectedhomeip does.
//
// If issuer is nil, c is self-signed: its issuer is its own subject, and key must be its own key.
// Otherwise key must be issuer's key.
func Sign(c, issuer *Certificate, key crypto.Signer) error {
	pub, ok := key.Public().(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return invalid("signing key isn't ECDSA P-256")
	}
	pubBytes, err := pub.Bytes()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(c.SubjectKeyID) == 0 {
		c.SubjectKeyID = keyID(c.PublicKey)
	}
	if issuer == nil {
		issuer = c
	}
	if !bytes.Equal(pubBytes, issuer.PublicKey) {
		return invalid("signing key isn't the issuer's")
	}
	c.Issuer = append(DN(nil), issuer.Subject...)
	c.AuthorityKeyID = bytes.Clone(issuer.SubjectKeyID)
	if len(c.SerialNumber) == 0 {
		if c.SerialNumber, err = randomSerial(); err != nil {
			return err
		}
	}
	tbs, err := c.tbs()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(tbs)
	sig, err := key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return fmt.Errorf("matter: signing certificate: %w", err)
	}
	var rs struct{ R, S *big.Int }
	if rest, err := asn1.Unmarshal(sig, &rs); err != nil || len(rest) > 0 {
		return fmt.Errorf("matter: signer returned a malformed ECDSA signature")
	}
	c.Signature = make([]byte, signatureLength)
	rs.R.FillBytes(c.Signature[:signatureLength/2])
	rs.S.FillBytes(c.Signature[signatureLength/2:])
	return nil
}

func keyID(pub []byte) []byte {
	sum := sha1.Sum(pub) //nolint:gosec // see the import
	return sum[:]
}

// randomSerial returns a random positive 8-byte serial number with no leading zero byte.
func randomSerial() ([]byte, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	b[0] = b[0]&0x7F | 0x40
	return b, nil
}

func publicKeyBytes(pub *ecdsa.PublicKey) ([]byte, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, invalid("public key isn't ECDSA P-256")
	}
	b, err := pub.Bytes()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return b, nil
}

func caCertificate(subject DN, key crypto.Signer, notBefore, notAfter time.Time) (*Certificate, error) {
	pub, ok := key.Public().(*ecdsa.PublicKey)
	if !ok {
		return nil, invalid("key isn't ECDSA")
	}
	pubBytes, err := publicKeyBytes(pub)
	if err != nil {
		return nil, err
	}
	return &Certificate{
		NotBefore:        notBefore,
		NotAfter:         notAfter,
		Subject:          subject,
		PublicKey:        pubBytes,
		BasicConstraints: &BasicConstraints{IsCA: true},
		KeyUsage:         x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, nil
}

// withFabric returns dn, with the fabric ID appended if it isn't 0.
func withFabric(dn DN, fabricID uint64) DN {
	if fabricID != 0 {
		dn = append(dn, Attribute{Type: AttrFabricID, Value: fabricID})
	}
	return dn
}

// NewRCAC makes a self-signed root CA certificate for key, with the given RCAC ID and, unless it is
// 0, fabric ID. A zero notAfter means no expiry.
func NewRCAC(key crypto.Signer, rcacID, fabricID uint64, notBefore, notAfter time.Time) (*Certificate, error) {
	c, err := caCertificate(withFabric(DN{{Type: AttrRCACID, Value: rcacID}}, fabricID), key, notBefore, notAfter)
	if err != nil {
		return nil, err
	}
	if err := Sign(c, nil, key); err != nil {
		return nil, err
	}
	return c, nil
}

// NewICAC makes an intermediate CA certificate for the key icacKey, issued by root and signed with
// rootKey, with the given ICAC ID and, unless it is 0, fabric ID. A zero notAfter means no
// expiry.
func NewICAC(root *Certificate, rootKey, icacKey crypto.Signer, icacID, fabricID uint64, notBefore, notAfter time.Time) (*Certificate, error) {
	c, err := caCertificate(withFabric(DN{{Type: AttrICACID, Value: icacID}}, fabricID), icacKey, notBefore, notAfter)
	if err != nil {
		return nil, err
	}
	if err := Sign(c, root, rootKey); err != nil {
		return nil, err
	}
	return c, nil
}

// NOCSubject is who a node operational certificate is for.
type NOCSubject struct {
	NodeID   uint64
	FabricID uint64
	// CATs are the node's CASE Authenticated Tags, at most MaxCATs.
	CATs []uint32
}

// NewNOC makes a node operational certificate for the public key pub, issued by issuer (the RCAC
// or ICAC) and signed with issuerKey. A zero notAfter means no expiry.
func NewNOC(issuer *Certificate, issuerKey crypto.Signer, pub *ecdsa.PublicKey, subject NOCSubject, notBefore, notAfter time.Time) (*Certificate, error) {
	pubBytes, err := publicKeyBytes(pub)
	if err != nil {
		return nil, err
	}
	dn := DN{{Type: AttrNodeID, Value: subject.NodeID}, {Type: AttrFabricID, Value: subject.FabricID}}
	for _, cat := range subject.CATs {
		dn = append(dn, Attribute{Type: AttrCASEAuthTag, Value: uint64(cat)})
	}
	c := &Certificate{
		NotBefore:        notBefore,
		NotAfter:         notAfter,
		Subject:          dn,
		PublicKey:        pubBytes,
		BasicConstraints: &BasicConstraints{},
		KeyUsage:         x509.KeyUsageDigitalSignature,
		ExtKeyUsage:      []KeyPurpose{PurposeClientAuth, PurposeServerAuth},
	}
	if err := Sign(c, issuer, issuerKey); err != nil {
		return nil, err
	}
	if err := checkKind(c, KindNode); err != nil {
		return nil, err
	}
	return c, nil
}
