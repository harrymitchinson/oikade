package attestation

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"fmt"
	"io"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// Trust is what a commissioner trusts to verify a node's attestation.
type Trust struct {
	PAAs      []*x509.Certificate
	CDSigners []*x509.Certificate
	// Now is when the certificates must be valid. Zero means the current time.
	Now time.Time
}

// TestTrust trusts connectedhomeip's test PAA and CD signer, which Test's credentials chain to.
func TestTrust() (Trust, error) {
	paa, err := TestPAA()
	if err != nil {
		return Trust{}, err
	}
	cd, err := TestCDSigner()
	if err != nil {
		return Trust{}, err
	}
	return Trust{PAAs: []*x509.Certificate{paa}, CDSigners: []*x509.Certificate{cd}}, nil
}

// Result is what a verified attestation says of the node.
type Result struct {
	VendorID, ProductID uint16
	DAC                 *x509.Certificate
	Declaration         *Declaration
}

// Verify checks a node's AttestationResponse, as a commissioner does (Core spec, 6.2.3.1): the
// DAC chains through the PAI to a trusted PAA with consistent vendor and product IDs, the
// elements carry the commissioner's nonce and a CD that a trusted signer signed for the DAC's
// product, and the signature is the DAC key's over the elements and the session's challenge.
func (t Trust) Verify(dacDER, paiDER, elements, signature, challenge, nonce []byte) (*Result, error) {
	dac, err := x509.ParseCertificate(dacDER)
	if err != nil {
		return nil, fmt.Errorf("%w: DAC: %w", ErrInvalid, err)
	}
	pai, err := x509.ParseCertificate(paiDER)
	if err != nil {
		return nil, fmt.Errorf("%w: PAI: %w", ErrInvalid, err)
	}
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	for _, p := range t.PAAs {
		roots.AddCert(p)
	}
	inter.AddCert(pai)
	chains, err := dac.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, CurrentTime: t.Now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: DAC chain: %w", ErrInvalid, err)
	}
	if len(chains[0]) != 3 || !chains[0][1].Equal(pai) {
		return nil, fmt.Errorf("%w: DAC chain isn't DAC, PAI, PAA", ErrInvalid)
	}
	vid, pid, hasVID, hasPID := ids(dac)
	if !hasVID || !hasPID {
		return nil, fmt.Errorf("%w: DAC has no vendor or product ID", ErrInvalid)
	}
	for _, c := range chains[0][1:] {
		v, p, hv, hp := ids(c)
		if (hv && v != vid) || (hp && p != pid) {
			return nil, fmt.Errorf("%w: %s's vendor or product ID doesn't match the DAC's", ErrInvalid, c.Subject)
		}
	}
	dacKey, ok := dac.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: DAC key isn't ECDSA", ErrInvalid)
	}
	if !VerifySignature(dacKey, append(append([]byte(nil), elements...), challenge...), signature) {
		return nil, fmt.Errorf("%w: attestation signature", ErrInvalid)
	}
	f, err := structFields(elements, 1, 2)
	if err != nil {
		return nil, fmt.Errorf("%w: attestation elements: %w", ErrInvalid, err)
	}
	if !bytes.Equal(f[2], nonce) {
		return nil, fmt.Errorf("%w: attestation nonce doesn't match", ErrInvalid)
	}
	signers := map[string]*ecdsa.PublicKey{}
	for _, c := range t.CDSigners {
		if k, ok := c.PublicKey.(*ecdsa.PublicKey); ok {
			signers[string(c.SubjectKeyId)] = k
		}
	}
	cd, err := ParseCD(f[1], signers)
	if err != nil {
		return nil, err
	}
	if !cd.covers(vid, pid) {
		return nil, fmt.Errorf("%w: CD doesn't cover vendor %#04x product %#04x", ErrInvalid, vid, pid)
	}
	return &Result{VendorID: vid, ProductID: pid, DAC: dac, Declaration: cd}, nil
}

// VerifyNOCSR checks a node's CSRResponse against its DAC, from Verify, and returns the CSR.
func VerifyNOCSR(dac *x509.Certificate, elements, signature, challenge, nonce []byte) ([]byte, error) {
	key, ok := dac.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: DAC key isn't ECDSA", ErrInvalid)
	}
	if !VerifySignature(key, append(append([]byte(nil), elements...), challenge...), signature) {
		return nil, fmt.Errorf("%w: NOCSR signature", ErrInvalid)
	}
	f, err := structFields(elements, 1, 2)
	if err != nil {
		return nil, fmt.Errorf("%w: NOCSR elements: %w", ErrInvalid, err)
	}
	if !bytes.Equal(f[2], nonce) {
		return nil, fmt.Errorf("%w: CSR nonce doesn't match", ErrInvalid)
	}
	return f[1], nil
}

// structFields reads the octet string fields of a TLV structure, which must have those tagged
// want.
func structFields(b []byte, want ...uint32) (map[uint32][]byte, error) {
	r := tlv.NewReader(b)
	if err := r.Next(); err != nil || r.Type() != tlv.Structure {
		return nil, fmt.Errorf("not a structure")
	}
	if err := r.Enter(); err != nil {
		return nil, err
	}
	out := map[uint32][]byte{}
	for {
		err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if r.Tag().Kind == tlv.ContextTag && r.Type() == tlv.Bytes {
			v, _ := r.Bytes()
			out[r.Tag().Number] = v
		}
	}
	for _, t := range want {
		if out[t] == nil {
			return nil, fmt.Errorf("no field %d", t)
		}
	}
	return out, nil
}
