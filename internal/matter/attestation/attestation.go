// Package attestation is Matter device attestation (Matter Core Specification, 6.2): the
// Device Attestation Certificate (DAC) and its Product Attestation Intermediate (PAI), the
// Certification Declaration (CD), and the attestation and NOCSR elements a node signs with its
// DAC key.
//
// Test returns connectedhomeip's published test credentials, which chain to its test PAA, with a
// CD signed by its test CD key. A commissioner reports a node attested with them as uncertified.
package attestation

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

//go:embed testcreds/*.pem
var testcreds embed.FS

// Test vendor and product IDs: connectedhomeip's test DAC is for these.
const (
	TestVendorID  uint16 = 0xFFF1
	TestProductID uint16 = 0x8000
)

// NonceSize is the size of the attestation and CSR nonces.
const NonceSize = 32

// SignatureSize is the size of a Matter ECDSA P-256 signature: r then s.
const SignatureSize = 64

// ErrInvalid wraps every error from verifying attestation.
var ErrInvalid = errors.New("matter: invalid device attestation")

// Credentials are what a node attests with.
type Credentials struct {
	DAC, PAI []byte // X.509 DER
	Key      crypto.Signer
	CD       []byte // the Certification Declaration, CMS
}

// Sign signs message with the DAC key, as a Matter signature: r then s.
func (c *Credentials) Sign(message []byte) ([]byte, error) {
	return Sign(c.Key, message)
}

// Sign signs message's SHA-256 with key, a P-256 key, and returns r then s.
func Sign(key crypto.Signer, message []byte) ([]byte, error) {
	digest := sha256.Sum256(message)
	der, err := key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return nil, err
	}
	var sig struct{ R, S *big.Int }
	if _, err := asn1Unmarshal(der, &sig); err != nil {
		return nil, err
	}
	out := make([]byte, SignatureSize)
	sig.R.FillBytes(out[:32])
	sig.S.FillBytes(out[32:])
	return out, nil
}

// VerifySignature checks a Matter signature, r then s, of message by pub.
func VerifySignature(pub *ecdsa.PublicKey, message, sig []byte) bool {
	if len(sig) != SignatureSize {
		return false
	}
	digest := sha256.Sum256(message)
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, digest[:], r, s)
}

// AttestationElements encodes the attestation elements a node returns to AttestationRequest.
// timestamp is 0 for a node without a clock.
func AttestationElements(cd, nonce []byte, timestamp uint32) []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), cd)
	w.PutBytes(tlv.Context(2), nonce)
	w.PutUint(tlv.Context(3), uint64(timestamp))
	w.End()
	return w.Bytes()
}

// NOCSRElements encodes the NOCSR elements a node returns to CSRRequest.
func NOCSRElements(csr, nonce []byte) []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), csr)
	w.PutBytes(tlv.Context(2), nonce)
	w.End()
	return w.Bytes()
}

var test = sync.OnceValues(func() (*Credentials, error) {
	dac, err := pemBlock("Chip-Test-DAC-FFF1-8000-0000-Cert.pem")
	if err != nil {
		return nil, err
	}
	pai, err := pemBlock("Chip-Test-PAI-FFF1-8000-Cert.pem")
	if err != nil {
		return nil, err
	}
	key, err := pemKey("Chip-Test-DAC-FFF1-8000-0000-Key.pem")
	if err != nil {
		return nil, err
	}
	cdKey, err := pemKey("Chip-Test-CD-Signing-Key.pem")
	if err != nil {
		return nil, err
	}
	cdCert, err := TestCDSigner()
	if err != nil {
		return nil, err
	}
	cd, err := SignCD(Declaration{
		VendorID: TestVendorID, ProductIDs: []uint16{TestProductID},
		DeviceTypeID: 0x0016, CertificateID: "ZIG20142ZB330003-24", VersionNumber: 1,
	}, cdKey, cdCert.SubjectKeyId)
	if err != nil {
		return nil, err
	}
	return &Credentials{DAC: dac, PAI: pai, Key: key, CD: cd}, nil
})

// Test returns connectedhomeip's test DAC and PAI for vendor 0xFFF1 and product 0x8000, and a CD
// for them signed with its test CD key.
func Test() (*Credentials, error) { return test() }

// TestPAA returns connectedhomeip's test PAA for vendor 0xFFF1, which the test PAI chains to.
func TestPAA() (*x509.Certificate, error) { return pemCert("Chip-Test-PAA-FFF1-Cert.pem") }

// TestCDSigner returns connectedhomeip's test CD signing certificate.
func TestCDSigner() (*x509.Certificate, error) { return pemCert("Chip-Test-CD-Signing-Cert.pem") }

func pemBlock(name string) ([]byte, error) {
	b, err := testcreds.ReadFile("testcreds/" + name)
	if err != nil {
		return nil, err
	}
	p, _ := pem.Decode(b)
	if p == nil {
		return nil, fmt.Errorf("matter: %s isn't PEM", name)
	}
	return p.Bytes, nil
}

func pemCert(name string) (*x509.Certificate, error) {
	b, err := pemBlock(name)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(b)
}

func pemKey(name string) (*ecdsa.PrivateKey, error) {
	b, err := pemBlock(name)
	if err != nil {
		return nil, err
	}
	return x509.ParseECPrivateKey(b)
}

// Matter's X.509 attributes for vendor and product IDs, held as four hex digits.
var (
	oidVendorID  = []int{1, 3, 6, 1, 4, 1, 37244, 2, 1}
	oidProductID = []int{1, 3, 6, 1, 4, 1, 37244, 2, 2}
)

// ids returns the vendor and product IDs in an attestation certificate's subject.
func ids(c *x509.Certificate) (vid, pid uint16, hasVID, hasPID bool) {
	for _, n := range c.Subject.Names {
		s, ok := n.Value.(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(s, 16, 16)
		if err != nil || len(s) != 4 {
			continue
		}
		switch {
		case n.Type.Equal(oidVendorID):
			vid, hasVID = uint16(v), true
		case n.Type.Equal(oidProductID):
			pid, hasPID = uint16(v), true
		}
	}
	return vid, pid, hasVID, hasPID
}
