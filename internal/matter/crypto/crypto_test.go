package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.NewReplacer(" ", "", "\n", "", "\t", "").Replace(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCCMVectors uses connectedhomeip's AES-CCM-128 vectors with Matter's 13-byte nonce and
// 16-byte tag (src/crypto/tests/AES_CCM_128_test_vectors.h).
func TestCCMVectors(t *testing.T) {
	cases := []struct{ name, key, nonce, ad, pt, ct, tag string }{
		{
			name:  "chiptest_dac9e1195a0d_test_vector_270",
			key:   "404142434445464748494a4b4c4d4e4f",
			nonce: "101112131415161718191A1B1C",
			tag:   "32d6f8243a26d0bd98d01b0f448e7773",
		},
		{
			name:  "aesccm128_matter_2ef53070ae20_test_vector_0",
			key:   "0953fa93e7caac9638f58820220a398e",
			nonce: "00800000011201000012345678",
			pt:    "fffd034b50057e400000010000",
			ct:    "b5e5bfdacbaf6cb7fb6bff871f",
			tag:   "b0d6dd827d35bf372fa6425dcd17d356",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			aead, err := NewCCM(unhex(t, c.key), TagSize, NonceSize)
			if err != nil {
				t.Fatal(err)
			}
			nonce, ad, pt := unhex(t, c.nonce), unhex(t, c.ad), unhex(t, c.pt)
			want := append(unhex(t, c.ct), unhex(t, c.tag)...)
			got := aead.Seal(nil, nonce, pt, ad)
			if !bytes.Equal(got, want) {
				t.Fatalf("Seal = % x, want % x", got, want)
			}
			back, err := aead.Open(nil, nonce, got, ad)
			if err != nil || !bytes.Equal(back, pt) {
				t.Fatalf("Open = % x, %v", back, err)
			}
			got[len(got)-1] ^= 1
			if _, err := aead.Open(nil, nonce, got, ad); err == nil {
				t.Error("Open accepted a tampered tag")
			}
		})
	}
}

// TestCCMRFC3610 is RFC 3610's Packet Vector #1: an 8-byte tag and 8 bytes of additional data.
func TestCCMRFC3610(t *testing.T) {
	aead, err := NewCCM(unhex(t, "C0C1C2C3C4C5C6C7C8C9CACBCCCDCECF"), 8, 13)
	if err != nil {
		t.Fatal(err)
	}
	in := unhex(t, "0001020304050607 08090A0B0C0D0E0F101112131415161718191A1B1C1D1E")
	got := aead.Seal(nil, unhex(t, "00000003020100A0A1A2A3A4A5"), in[8:], in[:8])
	want := unhex(t, "588C979A61C663D2F066D0C2C0F989806D5F6B61DAC384 17E8D12CFDF926E0")
	if !bytes.Equal(got, want) {
		t.Errorf("Seal = % X, want % X", got, want)
	}
}

func TestCCMRoundTripLengths(t *testing.T) {
	aead, _ := NewCCM(make([]byte, 16), TagSize, NonceSize)
	nonce := make([]byte, NonceSize)
	for _, n := range []int{0, 1, 15, 16, 17, 31, 32, 33, 1200} {
		for _, adLen := range []int{0, 1, 14, 16, 30} {
			pt, ad := bytes.Repeat([]byte{0xAB}, n), bytes.Repeat([]byte{0xCD}, adLen)
			sealed := aead.Seal([]byte("prefix"), nonce, pt, ad)
			got, err := aead.Open(nil, nonce, sealed[6:], ad)
			if err != nil || !bytes.Equal(got, pt) {
				t.Errorf("%d bytes, %d of AD: %v", n, adLen, err)
			}
			if adLen > 0 {
				if _, err := aead.Open(nil, nonce, sealed[6:], ad[1:]); err == nil {
					t.Errorf("%d bytes: Open accepted different AD", n)
				}
			}
		}
	}
}

// TestSpake2pVector is connectedhomeip's first SPAKE2+ vector, from
// draft-bar-cfrg-spake2plus-01 (src/crypto/tests/SPAKE2P_RFC_test_vectors.h, test_vector_16).
func TestSpake2pVector(t *testing.T) {
	tr := Transcript{
		Context:    []byte("SPAKE2+-P256-SHA256-HKDF draft-01"),
		IDProver:   []byte("client"),
		IDVerifier: []byte("server"),
	}
	w0 := unhex(t, "e6887cf9bdfb7579c69bf47928a84514b5e355ac034863f7ffaf4390e67d798c")
	w1 := unhex(t, "24b5ae4abda868ec9336ffc3b78ee31c5755bef1759227ef5372ca139b94e512")
	l := unhex(t, `0495645cfb74df6e58f9748bb83a86620bab7c82e107f57d6870da8cbcb2ff9f70
		63a14b6402c62f99afcb9706a4d1a143273259fe76f1c605a3639745a92154b9`)
	x := unhex(t, "8b0f3f383905cf3a3bb955ef8fb62e24849dd349a05ca79aafb18041d30cbdb6")
	wantX := unhex(t, `04af09987a593d3bac8694b123839422c3cc87e37d6b41c1d630f000dd64980e53
		7ae704bcede04ea3bec9b7475b32fa2ca3b684be14d11645e38ea6609eb39e7e`)
	y := unhex(t, "2e0895b0e763d6d5a9564433e64ac3cac74ff897f6c3445247ba1bab40082a91")
	wantY := unhex(t, `04417592620aebf9fd203616bbb9f121b730c258b286f890c5f19fea833a9c900c
		be9057bc549a3e19975be9927f0e7614f08d1f0a108eede5fd7eb5624584a4f4`)
	want := Keys{
		Ka:  unhex(t, "f9cab9adcc0ed8e5a4db11a8505914b2"),
		Ke:  unhex(t, "801db297654816eb4f02868129b9dc89"),
		KcA: unhex(t, "0d248d7d19234f1486b2efba5179c52d"),
		KcB: unhex(t, "556291df26d705a2caedd6474dd0079b"),
		CA:  unhex(t, "d4376f2da9c72226dd151b77c2919071155fc22a2068d90b5faa6c78c11e77dd"),
		CB:  unhex(t, "0660a680663e8c5695956fb22dff298b1d07a526cf3cc591adfecd1f6ef6e02e"),
	}

	if got := baseMul(w1).bytes(); !bytes.Equal(got, l) {
		t.Errorf("L = w1·P = % x, want % x", got, l)
	}
	shareX := ProverShare(w0, x)
	if !bytes.Equal(shareX, wantX) {
		t.Fatalf("X = % x, want % x", shareX, wantX)
	}
	shareY, vk, err := tr.VerifierFinish(Verifier{W0: w0, L: l}, y, shareX)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(shareY, wantY) {
		t.Fatalf("Y = % x, want % x", shareY, wantY)
	}
	pk, err := tr.ProverFinish(w0, w1, x, shareX, shareY)
	if err != nil {
		t.Fatal(err)
	}
	for who, k := range map[string]*Keys{"verifier": vk, "prover": pk} {
		for name, pair := range map[string][2][]byte{
			"Ka": {k.Ka, want.Ka}, "Ke": {k.Ke, want.Ke}, "KcA": {k.KcA, want.KcA},
			"KcB": {k.KcB, want.KcB}, "cA": {k.CA, want.CA}, "cB": {k.CB, want.CB},
		} {
			if !bytes.Equal(pair[0], pair[1]) {
				t.Errorf("%s %s = % x, want % x", who, name, pair[0], pair[1])
			}
		}
	}
}

// TestVerifierFromPasscode is connectedhomeip's default test verifier: passcode 20202021, salt
// "SPAKE2P Key Salt", 1000 iterations (CHIP_DEVICE_CONFIG_USE_TEST_SPAKE2P_VERIFIER).
func TestVerifierFromPasscode(t *testing.T) {
	want, _ := base64.StdEncoding.DecodeString("uWFwqugDNGiEck/po7KHwwMwwqZgN10XuyBajPGuyzUEV/iree4lOrao5GuwnlQ65CJzbeUB49s31EH+NEkg0JVI5MGCQGMMT/SRPFNRODm3wH/MBiehuFc6FJ/NH6Rmzw==")
	v, err := NewVerifier(20202021, []byte("SPAKE2P Key Salt"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Bytes(); !bytes.Equal(got, want) {
		t.Errorf("verifier = %s, want %s", base64.StdEncoding.EncodeToString(got), base64.StdEncoding.EncodeToString(want))
	}
	if _, err := ParseVerifier(want); err != nil {
		t.Errorf("ParseVerifier: %v", err)
	}
}

func TestPBKDFBounds(t *testing.T) {
	salt := make([]byte, 16)
	for _, c := range []struct {
		salt []byte
		iter int
	}{{salt, 999}, {salt, 100001}, {salt[:15], 1000}, {make([]byte, 33), 1000}} {
		if _, err := NewVerifier(1, c.salt, c.iter); err == nil {
			t.Errorf("salt %d bytes, %d iterations: accepted", len(c.salt), c.iter)
		}
	}
}

func TestSpake2pRejectsBadShares(t *testing.T) {
	v, _ := NewVerifier(20202021, []byte("SPAKE2P Key Salt"), 1000)
	y, _ := RandomScalar()
	tr := Transcript{Context: []byte("c")}
	bad := [][]byte{
		nil,
		make([]byte, PointSize), // zeros: not on the curve
		append([]byte{0x04}, bytes.Repeat([]byte{1}, 64)...),
		pointM.bytes()[:64],
	}
	for _, x := range bad {
		if _, _, err := tr.VerifierFinish(v, y, x); err == nil {
			t.Errorf("VerifierFinish accepted X = % x", x)
		}
	}
}

func TestSpake2pWrongPasscode(t *testing.T) {
	salt := []byte("SPAKE2P Key Salt")
	v, _ := NewVerifier(20202021, salt, 1000)
	w0, w1, _ := PBKDFScalars(20202022, salt, 1000)
	x, _ := RandomScalar()
	y, _ := RandomScalar()
	tr := Transcript{Context: []byte("c")}
	shareX := ProverShare(w0, x)
	shareY, vk, err := tr.VerifierFinish(v, y, shareX)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := tr.ProverFinish(w0, w1, x, shareX, shareY)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(pk.CA, vk.CA) || bytes.Equal(pk.Ke, vk.Ke) {
		t.Error("a wrong passcode agreed on keys")
	}
}
