package crypto

import (
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
)

// SPAKE2+ on P-256 with SHA-256, HKDF and HMAC, as Matter uses it for PASE (Core spec, 3.10).
// Matter follows draft-bar-cfrg-spake2plus-01: the transcript hash splits into Ka and Ke, and the
// confirmation keys come from Ka.
//
// Go's standard library has no public P-256 point addition except in crypto/elliptic, which is
// deprecated but kept under the Go 1 promise. Its P-256 is backed by the constant-time
// crypto/internal/fips140/nistec.

// Sizes of SPAKE2+ values.
const (
	ScalarSize = 32
	PointSize  = 65 // uncompressed
	// VerifierSize is a serialized verifier: w0 then L.
	VerifierSize = ScalarSize + PointSize
	hashSize     = sha256.Size
	wsSize       = ScalarSize + 8 // w0s and w1s, before reduction mod n
)

// PBKDF2 bounds for PASE (Core spec, 3.9).
const (
	MinIterations = 1000
	MaxIterations = 100000
	MinSaltSize   = 16
	MaxSaltSize   = 32
)

var (
	curve = elliptic.P256()
	order = curve.Params().N
	prime = curve.Params().P
	// The SPAKE2+ constants for P-256 (RFC 9383, 4).
	pointM      = mustDecompress("02886e2f97ace46e55ba9dd7242579f2993b64e16ef3dcab95afd497333d8fa12f")
	pointN      = mustDecompress("03d8bbd6c639c62937b04d997f38c3770719c629d7014d49a24b4f98baa1292b49")
	pointMBytes = pointM.bytes()
	pointNBytes = pointN.bytes()
)

var errBadPoint = errors.New("matter: SPAKE2+ share is not a point on P-256")

type point struct{ x, y *big.Int }

func mustDecompress(h string) point {
	b, err := hex.DecodeString(h)
	if err != nil {
		panic(err)
	}
	x, y := elliptic.UnmarshalCompressed(curve, b)
	if x == nil {
		panic("matter: bad SPAKE2+ constant")
	}
	return point{x, y}
}

func (p point) bytes() []byte {
	//nolint:staticcheck // SA1019: the only public P-256 point encoding with addition; see above.
	return elliptic.Marshal(curve, p.x, p.y)
}

func parsePoint(b []byte) (point, error) {
	if len(b) != PointSize {
		return point{}, errBadPoint
	}
	//nolint:staticcheck // SA1019: see above. Unmarshal rejects points off the curve and infinity.
	x, y := elliptic.Unmarshal(curve, b)
	if x == nil {
		return point{}, errBadPoint
	}
	return point{x, y}, nil
}

func mul(p point, k []byte) point {
	//nolint:staticcheck // SA1019: see above.
	x, y := curve.ScalarMult(p.x, p.y, k)
	return point{x, y}
}

func baseMul(k []byte) point {
	//nolint:staticcheck // SA1019: see above.
	x, y := curve.ScalarBaseMult(k)
	return point{x, y}
}

func add(p, q point) point {
	//nolint:staticcheck // SA1019: see above.
	x, y := curve.Add(p.x, p.y, q.x, q.y)
	return point{x, y}
}

func neg(p point) point {
	if isIdentity(p) {
		return p
	}
	return point{p.x, new(big.Int).Sub(prime, p.y)}
}

func isIdentity(p point) bool { return p.x.Sign() == 0 && p.y.Sign() == 0 }

// scalar encodes k as a 32-byte big-endian scalar.
func scalar(k *big.Int) []byte { return k.FillBytes(make([]byte, ScalarSize)) }

// PBKDFScalars derives w0 and w1 from a setup passcode (Core spec, 3.10): PBKDF2-HMAC-SHA256 over
// the passcode as 4 little-endian bytes, split in two and reduced mod the group order.
func PBKDFScalars(passcode uint32, salt []byte, iterations int) (w0, w1 []byte, err error) {
	if iterations < MinIterations || iterations > MaxIterations {
		return nil, nil, fmt.Errorf("matter: PBKDF2 iterations %d outside [%d, %d]", iterations, MinIterations, MaxIterations)
	}
	if len(salt) < MinSaltSize || len(salt) > MaxSaltSize {
		return nil, nil, fmt.Errorf("matter: PBKDF2 salt of %d bytes outside [%d, %d]", len(salt), MinSaltSize, MaxSaltSize)
	}
	pw := binary.LittleEndian.AppendUint32(nil, passcode)
	ws, err := pbkdf2.Key(sha256.New, string(pw), salt, iterations, 2*wsSize)
	if err != nil {
		return nil, nil, err
	}
	reduce := func(b []byte) []byte { return scalar(new(big.Int).Mod(new(big.Int).SetBytes(b), order)) }
	return reduce(ws[:wsSize]), reduce(ws[wsSize:]), nil
}

// Verifier is what a device stores instead of its passcode: w0 and L = w1·P.
type Verifier struct {
	W0 []byte // ScalarSize
	L  []byte // PointSize
}

// NewVerifier computes the verifier for a passcode.
func NewVerifier(passcode uint32, salt []byte, iterations int) (Verifier, error) {
	w0, w1, err := PBKDFScalars(passcode, salt, iterations)
	if err != nil {
		return Verifier{}, err
	}
	return Verifier{W0: w0, L: baseMul(w1).bytes()}, nil
}

// Bytes serializes the verifier as w0 then L, as connectedhomeip's tools do.
func (v Verifier) Bytes() []byte { return append(append([]byte(nil), v.W0...), v.L...) }

// ParseVerifier reads a serialized verifier.
func ParseVerifier(b []byte) (Verifier, error) {
	if len(b) != VerifierSize {
		return Verifier{}, fmt.Errorf("matter: verifier of %d bytes, want %d", len(b), VerifierSize)
	}
	v := Verifier{W0: b[:ScalarSize:ScalarSize], L: b[ScalarSize:]}
	return v, v.validate()
}

// validate checks that w0 is in [1, n-1] and L is a point on the curve.
func (v Verifier) validate() error {
	if !validScalar(v.W0) {
		return errors.New("matter: verifier's w0 is not in [1, n-1]")
	}
	_, err := parsePoint(v.L)
	return err
}

func validScalar(k []byte) bool {
	n := new(big.Int).SetBytes(k)
	return len(k) == ScalarSize && n.Sign() > 0 && n.Cmp(order) < 0
}

// Transcript identifies one SPAKE2+ run: its context and the parties' identities, which Matter
// leaves empty.
type Transcript struct {
	Context    []byte
	IDProver   []byte
	IDVerifier []byte
}

// Keys are what a SPAKE2+ run agrees on.
type Keys struct {
	Ka, Ke   []byte // the two halves of the transcript hash
	KcA, KcB []byte // confirmation keys
	CA       []byte // the prover's confirmation, HMAC(KcA, Y)
	CB       []byte // the verifier's confirmation, HMAC(KcB, X)
}

// RandomScalar returns a uniformly random scalar in [1, n-1].
func RandomScalar() ([]byte, error) {
	k, err := rand.Int(rand.Reader, new(big.Int).Sub(order, big.NewInt(1)))
	if err != nil {
		return nil, err
	}
	return scalar(k.Add(k, big.NewInt(1))), nil
}

// ProverShare is the prover's share X = x·P + w0·M.
func ProverShare(w0, x []byte) []byte {
	return add(baseMul(x), mul(pointM, w0)).bytes()
}

// ProverFinish completes the run for the prover, from its secrets and both shares.
func (t Transcript) ProverFinish(w0, w1, x, shareX, shareY []byte) (*Keys, error) {
	if !validScalar(w0) || !validScalar(w1) || !validScalar(x) {
		return nil, errors.New("matter: SPAKE2+ scalars are not in [1, n-1]")
	}
	y, err := parsePoint(shareY)
	if err != nil {
		return nil, err
	}
	base := add(y, neg(mul(pointN, w0))) // Y - w0·N
	z, v := mul(base, x), mul(base, w1)
	if isIdentity(z) || isIdentity(v) {
		return nil, errBadPoint
	}
	return t.keys(w0, shareX, shareY, z.bytes(), v.bytes()), nil
}

// VerifierFinish answers the prover's share X with the verifier's share Y = y·P + w0·N, and
// completes the run for the verifier.
func (t Transcript) VerifierFinish(v Verifier, y, shareX []byte) (shareY []byte, k *Keys, err error) {
	if err := v.validate(); err != nil {
		return nil, nil, err
	}
	if !validScalar(y) {
		return nil, nil, errors.New("matter: SPAKE2+ y is not in [1, n-1]")
	}
	x, err := parsePoint(shareX)
	if err != nil {
		return nil, nil, err
	}
	l, _ := parsePoint(v.L)
	shareY = add(baseMul(y), mul(pointN, v.W0)).bytes()
	z := mul(add(x, neg(mul(pointM, v.W0))), y) // y·(X - w0·M)
	vv := mul(l, y)
	if isIdentity(z) || isIdentity(vv) {
		return nil, nil, errBadPoint
	}
	return shareY, t.keys(v.W0, shareX, shareY, z.bytes(), vv.bytes()), nil
}

func (t Transcript) keys(w0, shareX, shareY, z, v []byte) *Keys {
	var tt []byte
	for _, item := range [][]byte{t.Context, t.IDProver, t.IDVerifier, pointMBytes, pointNBytes, shareX, shareY, z, v, w0} {
		tt = binary.LittleEndian.AppendUint64(tt, uint64(len(item)))
		tt = append(tt, item...)
	}
	h := sha256.Sum256(tt)
	k := &Keys{Ka: h[:hashSize/2], Ke: h[hashSize/2:]}
	kc, err := hkdf.Key(sha256.New, k.Ka, nil, "ConfirmationKeys", hashSize)
	if err != nil {
		panic(err) // only for an output longer than HKDF allows
	}
	k.KcA, k.KcB = kc[:hashSize/2], kc[hashSize/2:]
	k.CA, k.CB = MAC(k.KcA, shareY), MAC(k.KcB, shareX)
	return k
}

// MAC is HMAC-SHA256.
func MAC(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}
