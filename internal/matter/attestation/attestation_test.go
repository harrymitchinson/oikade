package attestation

import (
	"crypto/rand"
	"errors"
	"testing"
)

func nonce() []byte {
	n := make([]byte, NonceSize)
	_, _ = rand.Read(n)
	return n
}

// TestTestCredentialsVerify signs attestation elements with the test DAC, and verifies them
// against the test PAA and CD signer.
func TestTestCredentialsVerify(t *testing.T) {
	creds, err := Test()
	if err != nil {
		t.Fatal(err)
	}
	trust, err := TestTrust()
	if err != nil {
		t.Fatal(err)
	}
	n, challenge := nonce(), nonce()[:16]
	elements := AttestationElements(creds.CD, n, 0)
	sig, err := creds.Sign(append(append([]byte(nil), elements...), challenge...))
	if err != nil {
		t.Fatal(err)
	}
	res, err := trust.Verify(creds.DAC, creds.PAI, elements, sig, challenge, n)
	if err != nil {
		t.Fatal(err)
	}
	if res.VendorID != TestVendorID || res.ProductID != TestProductID || res.Declaration.CertificationType != 0 {
		t.Errorf("result %+v, declaration %+v", res, res.Declaration)
	}

	csrNonce := nonce()
	nocsr := NOCSRElements([]byte("a CSR"), csrNonce)
	sig, err = creds.Sign(append(append([]byte(nil), nocsr...), challenge...))
	if err != nil {
		t.Fatal(err)
	}
	csr, err := VerifyNOCSR(res.DAC, nocsr, sig, challenge, csrNonce)
	if err != nil || string(csr) != "a CSR" {
		t.Errorf("VerifyNOCSR: %q, %v", csr, err)
	}

	// What must fail.
	for name, f := range map[string]func() error{
		"another challenge": func() error {
			_, err := trust.Verify(creds.DAC, creds.PAI, elements, sig, nonce()[:16], n)
			return err
		},
		"another nonce": func() error {
			s, _ := creds.Sign(append(append([]byte(nil), elements...), challenge...))
			_, err := trust.Verify(creds.DAC, creds.PAI, elements, s, challenge, nonce())
			return err
		},
		"PAI as DAC": func() error {
			s, _ := creds.Sign(append(append([]byte(nil), elements...), challenge...))
			_, err := trust.Verify(creds.PAI, creds.PAI, elements, s, challenge, n)
			return err
		},
		"untrusted PAA": func() error {
			s, _ := creds.Sign(append(append([]byte(nil), elements...), challenge...))
			_, err := Trust{CDSigners: trust.CDSigners}.Verify(creds.DAC, creds.PAI, elements, s, challenge, n)
			return err
		},
		"untrusted CD signer": func() error {
			s, _ := creds.Sign(append(append([]byte(nil), elements...), challenge...))
			_, err := Trust{PAAs: trust.PAAs}.Verify(creds.DAC, creds.PAI, elements, s, challenge, n)
			return err
		},
		"NOCSR, another nonce": func() error {
			_, err := VerifyNOCSR(res.DAC, nocsr, sig, challenge, nonce())
			return err
		},
	} {
		if err := f(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestCDOtherProduct: a CD that doesn't list the DAC's product fails.
func TestCDOtherProduct(t *testing.T) {
	creds, _ := Test()
	trust, _ := TestTrust()
	key, _ := pemKey("Chip-Test-CD-Signing-Key.pem")
	cd, err := SignCD(Declaration{VendorID: TestVendorID, ProductIDs: []uint16{0x8001}, CertificateID: "ZIG20142ZB330003-24"}, key, trust.CDSigners[0].SubjectKeyId)
	if err != nil {
		t.Fatal(err)
	}
	n, challenge := nonce(), nonce()[:16]
	elements := AttestationElements(cd, n, 0)
	sig, _ := creds.Sign(append(append([]byte(nil), elements...), challenge...))
	if _, err := trust.Verify(creds.DAC, creds.PAI, elements, sig, challenge, n); !errors.Is(err, ErrInvalid) {
		t.Errorf("Verify: %v", err)
	}
}
