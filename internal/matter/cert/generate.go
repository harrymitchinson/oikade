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
// RFC 5280 suggests and connectedhomeip does. If Sign fails, c is unchanged.
//
// If issuer is nil, c is self-signed: its issuer is its own subject, and key must be its own key.
// Otherwise key must be issuer's key.
func Sign(c, issuer *Certificate, key crypto.Signer) error {
	pubBytes, err := signerKey(key)
	if err != nil {
		return err
	}
	d := *c // filled in, and copied back only on success
	if len(d.SubjectKeyID) == 0 {
		d.SubjectKeyID = keyID(d.PublicKey)
	}
	if issuer == nil {
		issuer = &d
	}
	if !bytes.Equal(pubBytes, issuer.PublicKey) {
		return invalid("signing key isn't the issuer's")
	}
	if len(issuer.SubjectKeyID) == 0 {
		return invalid("issuer has no subject key ID")
	}
	d.Issuer = append(DN(nil), issuer.Subject...)
	d.AuthorityKeyID = bytes.Clone(issuer.SubjectKeyID)
	if len(d.SerialNumber) == 0 {
		if d.SerialNumber, err = randomSerial(); err != nil {
			return err
		}
	}
	tbs, err := d.tbs()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(tbs)
	sig, err := key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return fmt.Errorf("matter: signing certificate: %w", err)
	}
	var rs struct{ R, S *big.Int }
	if rest, err := asn1.Unmarshal(sig, &rs); err != nil || len(rest) > 0 ||
		rs.R.Sign() <= 0 || rs.S.Sign() <= 0 || rs.R.BitLen() > 256 || rs.S.BitLen() > 256 {
		return fmt.Errorf("matter: signer returned a malformed ECDSA P-256 signature")
	}
	d.Signature = make([]byte, signatureLength)
	rs.R.FillBytes(d.Signature[:signatureLength/2])
	rs.S.FillBytes(d.Signature[signatureLength/2:])
	*c = d
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

// publicKeyBytes returns a P-256 public key in uncompressed form.
func publicKeyBytes(pub *ecdsa.PublicKey) ([]byte, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, invalid("key isn't ECDSA P-256")
	}
	b, err := pub.Bytes()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return b, nil
}

// signerKey returns a signer's P-256 public key in uncompressed form.
func signerKey(key crypto.Signer) ([]byte, error) {
	pub, _ := key.Public().(*ecdsa.PublicKey)
	return publicKeyBytes(pub)
}

func caCertificate(subject DN, key crypto.Signer, notBefore, notAfter time.Time) (*Certificate, error) {
	if notBefore.IsZero() {
		return nil, invalid("no notBefore")
	}
	pubBytes, err := signerKey(key)
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
// 0, fabric ID. notBefore must be set; a zero notAfter means no expiry.
func NewRCAC(key crypto.Signer, rcacID, fabricID uint64, notBefore, notAfter time.Time) (*Certificate, error) {
	c, err := caCertificate(withFabric(DN{{Type: AttrRCACID, Value: rcacID}}, fabricID), key, notBefore, notAfter)
	if err != nil {
		return nil, err
	}
	if err := Sign(c, nil, key); err != nil {
		return nil, err
	}
	if err := checkRoot(c); err != nil {
		return nil, err
	}
	return c, nil
}

// NewICAC makes an intermediate CA certificate for the key icacKey, issued by root and signed with
// rootKey, with the given ICAC ID and, unless it is 0, fabric ID. notBefore must be set; a zero
// notAfter means no expiry.
func NewICAC(root *Certificate, rootKey, icacKey crypto.Signer, icacID, fabricID uint64, notBefore, notAfter time.Time) (*Certificate, error) {
	c, err := caCertificate(withFabric(DN{{Type: AttrICACID, Value: icacID}}, fabricID), icacKey, notBefore, notAfter)
	if err != nil {
		return nil, err
	}
	if err := Sign(c, root, rootKey); err != nil {
		return nil, err
	}
	if err := checkIssued(c, root, KindICA); err != nil {
		return nil, err
	}
	if rootFabric, ok := root.FabricID(); ok && fabricID != 0 && fabricID != rootFabric {
		return nil, fmt.Errorf("%w: fabric ID %#016x isn't the root's %#016x", ErrChain, fabricID, rootFabric)
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
// or ICAC) and signed with issuerKey. notBefore must be set; a zero notAfter means no expiry.
func NewNOC(issuer *Certificate, issuerKey crypto.Signer, pub *ecdsa.PublicKey, subject NOCSubject, notBefore, notAfter time.Time) (*Certificate, error) {
	if notBefore.IsZero() {
		return nil, invalid("no notBefore")
	}
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
	if id, ok := issuer.FabricID(); ok && id != subject.FabricID {
		return nil, fmt.Errorf("%w: fabric ID %#016x isn't the issuer's %#016x", ErrChain, subject.FabricID, id)
	}
	return c, nil
}
