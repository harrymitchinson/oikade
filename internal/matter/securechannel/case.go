package securechannel

import (
	"context"
	"crypto"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/netip"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/cert"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// CASE establishes a session between two nodes of a fabric, each proving itself with its node
// operational certificate (Core spec, 4.14.2):
//
//	initiator                              responder
//	Sigma1 (random, destination ID, ephemeral key) ──▶
//	         ◀── Sigma2 (random, ephemeral key, encrypted NOC and signature)
//	Sigma3 (encrypted NOC and signature) ──▶
//	         ◀── StatusReport
//
// A peer that has established CASE before can resume it instead: Sigma1 then carries the
// resumption ID from last time, and the responder answers with Sigma2_Resume, which the
// initiator acknowledges with a StatusReport.

const (
	// caseTimeout bounds a whole CASE handshake.
	caseTimeout = time.Minute
	// maxCASEHandshakes is how many handshakes a CASEResponder serves at once. Others are told
	// it's busy, and to try again after caseBusyWait.
	maxCASEHandshakes = 4
	caseBusyWait      = 500 * time.Millisecond
)

var (
	sigma2Info         = "Sigma2"
	sigma3Info         = "Sigma3"
	sigma1ResumeInfo   = "Sigma1_Resume"
	sigma2ResumeInfo   = "Sigma2_Resume"
	caseKeysInfo       = "SessionKeys"
	resumptionKeysInfo = "SessionResumptionKeys"
	groupKeyInfo       = "GroupKey v1.0"

	tbeData2Nonce = []byte("NCASE_Sigma2N")
	tbeData3Nonce = []byte("NCASE_Sigma3N")
	resume1Nonce  = []byte("NCASE_SigmaS1")
	resume2Nonce  = []byte("NCASE_SigmaS2")

	// matterEpoch is when Matter time starts. A clock before it hasn't been set.
	matterEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ErrCASEFailed is returned when a CASE handshake fails: the peer refused it, or its
// certificates or signature didn't check out.
var ErrCASEFailed = errors.New("matter: CASE failed")

// Fabric is what CASE needs of one fabric that a node belongs to: the fabric's root certificate,
// the node's operational certificates and key, and the fabric's identity protection key.
type Fabric struct {
	// Index is the fabric's index in the node's fabric table, from 1.
	Index uint8
	// RCAC, ICAC and NOC are the root, intermediate (nil if there's none) and node operational
	// certificates, in Matter TLV.
	RCAC, ICAC, NOC []byte
	// Key is the node's operational key, an ECDSA P-256 key.
	Key crypto.Signer
	// IPK is the identity protection key's epoch key, 16 bytes, as AddNOC gives it.
	IPK []byte
}

// FabricTable is the fabrics a CASEResponder answers for. Fabrics is called for each handshake,
// and must be safe for concurrent use. It returns a new *Fabric when a fabric changes, and never
// modifies one it has returned: CASE keeps what it derives from each *Fabric.
type FabricTable interface {
	Fabrics() []*Fabric
}

// fabricKeys is a Fabric with what CASE derives from it.
type fabricKeys struct {
	*Fabric
	rcac             *cert.Certificate
	fabricID, nodeID uint64
	ipk              []byte // the operational group key derived from the IPK epoch key
}

func (f *Fabric) keys() (*fabricKeys, error) {
	if f.Index == 0 {
		return nil, errors.New("matter: fabric index 0")
	}
	rcac, err := cert.Decode(f.RCAC)
	if err != nil {
		return nil, fmt.Errorf("fabric %d root: %w", f.Index, err)
	}
	noc, err := cert.Decode(f.NOC)
	if err != nil {
		return nil, fmt.Errorf("fabric %d NOC: %w", f.Index, err)
	}
	nodeID, err := noc.NodeID()
	if err != nil {
		return nil, fmt.Errorf("fabric %d NOC: %w", f.Index, err)
	}
	fabricID, _ := noc.FabricID()
	cfid, err := cert.CompressedFabricID(rcac.PublicKey, fabricID)
	if err != nil {
		return nil, fmt.Errorf("fabric %d root: %w", f.Index, err)
	}
	ipk, err := operationalIPK(f.IPK, cfid)
	if err != nil {
		return nil, fmt.Errorf("fabric %d: %w", f.Index, err)
	}
	return &fabricKeys{Fabric: f, rcac: rcac, fabricID: fabricID, nodeID: nodeID, ipk: ipk}, nil
}

// operationalIPK derives a fabric's operational IPK from its epoch key (Core spec, 4.17.2).
func operationalIPK(epochKey []byte, compressedFabricID uint64) ([]byte, error) {
	if len(epochKey) != mcrypto.SymmetricKeySize {
		return nil, fmt.Errorf("matter: IPK epoch key of %d bytes", len(epochKey))
	}
	salt := binary.BigEndian.AppendUint64(nil, compressedFabricID)
	return hkdf.Key(sha256.New, epochKey, salt, groupKeyInfo, mcrypto.SymmetricKeySize)
}

// destinationID names the node a Sigma1 is for, without naming it in the clear (Core spec,
// 4.14.2.4).
func destinationID(ipk, initiatorRandom, rootPublicKey []byte, fabricID, nodeID uint64) []byte {
	m := hmac.New(sha256.New, ipk)
	m.Write(initiatorRandom)
	m.Write(rootPublicKey)
	_ = binary.Write(m, binary.LittleEndian, fabricID)
	_ = binary.Write(m, binary.LittleEndian, nodeID)
	return m.Sum(nil)
}

func caseKey(secret []byte, info string, salt ...[]byte) ([]byte, error) {
	var s []byte
	for _, b := range salt {
		s = append(s, b...)
	}
	return hkdf.Key(sha256.New, secret, s, info, mcrypto.SymmetricKeySize)
}

func transcriptHash(msgs ...[]byte) []byte {
	h := sha256.New()
	for _, m := range msgs {
		h.Write(m)
	}
	return h.Sum(nil)
}

func newCCM(key []byte) (cipher.AEAD, error) {
	return mcrypto.NewCCM(key, mcrypto.TagSize, mcrypto.NonceSize)
}

// resumeMIC is the MIC that proves a peer holds a resumption's shared secret (Core spec,
// 4.14.2.2): an AES-CCM tag over nothing, under a key derived from the secret.
func resumeMIC(secret, initiatorRandom, resumptionID []byte, info string, nonce []byte) ([]byte, error) {
	k, err := caseKey(secret, info, initiatorRandom, resumptionID)
	if err != nil {
		return nil, err
	}
	aead, err := newCCM(k)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, nil, nil), nil
}

// sign signs msg with an operational key, and returns the signature as r and s, 32 bytes each.
func sign(key crypto.Signer, msg []byte) ([]byte, error) {
	digest := sha256.Sum256(msg)
	der, err := key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("matter: signing: %w", err)
	}
	var rs struct{ R, S *big.Int }
	if rest, err := asn1.Unmarshal(der, &rs); err != nil || len(rest) > 0 ||
		rs.R.Sign() <= 0 || rs.S.Sign() <= 0 || rs.R.BitLen() > 256 || rs.S.BitLen() > 256 {
		return nil, errors.New("matter: operational key returned a malformed ECDSA P-256 signature")
	}
	sig := make([]byte, signatureSize)
	rs.R.FillBytes(sig[:signatureSize/2])
	rs.S.FillBytes(sig[signatureSize/2:])
	return sig, nil
}

func verifySignature(pub *ecdsa.PublicKey, msg, sig []byte) bool {
	digest := sha256.Sum256(msg)
	r := new(big.Int).SetBytes(sig[:signatureSize/2])
	s := new(big.Int).SetBytes(sig[signatureSize/2:])
	return ecdsa.Verify(pub, digest[:], r, s)
}

// validityTime is the time to check certificates' validity at. A clock that was never set reads
// before the Matter epoch: CASE then skips validity periods, as connectedhomeip does for a node
// with no time source, rather than refusing every peer until the clock is set.
func validityTime(now func() time.Time) time.Time {
	t := time.Now()
	if now != nil {
		t = now()
	}
	if t.Before(matterEpoch) {
		return time.Time{}
	}
	return t
}

// peer is a CASE peer whose certificates checked out.
type peer struct {
	nodeID uint64
	cats   []uint32
	key    *ecdsa.PublicKey
}

// verifyPeer checks a peer's certificates chain to the fabric's root, and are on its fabric.
func (f *fabricKeys) verifyPeer(nocBytes, icacBytes []byte, now time.Time) (*peer, error) {
	noc, err := cert.Decode(nocBytes)
	if err != nil {
		return nil, err
	}
	var icac *cert.Certificate
	if len(icacBytes) > 0 {
		if icac, err = cert.Decode(icacBytes); err != nil {
			return nil, err
		}
	}
	if err := cert.Verify(f.rcac, icac, noc, now); err != nil {
		return nil, err
	}
	if id, _ := noc.FabricID(); id != f.fabricID {
		return nil, fmt.Errorf("%w: peer is on fabric %#016x, not %#016x", ErrCASEFailed, id, f.fabricID)
	}
	nodeID, err := noc.NodeID()
	if err != nil {
		return nil, err
	}
	key, err := noc.ECDSAPublicKey()
	if err != nil {
		return nil, err
	}
	return &peer{nodeID: nodeID, cats: noc.CATs(), key: key}, nil
}

// CASEResponder answers CASE for the fabrics a node belongs to.
type CASEResponder struct {
	Transport *transport.Transport
	Fabrics   FabricTable
	// Params are the node's MRP intervals, sent to the initiator. nil sends none.
	Params *SessionParams
	// Resumption remembers the node's recent peers, so they can resume. nil disables resumption.
	Resumption *ResumptionStore
	// Now is the time to check peers' certificates at; nil is time.Now. See validityTime.
	Now func() time.Time
	// OnSession is called with each session CASE establishes.
	OnSession func(*transport.Session)
	Log       *slog.Logger

	mu     sync.Mutex
	active int
	keys   map[*Fabric]cachedKeys // what CASE derived from each fabric Fabrics returned
}

type cachedKeys struct {
	keys *fabricKeys
	err  error
}

// Register starts answering Sigma1 on the Transport.
func (r *CASEResponder) Register() {
	r.Transport.Handle(transport.SecureChannel, []uint8{OpSigma1}, r.serve)
}

func (r *CASEResponder) serve(ctx context.Context, ex *transport.Exchange, first transport.Message) {
	defer ex.Close()
	log := r.Log.With("peer", ex.Session().Peer())

	if ex.Session().Secure() {
		log.Info("Refused CASE over a secure session")
		r.status(ctx, ex, secureChannelStatus(GeneralFailure, CodeInvalidParameter))
		return
	}
	r.mu.Lock()
	if r.active >= maxCASEHandshakes {
		r.mu.Unlock()
		log.Info("Refused CASE while busy")
		r.status(ctx, ex, busyStatus())
		return
	}
	r.active++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, caseTimeout)
	defer cancel()
	s, resumed, err := r.handshake(ctx, ex, first)
	if err != nil {
		log.Info("CASE failed", "err", err)
		return
	}
	log.Info("Established CASE session", "session", s.LocalSessionID(), "fabric", s.FabricIndex(),
		"node", fmt.Sprintf("%016X", s.PeerNodeID()), "resumed", resumed)
	if r.OnSession != nil {
		r.OnSession(s)
	}
}

func (r *CASEResponder) status(ctx context.Context, ex *transport.Exchange, sr StatusReport) {
	if err := ex.Send(ctx, sr.Message()); err != nil {
		r.Log.Debug("Could not send status report", "err", err)
	}
}

func (r *CASEResponder) fail(ctx context.Context, ex *transport.Exchange, code uint16, err error) error {
	r.status(ctx, ex, secureChannelStatus(GeneralFailure, code))
	if !errors.Is(err, ErrCASEFailed) {
		err = fmt.Errorf("%w: %w", ErrCASEFailed, err)
	}
	return err
}

// busyStatus tells an initiator to try again after caseBusyWait.
func busyStatus() StatusReport {
	sr := secureChannelStatus(GeneralBusy, CodeBusy)
	sr.Data = binary.LittleEndian.AppendUint16(nil, uint16(caseBusyWait.Milliseconds()))
	return sr
}

// fabricKeys returns the fabrics CASE can use, derived once for each *Fabric the table returns:
// a FabricTable returns a new *Fabric when a fabric changes, and never modifies one it returned.
// A fabric CASE can't use is logged once.
func (r *CASEResponder) fabricKeys() []*fabricKeys {
	fabrics := r.Fabrics.Fabrics()
	r.mu.Lock()
	defer r.mu.Unlock()
	cache := make(map[*Fabric]cachedKeys, len(fabrics))
	var out []*fabricKeys
	for _, f := range fabrics {
		c, ok := r.keys[f]
		if !ok {
			c.keys, c.err = f.keys()
			if c.err != nil {
				r.Log.Error("Could not use fabric for CASE", "err", c.err, "fabric", f.Index)
			}
		}
		cache[f] = c
		if c.err == nil {
			out = append(out, c.keys)
		}
	}
	r.keys = cache
	return out
}

// fabric returns the fabric with the given index, ready for CASE.
func (r *CASEResponder) fabric(index uint8) *fabricKeys {
	for _, k := range r.fabricKeys() {
		if k.Index == index {
			return k
		}
	}
	return nil
}

// match finds the fabric and node a Sigma1's destination ID names.
func (r *CASEResponder) match(dest, initiatorRandom []byte) *fabricKeys {
	for _, k := range r.fabricKeys() {
		want := destinationID(k.ipk, initiatorRandom, k.rcac.PublicKey, k.fabricID, k.nodeID)
		if subtle.ConstantTimeCompare(want, dest) == 1 {
			return k
		}
	}
	return nil
}

func (r *CASEResponder) handshake(ctx context.Context, ex *transport.Exchange, first transport.Message) (*transport.Session, bool, error) {
	s1, err := decodeSigma1(first.Payload)
	if err != nil {
		return nil, false, r.fail(ctx, ex, CodeInvalidParameter, err)
	}
	if s1.resumptionID != nil {
		// A resumption ID is good for one resumption: take it, so a replayed or concurrent
		// Sigma1 with it falls back to a full CASE. Only a Sigma1 with the right MIC takes it,
		// so one that merely names it, as anyone watching can, doesn't spend it.
		res, ok := r.Resumption.take(s1.resumptionID, func(res resumption) bool {
			mic, err := resumeMIC(res.secret, s1.initiatorRandom, s1.resumptionID, sigma1ResumeInfo, resume1Nonce)
			return err == nil && subtle.ConstantTimeCompare(mic, s1.resumeMIC) == 1
		})
		if ok {
			if f := r.fabric(res.fabric); f != nil && res.on(f) {
				s, err := r.resume(ctx, ex, s1, res, f)
				return s, true, err
			}
		}
		// An unknown or stale resumption falls back to a full CASE.
	}
	f := r.match(s1.destinationID, s1.initiatorRandom)
	if f == nil {
		return nil, false, r.fail(ctx, ex, CodeNoSharedTrustRoots, errors.New("no fabric matches the destination ID"))
	}
	s, err := r.full(ctx, ex, first.Payload, s1, f)
	return s, false, err
}

// establish adds the session before telling the initiator it's established, so its first secure
// message finds it, and keeps it only if the initiator hears.
func (r *CASEResponder) establish(ctx context.Context, ex *transport.Exchange, c transport.SecureSessionConfig) (*transport.Session, error) {
	s := r.Transport.AddSecureSession(c)
	ackCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	if err := ex.Send(ackCtx, secureChannelStatus(GeneralSuccess, CodeSessionEstablishmentSuccess).Message()); err != nil {
		r.Transport.RemoveSession(s)
		return nil, err
	}
	if err := ex.WaitAck(ackCtx); err != nil {
		r.Transport.RemoveSession(s)
		return nil, fmt.Errorf("the initiator didn't acknowledge success: %w", err)
	}
	return s, nil
}

func (r *CASEResponder) full(ctx context.Context, ex *transport.Exchange, s1Bytes []byte, s1 *sigma1, f *fabricKeys) (*transport.Session, error) {
	peerEph, err := ecdh.P256().NewPublicKey(s1.initiatorEphPubKey)
	if err != nil {
		return nil, r.fail(ctx, ex, CodeInvalidParameter, err)
	}
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	secret, err := eph.ECDH(peerEph)
	if err != nil {
		return nil, r.fail(ctx, ex, CodeInvalidParameter, err)
	}
	ephPub := eph.PublicKey().Bytes()

	tr := r.Transport
	localID, err := tr.AllocateSessionID()
	if err != nil {
		r.status(ctx, ex, busyStatus())
		return nil, err
	}
	established := false
	defer func() {
		if !established {
			tr.ReleaseSessionID(localID)
		}
	}()

	resumptionID := make([]byte, resumptionIDSize)
	_, _ = rand.Read(resumptionID)
	sig, err := sign(f.Key, tbsData(f.NOC, f.ICAC, ephPub, s1.initiatorEphPubKey))
	if err != nil {
		return nil, r.fail(ctx, ex, CodeInvalidParameter, err)
	}
	tbe := (&tbeData{noc: f.NOC, icac: f.ICAC, signature: sig, resumptionID: resumptionID}).encode()
	responderRandom := random()
	s2k, err := caseKey(secret, sigma2Info, f.ipk, responderRandom, ephPub, transcriptHash(s1Bytes))
	if err != nil {
		return nil, err
	}
	aead, err := newCCM(s2k)
	if err != nil {
		return nil, err
	}
	s2Bytes := (&sigma2{
		responderRandom: responderRandom, responderSessionID: localID, responderEphPubKey: ephPub,
		encrypted2: aead.Seal(nil, tbeData2Nonce, tbe, nil), params: r.Params,
	}).encode()
	if err := ex.Send(ctx, message(OpSigma2, s2Bytes)); err != nil {
		return nil, err
	}

	s3Bytes, err := expectWithin(ctx, ex, OpSigma3, ErrCASEFailed)
	if err != nil {
		return nil, err
	}
	enc3, err := decodeSigma3(s3Bytes)
	if err != nil {
		return nil, r.fail(ctx, ex, CodeInvalidParameter, err)
	}
	s3k, err := caseKey(secret, sigma3Info, f.ipk, transcriptHash(s1Bytes, s2Bytes))
	if err != nil {
		return nil, err
	}
	if aead, err = newCCM(s3k); err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, tbeData3Nonce, enc3, nil)
	if err != nil {
		return nil, r.fail(ctx, ex, CodeInvalidParameter, fmt.Errorf("Sigma3 doesn't decrypt: %w", err))
	}
	t3, err := decodeTBEData(plain, false)
	if err != nil {
		return nil, r.fail(ctx, ex, CodeInvalidParameter, err)
	}
	p, err := f.verifyPeer(t3.noc, t3.icac, validityTime(r.Now))
	if err != nil {
		return nil, r.fail(ctx, ex, CodeInvalidParameter, fmt.Errorf("initiator's certificates: %w", err))
	}
	if !verifySignature(p.key, tbsData(t3.noc, t3.icac, s1.initiatorEphPubKey, ephPub), t3.signature) {
		return nil, r.fail(ctx, ex, CodeInvalidParameter, errors.New("the initiator's signature doesn't verify"))
	}

	sk, err := deriveSessionKeys(secret, append(append([]byte(nil), f.ipk...), transcriptHash(s1Bytes, s2Bytes, s3Bytes)...), caseKeysInfo)
	if err != nil {
		return nil, err
	}
	sec, err := NewSecurity(sk, false, f.nodeID, p.nodeID)
	if err != nil {
		return nil, err
	}
	established = true
	s, err := r.establish(ctx, ex, transport.SecureSessionConfig{
		LocalSessionID: localID, PeerSessionID: s1.initiatorSessionID, Peer: ex.Session().Peer(),
		LocalNodeID: f.nodeID, PeerNodeID: p.nodeID, FabricIndex: f.Index, CATs: p.cats, Auth: transport.AuthCASE, AttestationChallenge: sk.AttestationChallenge,
		Security: sec, PeerMRP: s1.params.MRP(),
	})
	if err != nil {
		return nil, err
	}
	res := resumption{fabric: f.Index, rootKey: f.rcac.PublicKey, fabricID: f.fabricID, node: p.nodeID, cats: p.cats, secret: secret}
	copy(res.id[:], resumptionID)
	r.Resumption.save(res)
	return s, nil
}

func (r *CASEResponder) resume(ctx context.Context, ex *transport.Exchange, s1 *sigma1, res resumption, f *fabricKeys) (*transport.Session, error) {
	tr := r.Transport
	localID, err := tr.AllocateSessionID()
	if err != nil {
		r.status(ctx, ex, busyStatus())
		return nil, err
	}
	established := false
	defer func() {
		if !established {
			tr.ReleaseSessionID(localID)
		}
	}()

	newID := make([]byte, resumptionIDSize)
	_, _ = rand.Read(newID)
	mic, err := resumeMIC(res.secret, s1.initiatorRandom, newID, sigma2ResumeInfo, resume2Nonce)
	if err != nil {
		return nil, err
	}
	sk, err := deriveSessionKeys(res.secret, append(append([]byte(nil), s1.initiatorRandom...), s1.resumptionID...), resumptionKeysInfo)
	if err != nil {
		return nil, err
	}
	sec, err := NewSecurity(sk, false, f.nodeID, res.node)
	if err != nil {
		return nil, err
	}
	// Add the session before sending Sigma2_Resume: the initiator may use it as soon as it has
	// acknowledged it.
	s := tr.AddSecureSession(transport.SecureSessionConfig{
		LocalSessionID: localID, PeerSessionID: s1.initiatorSessionID, Peer: ex.Session().Peer(),
		LocalNodeID: f.nodeID, PeerNodeID: res.node, FabricIndex: f.Index, CATs: res.cats, Auth: transport.AuthCASE, AttestationChallenge: sk.AttestationChallenge,
		Security: sec, PeerMRP: s1.params.MRP(),
	})
	established = true
	msg := (&sigma2Resume{resumptionID: newID, resumeMIC: mic, responderSessionID: localID, params: r.Params}).encode()
	if err := ex.Send(ctx, message(OpSigma2Resume, msg)); err != nil {
		tr.RemoveSession(s)
		return nil, err
	}
	stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	if err := expectSuccess(stepCtx, ex); err != nil {
		tr.RemoveSession(s)
		return nil, err
	}
	res.id = [resumptionIDSize]byte(newID)
	r.Resumption.save(res)
	return s, nil
}

// expectSuccess waits for a status report, and returns an error unless it's success.
func expectSuccess(ctx context.Context, ex *transport.Exchange) error {
	m, err := ex.Recv(ctx)
	if err != nil {
		return err
	}
	if m.Protocol != transport.SecureChannel || m.Opcode != OpStatusReport {
		return fmt.Errorf("%w: opcode %#x instead of a status report", ErrInvalidMessage, m.Opcode)
	}
	sr, err := ParseStatusReport(m.Payload)
	if err != nil {
		return err
	}
	if !sr.Success() {
		return fmt.Errorf("%w: %w", ErrCASEFailed, sr)
	}
	return nil
}

// CASEConfig configures EstablishCASE.
type CASEConfig struct {
	// Params are the initiator's MRP intervals, sent to the peer. nil sends none.
	Params *SessionParams
	// Resumption remembers peers, so a later EstablishCASE can resume. nil disables resumption.
	Resumption *ResumptionStore
	// Now is the time to check the peer's certificates at; nil is time.Now. See validityTime.
	Now func() time.Time
}

// EstablishCASE runs CASE as the initiator with the node peerNode of fabric f, at addr, and
// returns the secure session. If c.Resumption remembers the peer, it tries to resume first, and
// the peer falls back to a full CASE if it doesn't remember us.
func EstablishCASE(ctx context.Context, tr *transport.Transport, addr netip.AddrPort, f *Fabric, peerNode uint64, c CASEConfig) (s *transport.Session, err error) {
	fk, err := f.keys()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, caseTimeout)
	defer cancel()
	unsecured := tr.NewUnsecuredSession(addr)
	defer tr.RemoveSession(unsecured)
	ex, err := unsecured.NewExchange()
	if err != nil {
		return nil, err
	}
	defer ex.Close()

	localID, err := tr.AllocateSessionID()
	if err != nil {
		return nil, err
	}
	established := false
	defer func() {
		if !established {
			tr.ReleaseSessionID(localID)
		}
	}()

	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ephPub := eph.PublicKey().Bytes()
	initiatorRandom := random()
	s1 := &sigma1{
		initiatorRandom: initiatorRandom, initiatorSessionID: localID,
		destinationID:      destinationID(fk.ipk, initiatorRandom, fk.rcac.PublicKey, fk.fabricID, peerNode),
		initiatorEphPubKey: ephPub, params: c.Params,
	}
	res, resuming := c.Resumption.byPeer(f.Index, peerNode)
	resuming = resuming && res.on(fk)
	if resuming {
		s1.resumptionID = res.id[:]
		if s1.resumeMIC, err = resumeMIC(res.secret, initiatorRandom, s1.resumptionID, sigma1ResumeInfo, resume1Nonce); err != nil {
			return nil, err
		}
	}
	s1Bytes := s1.encode()
	if err := ex.Send(ctx, message(OpSigma1, s1Bytes)); err != nil {
		return nil, err
	}
	m, err := ex.Recv(ctx)
	if err != nil {
		return nil, err
	}
	// From here the peer holds a handshake for us. If we give up, tell it, unless it gave up
	// first.
	defer func() {
		if err != nil && !errors.As(err, new(StatusReport)) {
			abort(ex)
		}
	}()
	if m.Protocol != transport.SecureChannel {
		return nil, fmt.Errorf("%w: protocol %#x", ErrInvalidMessage, m.Protocol.ID)
	}
	i := &caseInitiator{tr: tr, ex: ex, addr: addr, f: fk, peerNode: peerNode, c: c, localID: localID}
	switch {
	case m.Opcode == OpStatusReport:
		sr, err := ParseStatusReport(m.Payload)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrCASEFailed, sr)
	case m.Opcode == OpSigma2Resume && resuming:
		s, err = i.resumed(ctx, initiatorRandom, res, m.Payload)
	case m.Opcode == OpSigma2:
		s, err = i.full(ctx, eph, s1Bytes, m.Payload)
	default:
		return nil, fmt.Errorf("%w: opcode %#x", ErrInvalidMessage, m.Opcode)
	}
	if err == nil {
		established = true
	} else if !errors.Is(err, ErrCASEFailed) && errors.Is(err, ErrInvalidMessage) {
		err = fmt.Errorf("%w: %w", ErrCASEFailed, err)
	}
	return s, err
}

type caseInitiator struct {
	tr       *transport.Transport
	ex       *transport.Exchange
	addr     netip.AddrPort
	f        *fabricKeys
	peerNode uint64
	c        CASEConfig
	localID  uint16
}

func (i *caseInitiator) full(ctx context.Context, eph *ecdh.PrivateKey, s1Bytes, s2Bytes []byte) (*transport.Session, error) {
	f := i.f
	s2, err := decodeSigma2(s2Bytes)
	if err != nil {
		return nil, err
	}
	peerEph, err := ecdh.P256().NewPublicKey(s2.responderEphPubKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	secret, err := eph.ECDH(peerEph)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	ephPub := eph.PublicKey().Bytes()
	s2k, err := caseKey(secret, sigma2Info, f.ipk, s2.responderRandom, s2.responderEphPubKey, transcriptHash(s1Bytes))
	if err != nil {
		return nil, err
	}
	aead, err := newCCM(s2k)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, tbeData2Nonce, s2.encrypted2, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: Sigma2 doesn't decrypt: %w", ErrCASEFailed, err)
	}
	t2, err := decodeTBEData(plain, true)
	if err != nil {
		return nil, err
	}
	p, err := f.verifyPeer(t2.noc, t2.icac, validityTime(i.c.Now))
	if err != nil {
		return nil, fmt.Errorf("%w: responder's certificates: %w", ErrCASEFailed, err)
	}
	if p.nodeID != i.peerNode {
		return nil, fmt.Errorf("%w: responder is node %016X, not %016X", ErrCASEFailed, p.nodeID, i.peerNode)
	}
	if !verifySignature(p.key, tbsData(t2.noc, t2.icac, s2.responderEphPubKey, ephPub), t2.signature) {
		return nil, fmt.Errorf("%w: the responder's signature doesn't verify", ErrCASEFailed)
	}

	sig, err := sign(f.Key, tbsData(f.NOC, f.ICAC, ephPub, s2.responderEphPubKey))
	if err != nil {
		return nil, err
	}
	tbe := (&tbeData{noc: f.NOC, icac: f.ICAC, signature: sig}).encode()
	s3k, err := caseKey(secret, sigma3Info, f.ipk, transcriptHash(s1Bytes, s2Bytes))
	if err != nil {
		return nil, err
	}
	if aead, err = newCCM(s3k); err != nil {
		return nil, err
	}
	s3Bytes := encodeSigma3(aead.Seal(nil, tbeData3Nonce, tbe, nil))
	if err := i.ex.Send(ctx, message(OpSigma3, s3Bytes)); err != nil {
		return nil, err
	}
	if err := expectSuccess(ctx, i.ex); err != nil {
		return nil, err
	}

	sk, err := deriveSessionKeys(secret, append(append([]byte(nil), f.ipk...), transcriptHash(s1Bytes, s2Bytes, s3Bytes)...), caseKeysInfo)
	if err != nil {
		return nil, err
	}
	sec, err := NewSecurity(sk, true, f.nodeID, p.nodeID)
	if err != nil {
		return nil, err
	}
	res := resumption{fabric: f.Index, rootKey: f.rcac.PublicKey, fabricID: f.fabricID, node: p.nodeID, cats: p.cats, secret: secret}
	copy(res.id[:], t2.resumptionID)
	i.c.Resumption.save(res)
	return i.tr.AddSecureSession(transport.SecureSessionConfig{
		LocalSessionID: i.localID, PeerSessionID: s2.responderSessionID, Peer: i.addr,
		LocalNodeID: f.nodeID, PeerNodeID: p.nodeID, FabricIndex: f.Index, CATs: p.cats, Auth: transport.AuthCASE, AttestationChallenge: sk.AttestationChallenge,
		Security: sec, PeerMRP: s2.params.MRP(),
	}), nil
}

func (i *caseInitiator) resumed(ctx context.Context, initiatorRandom []byte, res resumption, payload []byte) (*transport.Session, error) {
	f := i.f
	m, err := decodeSigma2Resume(payload)
	if err != nil {
		return nil, err
	}
	mic, err := resumeMIC(res.secret, initiatorRandom, m.resumptionID, sigma2ResumeInfo, resume2Nonce)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(mic, m.resumeMIC) != 1 {
		i.c.Resumption.forget(f.Index, i.peerNode)
		return nil, fmt.Errorf("%w: Sigma2_Resume's MIC doesn't match", ErrCASEFailed)
	}
	sk, err := deriveSessionKeys(res.secret, append(append([]byte(nil), initiatorRandom...), res.id[:]...), resumptionKeysInfo)
	if err != nil {
		return nil, err
	}
	sec, err := NewSecurity(sk, true, f.nodeID, res.node)
	if err != nil {
		return nil, err
	}
	// The responder keeps the session only if it hears that we have it too.
	stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	if err := i.ex.Send(stepCtx, secureChannelStatus(GeneralSuccess, CodeSessionEstablishmentSuccess).Message()); err != nil {
		return nil, err
	}
	if err := i.ex.WaitAck(stepCtx); err != nil {
		return nil, fmt.Errorf("the responder didn't acknowledge success: %w", err)
	}
	res.id = [resumptionIDSize]byte(m.resumptionID)
	i.c.Resumption.save(res)
	return i.tr.AddSecureSession(transport.SecureSessionConfig{
		LocalSessionID: i.localID, PeerSessionID: m.responderSessionID, Peer: i.addr,
		LocalNodeID: f.nodeID, PeerNodeID: res.node, FabricIndex: f.Index, CATs: res.cats, Auth: transport.AuthCASE, AttestationChallenge: sk.AttestationChallenge,
		Security: sec, PeerMRP: m.params.MRP(),
	}), nil
}
