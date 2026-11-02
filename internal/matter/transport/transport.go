// Package transport is the Matter message layer over UDP (Matter Core Specification, chapter 4):
// message and protocol headers, sessions, exchanges, duplicate detection, and the Message
// Reliability Protocol (MRP).
//
// A Transport owns one UDP socket. Unsolicited messages, the first of an exchange that a peer
// starts, go to the Handler registered for their protocol. Every other message goes to the
// Exchange it belongs to. Sessions are unsecured, for commissioning before PASE, or secure, with a
// Security that encrypts their messages.
package transport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Port is the UDP port Matter nodes listen on.
const Port = 5540

// MaxMessageSize is the largest message a Transport sends or accepts: the IPv6 minimum MTU less
// the IPv6 and UDP headers (Core spec, 4.4.4).
const MaxMessageSize = 1280 - 40 - 8

// Errors from exchanges and sessions.
var (
	ErrNoAck            = errors.New("matter: no acknowledgement after the last retransmission")
	ErrClosed           = errors.New("matter: exchange closed")
	ErrTooLarge         = fmt.Errorf("matter: message larger than %d bytes", MaxMessageSize)
	ErrSessionIDsInUse  = errors.New("matter: no free session ID")
	ErrTooManyExchanges = errors.New("matter: too many exchanges on the session")
)

// Limits on what a peer can make a Transport hold.
const (
	maxUnsecuredSessions = 16 // that peers start
	maxExchanges         = 32 // per session
	maxInbox             = 8  // messages waiting for an exchange's Recv
)

// Message is an application message: its protocol, opcode and payload.
type Message struct {
	Protocol ProtocolID
	Opcode   uint8
	Payload  []byte
}

// Handler serves an exchange that a peer started. msg is its first message. The Handler runs in
// its own goroutine and should Close the exchange when it's done.
type Handler func(ctx context.Context, ex *Exchange, msg Message)

// Security seals and opens the messages of a secure session. hdr is the encoded message header,
// which is authenticated but not encrypted.
type Security interface {
	// Seal encrypts and authenticates payload, and returns what follows the header, the integrity
	// check included.
	Seal(h *Header, hdr, payload []byte) ([]byte, error)
	// Open checks and decrypts what follows the header, and returns the payload.
	Open(h *Header, hdr, sealed []byte) ([]byte, error)
}

// Options configures a Transport.
type Options struct {
	Log logr.Logger
	// MRP is what the Transport assumes of a peer until a session says otherwise. Zero fields
	// take the spec's defaults.
	MRP MRPConfig
}

// Transport is the Matter message layer on one UDP socket.
type Transport struct {
	conn net.PacketConn
	log  logr.Logger
	mrp  MRPConfig

	mu        sync.Mutex
	handlers  map[handlerKey]Handler
	unsecured map[unsecuredKey]*Session
	secure    map[uint16]*Session // by local session ID
	counter   uint32              // the global unencrypted message counter
	ctx       context.Context     // Serve's, for Handlers
	handling  sync.WaitGroup      // running Handlers
}

type unsecuredKey struct {
	node      uint64 // the initiator's ephemeral node ID
	initiator bool   // we are the initiator
}

// Listen opens a UDP socket on addr, such as "[::]:5540", and returns a Transport on it.
func Listen(addr string, opts Options) (*Transport, error) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	return New(conn, opts), nil
}

// New returns a Transport on conn. Serve reads from it.
func New(conn net.PacketConn, opts Options) *Transport {
	return &Transport{
		conn:      conn,
		log:       opts.Log,
		mrp:       opts.MRP.withDefaults(),
		handlers:  map[handlerKey]Handler{},
		unsecured: map[unsecuredKey]*Session{},
		secure:    map[uint16]*Session{},
		counter:   initialCounter(),
		ctx:       context.Background(),
	}
}

// Addr returns the address the Transport listens on.
func (t *Transport) Addr() net.Addr { return t.conn.LocalAddr() }

type handlerKey struct {
	protocol ProtocolID
	opcode   uint8
}

// Handle registers h for exchanges that peers start in protocol p with one of the given opcodes,
// such as a Read Request, replacing any Handler registered for them before. Only these start an
// exchange: any other message without one, such as a late Status Response to an exchange that
// has ended, is acknowledged and dropped.
func (t *Transport) Handle(p ProtocolID, opcodes []uint8, h Handler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, op := range opcodes {
		t.handlers[handlerKey{p, op}] = h
	}
}

// Serve reads and dispatches messages until ctx is done or the socket fails. It closes the
// socket and every session, and waits for the Handlers it started to return.
func (t *Transport) Serve(ctx context.Context) error {
	defer t.handling.Wait() // only this goroutine starts Handlers, so none start after this
	handlers, cancel := context.WithCancel(ctx)
	defer cancel() // Handlers stop when Serve does, whatever stopped it
	t.mu.Lock()
	t.ctx = handlers
	t.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = t.conn.Close() })
	defer stop()
	defer t.shutdown()

	buf := make([]byte, MaxMessageSize+1)
	for {
		n, from, err := t.conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		addr, ok := addrPort(from)
		if !ok {
			continue
		}
		if n > MaxMessageSize {
			t.drop(addr, "message too large", "bytes", n)
			continue
		}
		t.receive(append([]byte(nil), buf[:n]...), addr)
	}
}

// NewUnsecuredSession starts an unsecured session with peer, as its initiator, such as a
// commissioner before PASE.
func (t *Transport) NewUnsecuredSession(peer netip.AddrPort) *Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		node := ephemeralNodeID()
		key := unsecuredKey{node: node, initiator: true}
		if t.unsecured[key] != nil {
			continue
		}
		s := t.newSession(peer)
		s.unsecured, s.ephemeral, s.initiator = &key, node, true
		s.recv.rollover = true
		t.unsecured[key] = s
		return s
	}
}

// AuthMode is how a secure session's peer authenticated.
type AuthMode uint8

const (
	AuthPASE AuthMode = 1 // with the setup passcode
	AuthCASE AuthMode = 2 // with its operational certificate
)

// SecureSessionConfig describes a secure session that PASE or CASE established.
type SecureSessionConfig struct {
	LocalSessionID uint16 // from AllocateSessionID
	PeerSessionID  uint16
	Peer           netip.AddrPort
	LocalNodeID    uint64 // 0 for PASE
	PeerNodeID     uint64 // 0 for PASE
	Auth           AuthMode
	FabricIndex    uint8    // the local fabric CASE ran on; 0 for PASE until AddNOC binds it
	CATs           []uint32 // the CASE Authenticated Tags in the peer's NOC
	// AttestationChallenge is the session's attestation challenge, from its key derivation.
	AttestationChallenge []byte
	Security             Security
	// PeerMRP is what the peer said of its MRP intervals. Zero fields keep the Transport's.
	PeerMRP MRPConfig
}

// AllocateSessionID reserves a free local session ID for a secure session being established.
// AddSecureSession then uses it, or ReleaseSessionID frees it if establishment fails.
func (t *Transport) AllocateSessionID() (uint16, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := uint16(mrand.Uint32N(0xFFFF)) + 1
	for i := range 0xFFFF {
		id := uint16((int(start)-1+i)%0xFFFF) + 1 // 1 to 0xFFFF; 0 is the unsecured session
		if _, used := t.secure[id]; !used {
			t.secure[id] = nil
			return id, nil
		}
	}
	return 0, ErrSessionIDsInUse
}

// ReleaseSessionID frees a session ID from AllocateSessionID that no session took.
func (t *Transport) ReleaseSessionID(id uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.secure[id]; ok && s == nil {
		delete(t.secure, id)
	}
}

// AddSecureSession adds a secure session, and returns it.
func (t *Transport) AddSecureSession(c SecureSessionConfig) *Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old := t.secure[c.LocalSessionID]; old != nil {
		old.closeLocked() // a caller reusing an ID: don't orphan the session that had it
	}
	s := t.newSession(c.Peer)
	s.localID, s.peerID = c.LocalSessionID, c.PeerSessionID
	s.localNode, s.peerNode = c.LocalNodeID, c.PeerNodeID
	s.auth, s.fabric, s.cats = c.Auth, c.FabricIndex, slices.Clone(c.CATs)
	s.challenge = slices.Clone(c.AttestationChallenge)
	s.security = c.Security
	s.mrp = c.PeerMRP.or(t.mrp)
	s.counter = initialCounter()
	t.secure[c.LocalSessionID] = s
	return s
}

func (t *Transport) newSession(peer netip.AddrPort) *Session {
	return &Session{
		t:         t,
		peer:      peer,
		mrp:       t.mrp,
		exchanges: map[exchangeKey]*Exchange{},
		nextEx:    uint16(mrand.Uint32()),
		done:      make(chan struct{}),
	}
}

// receive handles one datagram. pkt is the Transport's to keep.
func (t *Transport) receive(pkt []byte, from netip.AddrPort) {
	h, n, err := ParseHeader(pkt)
	if err != nil {
		t.drop(from, err.Error())
		return
	}
	switch {
	case h.Privacy:
		t.drop(from, "privacy obfuscation is not supported")
		return
	case h.Control:
		t.drop(from, "control messages are not supported")
		return
	case h.SessionType != UnicastSession:
		t.drop(from, "group messages are not supported")
		return
	}

	var s *Session
	payload := pkt[n:]
	if h.SessionID != 0 {
		t.mu.Lock()
		s = t.secure[h.SessionID]
		t.mu.Unlock()
		if s == nil {
			t.drop(from, "unknown session", "session", h.SessionID)
			return
		}
		// Decrypt outside the lock: only this goroutine opens, and a session's Security is fixed.
		if payload, err = s.security.Open(&h, pkt[:n], payload); err != nil {
			t.drop(from, "message failed authentication", "session", h.SessionID)
			return
		}
	}
	ph, m, err := ParseProtocolHeader(payload)
	if err != nil {
		t.drop(from, err.Error(), "session", h.SessionID)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if s == nil {
		if s = t.unsecuredSession(&h, from); s == nil {
			return
		}
	} else if s.closed {
		t.drop(from, "session closed", "session", h.SessionID)
		return
	}

	verdict := s.recv.check(h.Counter)
	if verdict == counterTooOld {
		t.drop(from, "message counter behind the window", "session", h.SessionID, "counter", h.Counter)
		return
	}
	if verdict == counterDuplicate {
		if ph.Reliable {
			s.sendStandaloneAck(exchangeKey{id: ph.ExchangeID, initiator: !ph.Initiator}, h.Counter)
		}
		t.log.V(1).Info("Dropped duplicate message", "peer", from, "session", h.SessionID, "counter", h.Counter)
		return
	}
	if s.security != nil {
		s.peer = from // a secure session follows its peer's address
	}
	if !s.dispatch(&ph, h.Counter, Message{Protocol: ph.Protocol, Opcode: ph.Opcode, Payload: payload[m:]}) {
		return // not recorded or acknowledged, so the peer retransmits it
	}
	s.recv.accept(h.Counter)
	s.heard = time.Now()
}

// unsecuredSession finds or creates the unsecured session that h belongs to.
func (t *Transport) unsecuredSession(h *Header, from netip.AddrPort) *Session {
	switch {
	case h.HasSource && !h.HasDestNode && !h.HasDestGroup:
		// From an initiator: we are the responder.
		key := unsecuredKey{node: h.Source}
		if s := t.unsecured[key]; s != nil {
			return s
		}
		if !t.roomForResponder() {
			t.drop(from, "too many unsecured sessions")
			return nil
		}
		s := t.newSession(from)
		s.unsecured, s.ephemeral = &key, h.Source
		s.recv.rollover = true
		t.unsecured[key] = s
		return s
	case h.HasDestNode && !h.HasSource:
		// To us as the initiator.
		if s := t.unsecured[unsecuredKey{node: h.DestNode, initiator: true}]; s != nil {
			return s
		}
		t.drop(from, "unknown unsecured session")
	default:
		t.drop(from, "unsecured message without exactly one ephemeral node ID")
	}
	return nil
}

// roomForResponder makes room for one more unsecured session that a peer starts, by evicting
// the idle one heard from least recently if there are maxUnsecuredSessions. Sessions with an
// exchange in progress, such as a PASE, are never evicted, so a flood of unauthenticated
// messages can't interrupt one: it reports false instead.
func (t *Transport) roomForResponder() bool {
	var oldest *Session
	n := 0
	for k, s := range t.unsecured {
		if k.initiator {
			continue
		}
		n++
		if len(s.exchanges) == 0 && (oldest == nil || s.heard.Before(oldest.heard)) {
			oldest = s
		}
	}
	if n < maxUnsecuredSessions {
		return true
	}
	if oldest == nil {
		return false
	}
	t.log.V(1).Info("Evicted unsecured session", "peer", oldest.peer)
	oldest.closeLocked()
	return true
}

// SecureSessions returns how many secure sessions the Transport holds.
func (t *Transport) SecureSessions() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, s := range t.secure {
		if s != nil {
			n++
		}
	}
	return n
}

// RemoveSession closes s and all its exchanges.
func (t *Transport) RemoveSession(s *Session) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s.closeLocked()
}

func (t *Transport) shutdown() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.unsecured {
		s.closeLocked()
	}
	for _, s := range t.secure {
		if s != nil {
			s.closeLocked()
		}
	}
}

// write sends pkt to peer. The caller holds t.mu.
func (t *Transport) write(pkt []byte, peer netip.AddrPort) {
	if _, err := t.conn.WriteTo(pkt, net.UDPAddrFromAddrPort(peer)); err != nil {
		t.log.V(1).Info("Could not send message", "peer", peer, "error", err.Error())
	}
}

func (t *Transport) drop(from netip.AddrPort, reason string, kv ...any) {
	t.log.V(1).Info("Dropped message", append([]any{"peer", from, "reason", reason}, kv...)...)
}

func (t *Transport) nextCounter() uint32 {
	t.counter++
	return t.counter
}

func addrPort(a net.Addr) (netip.AddrPort, bool) {
	u, ok := a.(*net.UDPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := u.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
}

// initialCounter is a random start for a message counter, in [1, 2^28] (Core spec, 4.6.1.1).
func initialCounter() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])&(1<<28-1) + 1
}

// ephemeralNodeID is a random node ID in the operational range, for an unsecured session's
// initiator (Core spec, 4.13.2.1).
func ephemeralNodeID() uint64 {
	var b [8]byte
	for {
		_, _ = rand.Read(b[:])
		if id := binary.LittleEndian.Uint64(b[:]); id != 0 && id <= 0xFFFF_FFEF_FFFF_FFFF {
			return id
		}
	}
}
