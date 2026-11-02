package cert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"time"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// vector decodes the named test certificate from its TLV form.
func vector(t *testing.T, name string) *Certificate {
	t.Helper()
	tc, ok := testCerts[name]
	if !ok {
		t.Fatalf("no test certificate %s", name)
	}
	c, err := Decode(unhex(t, tc.chip))
	if err != nil {
		t.Fatalf("Decode(%s): %v", name, err)
	}
	return c
}

// vectorKey returns the named test certificate's private key.
func vectorKey(t *testing.T, name string) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), unhex(t, testCerts[name].priv))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The vectors were made in October 2020 and expire in October 2040.
var inValidity = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

func TestVectorsRoundTrip(t *testing.T) {
	for name, tc := range testCerts {
		t.Run(name, func(t *testing.T) {
			chip, derCert := unhex(t, tc.chip), unhex(t, tc.der)
			c, err := Decode(chip)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			got, err := c.X509()
			if err != nil {
				t.Fatalf("X509: %v", err)
			}
			if !bytes.Equal(got, derCert) {
				t.Fatalf("TLV → X.509:\n got %x\nwant %x", got, derCert)
			}
			fromDER, err := FromX509(derCert)
			if err != nil {
				t.Fatalf("FromX509: %v", err)
			}
			back, err := fromDER.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !bytes.Equal(back, chip) {
				t.Fatalf("X.509 → TLV:\n got %x\nwant %x", back, chip)
			}

			if !bytes.Equal(c.PublicKey, unhex(t, tc.pub)) {
				t.Errorf("public key %x, want %s", c.PublicKey, tc.pub)
			}
			if !bytes.Equal(c.SubjectKeyID, unhex(t, tc.skid)) {
				t.Errorf("subject key ID %x, want %s", c.SubjectKeyID, tc.skid)
			}
			if !bytes.Equal(c.AuthorityKeyID, unhex(t, tc.akid)) {
				t.Errorf("authority key ID %x, want %s", c.AuthorityKeyID, tc.akid)
			}
			// Go's own parser agrees with the conversion.
			x, err := x509.ParseCertificate(derCert)
			if err != nil {
				t.Fatalf("x509.ParseCertificate: %v", err)
			}
			if !bytes.Equal(x.SubjectKeyId, c.SubjectKeyID) || x.KeyUsage != c.KeyUsage {
				t.Errorf("x509 sees subject key ID %x, key usage %#x", x.SubjectKeyId, x.KeyUsage)
			}
			if !x.NotBefore.Equal(c.NotBefore) {
				t.Errorf("not before %s, x509 says %s", c.NotBefore, x.NotBefore)
			}
		})
	}
}

func TestVectorFields(t *testing.T) {
	noc := vector(t, "Node01_01")
	if id, err := noc.NodeID(); err != nil || id != 0xDEDEDEDE00010001 {
		t.Errorf("NodeID = %#x, %v", id, err)
	}
	if id, ok := noc.FabricID(); !ok || id != 0xFAB000000000001D {
		t.Errorf("FabricID = %#x, %v", id, ok)
	}
	if k, _ := noc.Kind(); k != KindNode {
		t.Errorf("Kind = %s", k)
	}
	if !slices.Equal(noc.ExtKeyUsage, []KeyPurpose{PurposeClientAuth, PurposeServerAuth}) {
		t.Errorf("ExtKeyUsage = %v", noc.ExtKeyUsage)
	}
	want := time.Date(2020, 10, 15, 14, 23, 43, 0, time.UTC)
	if !noc.NotBefore.Equal(want) {
		t.Errorf("NotBefore = %s", noc.NotBefore)
	}

	if cats := vector(t, "Node02_08").CATs(); !slices.Equal(cats, []uint32{0xABCF00A0, 0xABCD0020, 0xABCE0100}) {
		t.Errorf("CATs = %x", cats)
	}
	if k, _ := vector(t, "ICA01").Kind(); k != KindICA {
		t.Errorf("ICA01 Kind = %s", k)
	}
	if k, _ := vector(t, "FWSign01").Kind(); k != KindFirmwareSigning {
		t.Errorf("FWSign01 Kind = %s", k)
	}
	if _, err := vector(t, "Root01").NodeID(); err == nil {
		t.Error("a root has a node ID")
	}
	if !vector(t, "Root03").NotAfter.IsZero() {
		t.Error("Root03 has an expiry")
	}
	if n := len(vector(t, "Node02_06").FutureExtensions); n != 2 {
		t.Errorf("Node02_06 has %d future extensions, want 2", n)
	}
}

func TestVectorChains(t *testing.T) {
	cases := []struct{ root, ica, node string }{
		{"Root01", "ICA01", "Node01_01"},
		{"Root01", "", "Node01_02"},
		{"Root02", "ICA02", "Node02_01"},
		{"Root02", "ICA02", "Node02_02"},
		{"Root02", "ICA02", "Node02_03"},
		{"Root02", "ICA02", "Node02_04"},
		{"Root02", "ICA02", "Node02_05"},
		{"Root02", "ICA02", "Node02_06"},
		{"Root02", "ICA02", "Node02_08"},
	}
	for _, c := range cases {
		t.Run(c.node, func(t *testing.T) {
			var ica *Certificate
			if c.ica != "" {
				ica = vector(t, c.ica)
			}
			if err := Verify(vector(t, c.root), ica, vector(t, c.node), inValidity); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Root03 is Root02's key and name, with no expiry.
	if err := checkRoot(vector(t, "Root03")); err != nil {
		t.Errorf("Root03: %v", err)
	}
	// ICA01_1 is another intermediate under Root01.
	if err := checkIssued(vector(t, "ICA01_1"), vector(t, "Root01"), KindICA); err != nil {
		t.Errorf("ICA01_1: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	root, ica, noc := vector(t, "Root01"), vector(t, "ICA01"), vector(t, "Node01_01")

	tampered := *noc
	tampered.Signature = slices.Clone(noc.Signature)
	tampered.Signature[10] ^= 1
	changed := *noc
	changed.Subject = DN{{Type: AttrNodeID, Value: 0xDEDEDEDE00010002}, noc.Subject[1]}

	// Node01_01 signed again by a key that isn't ICA01's.
	wrongSigner := *noc
	other := vectorKey(t, "ICA02")
	wrongSigner.Signature = nil
	if err := signWith(&wrongSigner, other); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		root, ica, nc *Certificate
		at            time.Time
		want          error
	}{
		{"expired", root, ica, noc, time.Date(2041, 1, 1, 0, 0, 0, 0, time.UTC), ErrExpired},
		{"not yet valid", root, ica, noc, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), ErrExpired},
		{"tampered signature", root, ica, &tampered, inValidity, ErrSignature},
		{"tampered subject", root, ica, &changed, inValidity, ErrSignature},
		{"wrong signer", root, ica, &wrongSigner, inValidity, ErrSignature},
		{"wrong intermediate", root, vector(t, "ICA01_1"), noc, inValidity, ErrChain},
		{"wrong root", vector(t, "Root02"), ica, noc, inValidity, ErrChain},
		{"intermediate missing", root, nil, noc, inValidity, ErrChain},
		{"node as intermediate", root, noc, noc, inValidity, ErrChain},
		{"critical future extension", vector(t, "Root02"), vector(t, "ICA02"), vector(t, "Node02_07"), inValidity, ErrChain},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Verify(c.root, c.ica, c.nc, c.at)
			if !errors.Is(err, c.want) {
				t.Fatalf("Verify = %v, want %v", err, c.want)
			}
		})
	}
}

// signWith signs c with key directly, whoever c says its issuer is.
func signWith(c *Certificate, key *ecdsa.PrivateKey) error {
	saved := *c
	pubBytes, err := key.PublicKey.Bytes()
	if err != nil {
		return err
	}
	fake := &Certificate{Subject: c.Issuer, PublicKey: pubBytes, SubjectKeyID: c.AuthorityKeyID}
	if err := Sign(c, fake, key); err != nil {
		return err
	}
	c.Issuer, c.AuthorityKeyID, c.SerialNumber = saved.Issuer, saved.AuthorityKeyID, saved.SerialNumber
	return nil
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestGeneratedChain(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(10, 0, 0)
	const fabric = 0x1122334455667788
	rootKey, icaKey, nodeKey := newKey(t), newKey(t), newKey(t)

	root, err := NewRCAC(rootKey, 1, fabric, start, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ica, err := NewICAC(root, rootKey, icaKey, 2, 0, start, end)
	if err != nil {
		t.Fatal(err)
	}
	noc, err := NewNOC(ica, icaKey, &nodeKey.PublicKey, NOCSubject{NodeID: 0x42, FabricID: fabric, CATs: []uint32{0x00010001}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := NewNOC(root, rootKey, &nodeKey.PublicKey, NOCSubject{NodeID: 0x43, FabricID: fabric}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	now := start.AddDate(1, 0, 0)
	if err := Verify(root, ica, noc, now); err != nil {
		t.Fatalf("Verify with an intermediate: %v", err)
	}
	if err := Verify(root, nil, direct, now); err != nil {
		t.Fatalf("Verify without one: %v", err)
	}
	if err := Verify(root, nil, direct, end.Add(time.Second)); !errors.Is(err, ErrExpired) {
		t.Fatalf("Verify after expiry = %v", err)
	}

	// Each converts both ways, and Go's X.509 verifier accepts the chain.
	parsed := map[*Certificate]*x509.Certificate{}
	for _, c := range []*Certificate{root, ica, noc} {
		chip, err := c.Encode()
		if err != nil {
			t.Fatal(err)
		}
		d, err := Decode(chip)
		if err != nil {
			t.Fatal(err)
		}
		derCert, err := d.X509()
		if err != nil {
			t.Fatal(err)
		}
		again, err := FromX509(derCert)
		if err != nil {
			t.Fatal(err)
		}
		if chip2, _ := again.Encode(); !bytes.Equal(chip, chip2) {
			t.Fatal("round trip changed the certificate")
		}
		if parsed[c], err = x509.ParseCertificate(derCert); err != nil {
			t.Fatal(err)
		}
	}
	if err := parsed[ica].CheckSignatureFrom(parsed[root]); err != nil {
		t.Errorf("x509: intermediate: %v", err)
	}
	if err := parsed[noc].CheckSignatureFrom(parsed[ica]); err != nil {
		t.Errorf("x509: node: %v", err)
	}
}

func TestGenerateRejects(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rootKey, nodeKey := newKey(t), newKey(t)
	root, err := NewRCAC(rootKey, 1, 0, start, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]NOCSubject{
		"node ID 0":        {NodeID: 0, FabricID: 1},
		"group node ID":    {NodeID: 0xFFFF_FFFF_FFFF_0001, FabricID: 1},
		"fabric ID 0":      {NodeID: 1},
		"CAT version 0":    {NodeID: 1, FabricID: 1, CATs: []uint32{0x00010000}},
		"four CATs":        {NodeID: 1, FabricID: 1, CATs: []uint32{0x10001, 0x20001, 0x30001, 0x40001}},
		"CAT twice":        {NodeID: 1, FabricID: 1, CATs: []uint32{0x10001, 0x10002}},
		"wrong issuer key": {NodeID: 1, FabricID: 1},
	} {
		t.Run(name, func(t *testing.T) {
			key := rootKey
			if name == "wrong issuer key" {
				key = nodeKey
			}
			if _, err := NewNOC(root, key, &nodeKey.PublicKey, s, start, time.Time{}); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrChain) {
				t.Fatalf("NewNOC = %v", err)
			}
		})
	}
	if _, err := NewRCAC(rootKey, 1, 0, time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("NewRCAC before 2000 = %v", err)
	}
}

// TestVerifyNoNodeID checks a certificate with a fabric ID but no node ID isn't accepted as a NOC.
func TestVerifyNoNodeID(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rootKey, nodeKey := newKey(t), newKey(t)
	root, err := NewRCAC(rootKey, 1, 0, start, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := nodeKey.PublicKey.Bytes()
	c := &Certificate{
		NotBefore:        start,
		Subject:          DN{{Type: AttrFabricID, Value: 1}},
		PublicKey:        pub,
		BasicConstraints: &BasicConstraints{},
		KeyUsage:         x509.KeyUsageDigitalSignature,
		ExtKeyUsage:      []KeyPurpose{PurposeClientAuth, PurposeServerAuth},
	}
	if err := Sign(c, root, rootKey); err != nil {
		t.Fatal(err)
	}
	if err := Verify(root, nil, c, start); !errors.Is(err, ErrChain) {
		t.Fatalf("Verify = %v, want ErrChain", err)
	}
	if _, err := c.NodeID(); err == nil {
		t.Fatal("NodeID found one")
	}
}

// TestCompressedFabricID uses the Core spec's example, as connectedhomeip's
// src/crypto/tests/TestChipCryptoPAL.cpp has it.
func TestCompressedFabricID(t *testing.T) {
	root := unhex(t, "044a9f42b1ca4840d37292bbc7f6a7e11e22200c976fc900dbc98a7a383a641cb8254a2e56d4e295a847943b4e3897c4a773e930277b4d9fbede8a052686bfacfa")
	got, err := CompressedFabricID(root, 0x2906C908D115D362)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0x87E1B004E235A130 {
		t.Fatalf("CompressedFabricID = %#016x, want 0x87e1b004e235a130", got)
	}
	if _, err := CompressedFabricID(root[:64], 1); err == nil {
		t.Fatal("accepted a short key")
	}
}

func TestCSR(t *testing.T) {
	key := newKey(t)
	csr, err := NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	req, err := x509.ParseCertificateRequest(csr)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.CheckSignature(); err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey.Equal(req.PublicKey) {
		t.Fatal("CSR has another key")
	}
	pub, err := ParseCSR(csr)
	if err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey.Equal(pub) {
		t.Fatal("ParseCSR returned another key")
	}
	csr[len(csr)-3] ^= 1
	if _, err := ParseCSR(csr); err == nil {
		t.Fatal("ParseCSR accepted a tampered CSR")
	}

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCSR(p384); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewCSR(P-384) = %v", err)
	}
}

func TestDecodeRejects(t *testing.T) {
	chip := unhex(t, testCerts["Node01_01"].chip)
	for n := range len(chip) {
		if _, err := Decode(chip[:n]); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Decode of %d bytes = %v", n, err)
		}
	}
	if _, err := Decode(append(slices.Clone(chip), 0x18)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Decode with trailing data = %v", err)
	}
	// connectedhomeip's Node01_01_Err01: the subject key ID's tag is a common-profile tag.
	bad := slices.Clone(chip)
	i := bytes.Index(bad, unhex(t, "3004146967c912"))
	bad = slices.Concat(bad[:i], []byte{0x50, 0xee, 0x04}, bad[i+2:])
	if _, err := Decode(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Decode(Err01) = %v", err)
	}

	derCert := unhex(t, testCerts["Node01_01"].der)
	for n := range len(derCert) {
		if _, err := FromX509(derCert[:n]); !errors.Is(err, ErrInvalid) {
			t.Fatalf("FromX509 of %d bytes = %v", n, err)
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, tc := range testCerts {
		b, _ := hex.DecodeString(tc.chip)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := Decode(b)
		if err != nil {
			return
		}
		again, err := c.Encode()
		if err != nil {
			t.Fatalf("Encode of a decoded certificate: %v", err)
		}
		if !bytes.Equal(again, b) {
			// TLV allows wider integers than the minimal ones Encode writes; the X.509 form is
			// what matters, and must convert back.
			if _, err := c.X509(); err != nil {
				t.Fatalf("X509 of a decoded certificate: %v", err)
			}
		}
	})
}

func FuzzFromX509(f *testing.F) {
	for _, tc := range testCerts {
		b, _ := hex.DecodeString(tc.der)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := FromX509(b)
		if err != nil {
			return
		}
		chip, err := c.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		d, err := Decode(chip)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if again, _ := d.X509(); !bytes.Equal(again, b) {
			t.Fatal("round trip changed the certificate")
		}
	})
}
