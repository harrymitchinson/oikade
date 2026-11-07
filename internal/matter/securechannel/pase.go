package securechannel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// PASE derives a session from the setup passcode with SPAKE2+ (Core spec, 4.14.1):
//
//	initiator                              responder
//	PBKDFParamRequest  ──────────────────▶
//	                   ◀──────────────────  PBKDFParamResponse (salt, iterations)
//	Pake1 (X)          ──────────────────▶
//	                   ◀──────────────────  Pake2 (Y, cB)
//	Pake3 (cA)         ──────────────────▶
//	                   ◀──────────────────  StatusReport

const (
	paseContextPrefix = "CHIP PAKE V1 Commissioning"
	paseKeysInfo      = "SessionKeys"
	// paseTimeout bounds a whole PASE handshake for the commissioner.
	paseTimeout = time.Minute
	// stepTimeout bounds how long the device waits for each of the commissioner's messages, so a
	// commissioner that goes quiet holds the one handshake slot only briefly.
	stepTimeout = 15 * time.Second
)

// ErrPASEFailed is returned by EstablishPASE when the device rejects the passcode or the
// handshake.
var ErrPASEFailed = errors.New("matter: PASE failed")

// Responder answers PASE for a device that a commissioner is adding. It serves one handshake at a
// time, and answers others with a busy status.
type Responder struct {
	Transport  *transport.Transport
	Verifier   mcrypto.Verifier
	Salt       []byte
	Iterations int
	// Params are the device's MRP intervals, sent to the commissioner. nil sends none.
	Params *SessionParams
	// OnSession is called with each session PASE establishes.
	OnSession func(*transport.Session)
	Log       *slog.Logger

	mu   sync.Mutex
	busy bool
}

// Register starts answering PBKDFParamRequests on the Transport.
func (r *Responder) Register() {
	r.Transport.Handle(transport.SecureChannel, []uint8{OpPBKDFParamRequest}, r.serve)
}

func (r *Responder) serve(ctx context.Context, ex *transport.Exchange, first transport.Message) {
	defer ex.Close()
	log := r.Log.With("peer", ex.Session().Peer())

	if ex.Session().Secure() {
		log.Info("Refused PASE over a secure session")
		r.status(ctx, ex, GeneralFailure, CodeInvalidParameter)
		return
	}
	r.mu.Lock()
	if r.busy {
		r.mu.Unlock()
		log.Info("Refused PASE while another is in progress")
		r.status(ctx, ex, GeneralBusy, CodeBusy)
		return
	}
	r.busy = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.busy = false
		r.mu.Unlock()
	}()

	s, err := r.handshake(ctx, ex, first)
	if err != nil {
		log.Info("PASE failed", "err", err)
		return
	}
	defer removeOnPanic(r.Transport, s)
	log.Info("Established PASE session", "session", s.LocalSessionID())
	if r.OnSession != nil {
		r.OnSession(s)
	}
}

func (r *Responder) handshake(ctx context.Context, ex *transport.Exchange, first transport.Message) (*transport.Session, error) {
	req, err := decodePBKDFParamRequest(first.Payload)
	if err != nil {
		r.status(ctx, ex, GeneralFailure, CodeInvalidParameter)
		return nil, err
	}
	if req.passcodeID != 0 {
		r.status(ctx, ex, GeneralFailure, CodeInvalidParameter)
		return nil, fmt.Errorf("%w: passcode ID %d", ErrInvalidMessage, req.passcodeID)
	}
	tr := r.Transport
	localID, err := tr.AllocateSessionID()
	if err != nil {
		r.status(ctx, ex, GeneralBusy, CodeBusy)
		return nil, err
	}
	established := false
	defer func() {
		if !established {
			tr.ReleaseSessionID(localID)
		}
	}()

	resp := &pbkdfParamResponse{
		initiatorRandom:    req.initiatorRandom,
		responderRandom:    random(),
		responderSessionID: localID,
		params:             r.Params,
	}
	if !req.hasPBKDFParameters {
		resp.iterations, resp.salt = uint32(r.Iterations), r.Salt
	}
	respBytes := resp.encode()
	if err := ex.Send(ctx, message(OpPBKDFParamResponse, respBytes)); err != nil {
		return nil, err
	}
	t := mcrypto.Transcript{Context: paseContext(first.Payload, respBytes)}

	pake1, err := expectWithin(ctx, ex, OpPake1, ErrPASEFailed)
	if err != nil {
		return nil, err
	}
	f, err := decodePake(pake1, mcrypto.PointSize)
	if err != nil {
		r.status(ctx, ex, GeneralFailure, CodeInvalidParameter)
		return nil, err
	}
	y, err := mcrypto.RandomScalar()
	if err != nil {
		return nil, err
	}
	shareY, keys, err := t.VerifierFinish(r.Verifier, y, f[0])
	if err != nil {
		r.status(ctx, ex, GeneralFailure, CodeInvalidParameter)
		return nil, err
	}
	if err := ex.Send(ctx, message(OpPake2, encodePake(shareY, keys.CB))); err != nil {
		return nil, err
	}

	pake3, err := expectWithin(ctx, ex, OpPake3, ErrPASEFailed)
	if err != nil {
		return nil, err
	}
	f, err = decodePake(pake3, sha256.Size)
	if err != nil || subtle.ConstantTimeCompare(f[0], keys.CA) != 1 {
		// A wrong passcode lands here: the commissioner's confirmation doesn't match.
		r.status(ctx, ex, GeneralFailure, CodeInvalidParameter)
		if err == nil {
			err = fmt.Errorf("%w: the commissioner's confirmation doesn't match", ErrPASEFailed)
		}
		return nil, err
	}

	sk, err := deriveSessionKeys(keys.Ke, nil, paseKeysInfo)
	if err != nil {
		return nil, err
	}
	sec, err := NewSecurity(sk, false, 0, 0)
	if err != nil {
		return nil, err
	}
	// Add the session before reporting success, so the commissioner's first secure message
	// finds it.
	s := tr.AddSecureSession(transport.SecureSessionConfig{
		LocalSessionID: localID, PeerSessionID: req.initiatorSessionID,
		Peer: ex.Session().Peer(), Security: sec, PeerMRP: req.params.MRP(),
		Auth: transport.AuthPASE, AttestationChallenge: sk.AttestationChallenge,
	})
	defer removeOnPanic(tr, s)
	established = true
	// Keep the session only if the commissioner hears that it was established.
	ackCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	if err := ex.Send(ackCtx, secureChannelStatus(GeneralSuccess, CodeSessionEstablishmentSuccess).Message()); err != nil {
		tr.RemoveSession(s)
		return nil, err
	}
	if err := ex.WaitAck(ackCtx); err != nil {
		tr.RemoveSession(s)
		return nil, fmt.Errorf("the commissioner didn't acknowledge success: %w", err)
	}
	return s, nil
}

// expectWithin is expect, waiting at most stepTimeout.
func expectWithin(ctx context.Context, ex *transport.Exchange, op uint8, failed error) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	return expect(ctx, ex, op, failed)
}

func (r *Responder) status(ctx context.Context, ex *transport.Exchange, general, code uint16) {
	if err := ex.Send(ctx, secureChannelStatus(general, code).Message()); err != nil {
		r.Log.Debug("Could not send status report", "err", err)
	}
}

// EstablishPASE runs PASE with a device as the commissioner, and returns the secure session.
// Params are the commissioner's MRP intervals, sent to the device; nil sends none.
func EstablishPASE(ctx context.Context, tr *transport.Transport, device netip.AddrPort, passcode uint32, params *SessionParams) (s *transport.Session, err error) {
	ctx, cancel := context.WithTimeout(ctx, paseTimeout)
	defer cancel()
	unsecured := tr.NewUnsecuredSession(device)
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

	req := &pbkdfParamRequest{initiatorRandom: random(), initiatorSessionID: localID, params: params}
	reqBytes := req.encode()
	if err := ex.Send(ctx, message(OpPBKDFParamRequest, reqBytes)); err != nil {
		return nil, err
	}
	respBytes, err := expect(ctx, ex, OpPBKDFParamResponse, ErrPASEFailed)
	if err != nil {
		return nil, err
	}
	// From here the device holds a handshake for us. If we give up, tell it, so it's free for
	// the next attempt, unless it gave up first.
	defer func() {
		if err != nil && !errors.As(err, new(StatusReport)) {
			abort(ex)
		}
	}()
	resp, err := decodePBKDFParamResponse(respBytes)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(resp.initiatorRandom, req.initiatorRandom) != 1 {
		return nil, fmt.Errorf("%w: the response is for another request", ErrInvalidMessage)
	}
	if resp.salt == nil {
		return nil, fmt.Errorf("%w: no PBKDF2 parameters", ErrInvalidMessage)
	}
	w0, w1, err := mcrypto.PBKDFScalars(passcode, resp.salt, int(resp.iterations))
	if err != nil {
		return nil, err
	}
	t := mcrypto.Transcript{Context: paseContext(reqBytes, respBytes)}

	x, err := mcrypto.RandomScalar()
	if err != nil {
		return nil, err
	}
	shareX := mcrypto.ProverShare(w0, x)
	if err := ex.Send(ctx, message(OpPake1, encodePake(shareX))); err != nil {
		return nil, err
	}
	pake2, err := expect(ctx, ex, OpPake2, ErrPASEFailed)
	if err != nil {
		return nil, err
	}
	f, err := decodePake(pake2, mcrypto.PointSize, sha256.Size)
	if err != nil {
		return nil, err
	}
	keys, err := t.ProverFinish(w0, w1, x, shareX, f[0])
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(f[1], keys.CB) != 1 {
		return nil, fmt.Errorf("%w: the device's confirmation doesn't match", ErrPASEFailed)
	}
	if err := ex.Send(ctx, message(OpPake3, encodePake(keys.CA))); err != nil {
		return nil, err
	}
	m, err := ex.Recv(ctx)
	if err != nil {
		return nil, err
	}
	if m.Protocol != transport.SecureChannel || m.Opcode != OpStatusReport {
		return nil, fmt.Errorf("%w: opcode %#x instead of a status report", ErrInvalidMessage, m.Opcode)
	}
	if sr, err := ParseStatusReport(m.Payload); err != nil {
		return nil, err
	} else if !sr.Success() {
		return nil, fmt.Errorf("%w: %w", ErrPASEFailed, sr)
	}

	sk, err := deriveSessionKeys(keys.Ke, nil, paseKeysInfo)
	if err != nil {
		return nil, err
	}
	sec, err := NewSecurity(sk, true, 0, 0)
	if err != nil {
		return nil, err
	}
	established = true
	return tr.AddSecureSession(transport.SecureSessionConfig{
		LocalSessionID: localID, PeerSessionID: resp.responderSessionID,
		Peer: device, Security: sec, PeerMRP: resp.params.MRP(),
		Auth: transport.AuthPASE, AttestationChallenge: sk.AttestationChallenge,
	}), nil
}

// abort tells the peer that the handshake failed, and waits briefly for its acknowledgement so
// that MRP has a chance to deliver it.
func abort(ex *transport.Exchange) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ex.Send(ctx, secureChannelStatus(GeneralFailure, CodeInvalidParameter).Message()) == nil {
		_ = ex.WaitAck(ctx)
	}
}

// paseContext is SPAKE2+'s context for PASE: a hash of the two PBKDF parameter messages.
func paseContext(req, resp []byte) []byte {
	h := sha256.New()
	h.Write([]byte(paseContextPrefix))
	h.Write(req)
	h.Write(resp)
	return h.Sum(nil)
}

// expect waits for a Secure Channel message with the given opcode. A status report instead is
// the peer giving up, and is returned as an error wrapping failed and the StatusReport.
func expect(ctx context.Context, ex *transport.Exchange, op uint8, failed error) ([]byte, error) {
	m, err := ex.Recv(ctx)
	if err != nil {
		return nil, err
	}
	if m.Protocol != transport.SecureChannel {
		return nil, fmt.Errorf("%w: protocol %#x", ErrInvalidMessage, m.Protocol.ID)
	}
	if m.Opcode == OpStatusReport {
		sr, err := ParseStatusReport(m.Payload)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", failed, sr)
	}
	if m.Opcode != op {
		return nil, fmt.Errorf("%w: opcode %#x, want %#x", ErrInvalidMessage, m.Opcode, op)
	}
	return m.Payload, nil
}

func message(op uint8, payload []byte) transport.Message {
	return transport.Message{Protocol: transport.SecureChannel, Opcode: op, Payload: payload}
}

func random() []byte {
	b := make([]byte, randomSize)
	_, _ = rand.Read(b)
	return b
}
