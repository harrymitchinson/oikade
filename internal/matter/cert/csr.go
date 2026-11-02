package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
)

// NewCSR returns a PKCS#10 certificate signing request, DER encoded, for key, the node's new
// operational key, as a node returns it to CSRRequest (Core spec, Node Operational Credentials cluster). Its subject is
// O=CSR, as connectedhomeip's: the commissioner takes only the key from it.
func NewCSR(key crypto.Signer) ([]byte, error) {
	if pub, ok := key.Public().(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		return nil, invalid("operational key isn't ECDSA P-256")
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:            pkix.Name{Organization: []string{"CSR"}},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	if err != nil {
		return nil, fmt.Errorf("matter: making CSR: %w", err)
	}
	return csr, nil
}

// ParseCSR checks a DER certificate signing request's signature, and returns its P-256 public key,
// for a commissioner to issue a NOC to.
func ParseCSR(der []byte) (*ecdsa.PublicKey, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("%w: CSR: %w", ErrInvalid, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: CSR: %w", ErrSignature, err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, invalid("CSR key isn't ECDSA P-256")
	}
	return pub, nil
}
