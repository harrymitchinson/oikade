package transport

import (
	"context"
	"math"
	mrand "math/rand/v2"
	"net/netip"
	"time"
)

// MRPConfig holds the Message Reliability Protocol's timing (Core spec, 4.12.8). Idle, Active and
// ActiveThreshold describe the peer: how long it may sleep, and so how long to wait before
// retransmitting to it.
type MRPConfig struct {
	Idle                 time.Duration // SESSION_IDLE_INTERVAL: the peer's retransmission base when idle
	Active               time.Duration // SESSION_ACTIVE_INTERVAL: and when active
	ActiveThreshold      time.Duration // how long after hearing from it the peer counts as active
	StandaloneAckTimeout time.Duration // how long to wait for a message to piggyback an ack on
	MaxTransmissions     int           // MRP_MAX_TRANSMISSIONS, the first one included
}

// The spec's MRP defaults and constants.
const (
	defaultIdle             = 500 * time.Millisecond
	defaultActive           = 300 * time.Millisecond
	defaultActiveThreshold  = 4 * time.Second
	defaultStandaloneAck    = 200 * time.Millisecond
	defaultMaxTransmissions = 5
	backoffBase             = 1.6
	backoffJitter           = 0.25
	backoffMargin           = 1.1
	backoffThreshold        = 1
)

func (c MRPConfig) withDefaults() MRPConfig {
	return c.or(MRPConfig{
		Idle: defaultIdle, Active: defaultActive, ActiveThreshold: defaultActiveThreshold,
		StandaloneAckTimeout: defaultStandaloneAck, MaxTransmissions: defaultMaxTransmissions,
	})
}

// or fills c's zero fields from d.
func (c MRPConfig) or(d MRPConfig) MRPConfig {
	if c.Idle == 0 {
		c.Idle = d.Idle
	}
	if c.Active == 0 {
		c.Active = d.Active
	}
	if c.ActiveThreshold == 0 {
		c.ActiveThreshold = d.ActiveThreshold
	}
	if c.StandaloneAckTimeout == 0 {
		c.StandaloneAckTimeout = d.StandaloneAckTimeout
	}
	if c.MaxTransmissions == 0 {
		c.MaxTransmissions = d.MaxTransmissions
	}
	return c
}

// Session is an unsecured or secure session with one peer. Its fields are guarded by the
// Transport's mutex.
type Session struct {
	t    *Transport
	peer netip.AddrPort
	mrp  MRPConfig

	// Unsecured sessions: the initiator's ephemeral node ID, and whether we are the initiator.
	unsecured *unsecuredKey
	ephemeral uint64
	initiator bool

	// Secure sessions.
	localID, peerID     uint16
	localNode, peerNode uint64
	fabric              uint8
	peerCATs            []uint32
	security            Security
	counter             uint32

	recv      reception
	heard     time.Time
	exchanges map[exchangeKey]*Exchange
	nextEx    uint16
	closed    bool
	done      chan struct{} // closed when the session is
}

type exchangeKey struct {
	id        uint16
	initiator bool // we started it
}

// Peer returns the peer's address, which a secure session updates from the messages it receives.
func (s *Session) Peer() netip.AddrPort {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	return s.peer
}

// Done returns a channel that's closed when the session is.
func (s *Session) Done() <-chan struct{} { return s.done }

// Secure reports whether the session encrypts its messages.
func (s *Session) Secure() bool { return s.security != nil }

// LocalSessionID returns a secure session's local ID, and 0 for an unsecured one.
func (s *Session) LocalSessionID() uint16 { return s.localID }

// PeerNodeID returns the peer's operational node ID, 0 if it has none (unsecured and PASE).
func (s *Session) PeerNodeID() uint64 { return s.peerNode }

// FabricIndex returns the local fabric a CASE session was established on, 0 for others.
func (s *Session) FabricIndex() uint8 { return s.fabric }

// PeerCATs returns the CASE Authenticated Tags in a CASE peer's NOC. The caller mustn't modify
// them.
func (s *Session) PeerCATs() []uint32 { return s.peerCATs }

// NewExchange starts an exchange on the session, as its initiator.
func (s *Session) NewExchange() (*Exchange, error) {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if len(s.exchanges) >= maxExchanges {
		return nil, ErrTooManyExchanges
	}
	for {
		s.nextEx++
		if s.exchanges[exchangeKey{s.nextEx, true}] == nil {
			return s.newExchange(s.nextEx, true), nil
		}
	}
}

func (s *Session) newExchange(id uint16, initiator bool) *Exchange {
	ex := &Exchange{s: s, key: exchangeKey{id, initiator}, changed: make(chan struct{})}
	s.exchanges[ex.key] = ex
	return ex
}

// dispatch hands a new message to its exchange, or to a Handler if it starts one. It returns
// false if the message can't be taken now: it is then neither recorded nor acknowledged, so the
// peer retransmits it.
func (s *Session) dispatch(ph *ProtocolHeader, counter uint32, msg Message) bool {
	key := exchangeKey{id: ph.ExchangeID, initiator: !ph.Initiator}
	ex := s.exchanges[key]
	if ph.Ack && ex != nil {
		ex.acked(ph.AckCounter)
	}
	if ph.Protocol == SecureChannel && ph.Opcode == OpStandaloneAck {
		return true
	}
	if ex == nil {
		h, ok := s.t.handlers[handlerKey{ph.Protocol, ph.Opcode}]
		if !ph.Initiator || !ok {
			// Nothing will ever take it: acknowledge it, so the peer stops sending it.
			if ph.Reliable {
				s.sendStandaloneAck(key, counter)
			}
			s.t.drop(s.peer, "no exchange for message", "exchange", ph.ExchangeID, "protocol", ph.Protocol.ID, "opcode", ph.Opcode)
			return true
		}
		if len(s.exchanges) >= maxExchanges {
			s.t.drop(s.peer, "too many exchanges", "exchange", ph.ExchangeID)
			return false
		}
		ex = s.newExchange(ph.ExchangeID, false)
		if ph.Reliable {
			ex.needAck(counter)
		}
		s.t.handling.Go(func() { h(s.t.ctx, ex, msg) })
		return true
	}
	if ex.closed {
		if ph.Reliable {
			s.sendStandaloneAck(key, counter)
		}
		s.t.drop(s.peer, "exchange closed", "exchange", ph.ExchangeID)
		return true
	}
	if len(ex.inbox) >= maxInbox {
		s.t.drop(s.peer, "exchange inbox full", "exchange", ph.ExchangeID)
		return false
	}
	if ph.Reliable {
		ex.needAck(counter)
	}
	ex.inbox = append(ex.inbox, msg)
	ex.signal()
	return true
}

// encode builds a message on the session, with the next message counter.
func (s *Session) encode(ph *ProtocolHeader, app []byte) ([]byte, uint32, error) {
	var h Header
	if s.security == nil {
		h.Counter = s.t.nextCounter()
		if s.initiator {
			h.HasSource, h.Source = true, s.ephemeral
		} else {
			h.HasDestNode, h.DestNode = true, s.ephemeral
		}
	} else {
		s.counter++
		h.Counter, h.SessionID = s.counter, s.peerID
	}
	pkt := h.Append(make([]byte, 0, MaxMessageSize))
	payload := append(ph.Append(make([]byte, 0, 32+len(app))), app...)
	if len(pkt)+len(payload) > MaxMessageSize {
		return nil, 0, ErrTooLarge // before sealing, which only adds to it
	}
	if s.security == nil {
		pkt = append(pkt, payload...)
	} else {
		sealed, err := s.security.Seal(&h, pkt, payload)
		if err != nil {
			return nil, 0, err
		}
		pkt = append(pkt, sealed...)
	}
	if len(pkt) > MaxMessageSize {
		return nil, 0, ErrTooLarge
	}
	return pkt, h.Counter, nil
}

func (s *Session) sendStandaloneAck(key exchangeKey, counter uint32) {
	ph := ProtocolHeader{
		Initiator: key.initiator, Ack: true, AckCounter: counter,
		Protocol: SecureChannel, Opcode: OpStandaloneAck, ExchangeID: key.id,
	}
	pkt, _, err := s.encode(&ph, nil)
	if err != nil {
		s.t.log.Error(err, "Could not encode standalone acknowledgement")
		return
	}
	s.t.write(pkt, s.peer)
}

// retransmitAfter is how long to wait after transmission n (from 0) of a reliable message
// (Core spec, 4.12.2.1).
func (s *Session) retransmitAfter(n int) time.Duration {
	base := s.mrp.Idle
	if !s.heard.IsZero() && time.Since(s.heard) < s.mrp.ActiveThreshold {
		base = s.mrp.Active
	}
	f := float64(base) * backoffMargin * math.Pow(backoffBase, float64(max(0, n-backoffThreshold)))
	return time.Duration(f * (1 + mrand.Float64()*backoffJitter))
}

func (s *Session) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
	for _, ex := range s.exchanges {
		ex.fail(ErrClosed)
	}
	if s.unsecured != nil {
		delete(s.t.unsecured, *s.unsecured)
	} else if s.t.secure[s.localID] == s {
		delete(s.t.secure, s.localID)
	}
}

// Exchange is one conversation on a session: a request and its responses (Core spec, 4.10).
// Messages sent on it are reliable: MRP retransmits each one until the peer acknowledges it.
type Exchange struct {
	s   *Session
	key exchangeKey

	inbox   []Message
	err     error // set when the exchange has failed
	closed  bool
	changed chan struct{} // closed and replaced on every change, to wake waiters

	pendingAck bool
	ackCounter uint32
	ackTimer   *time.Timer

	out *retransmission // the reliable message waiting for its ack
}

type retransmission struct {
	counter uint32
	pkt     []byte
	sends   int
	timer   *time.Timer
}

// Session returns the exchange's session.
func (ex *Exchange) Session() *Session { return ex.s }

// ID returns the exchange ID.
func (ex *Exchange) ID() uint16 { return ex.key.id }

// Send sends m reliably. It first waits until the peer has acknowledged the previous message, as
// MRP allows one in flight per exchange. Any acknowledgement the exchange owes rides along.
func (ex *Exchange) Send(ctx context.Context, m Message) error {
	t := ex.s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	for ex.out != nil && ex.err == nil && !ex.closed {
		if err := ex.wait(ctx); err != nil {
			return err
		}
	}
	if ex.err != nil {
		return ex.err
	}
	if ex.closed || ex.s.closed {
		return ErrClosed
	}
	ph := ProtocolHeader{
		Initiator: ex.key.initiator, Reliable: true,
		Opcode: m.Opcode, ExchangeID: ex.key.id, Protocol: m.Protocol,
		Ack: ex.pendingAck, AckCounter: ex.ackCounter,
	}
	pkt, counter, err := ex.s.encode(&ph, m.Payload)
	if err != nil {
		return err
	}
	ex.clearAck()
	ex.out = &retransmission{counter: counter, pkt: pkt}
	ex.transmit(ex.out)
	return nil
}

// Recv waits for the next message on the exchange.
func (ex *Exchange) Recv(ctx context.Context) (Message, error) {
	t := ex.s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		if len(ex.inbox) > 0 {
			m := ex.inbox[0]
			ex.inbox = ex.inbox[1:]
			return m, nil
		}
		if ex.err != nil {
			return Message{}, ex.err
		}
		if ex.closed {
			return Message{}, ErrClosed
		}
		if err := ex.wait(ctx); err != nil {
			return Message{}, err
		}
	}
}

// WaitAck waits until the peer has acknowledged the last message sent. It returns ErrNoAck if
// MRP gave up.
func (ex *Exchange) WaitAck(ctx context.Context) error {
	t := ex.s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	for ex.out != nil && ex.err == nil {
		if err := ex.wait(ctx); err != nil {
			return err
		}
	}
	return ex.err
}

// Close ends the exchange. It sends any acknowledgement it owes at once. A message still waiting
// for its acknowledgement keeps being retransmitted until it gets one or MRP gives up.
func (ex *Exchange) Close() {
	t := ex.s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if ex.closed {
		return
	}
	ex.closed = true
	ex.flushAck()
	if ex.out == nil {
		ex.remove()
	}
	ex.signal()
}

// wait releases the lock until the exchange changes or ctx is done. The caller holds t.mu.
func (ex *Exchange) wait(ctx context.Context) error {
	ch := ex.changed
	ex.s.t.mu.Unlock()
	defer ex.s.t.mu.Lock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (ex *Exchange) signal() {
	close(ex.changed)
	ex.changed = make(chan struct{})
}

func (ex *Exchange) transmit(r *retransmission) {
	ex.s.t.write(r.pkt, ex.s.peer)
	after := ex.s.retransmitAfter(r.sends)
	r.sends++
	r.timer = time.AfterFunc(after, func() { ex.retransmit(r) })
}

func (ex *Exchange) retransmit(r *retransmission) {
	t := ex.s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if ex.out != r {
		return
	}
	if r.sends >= ex.s.mrp.MaxTransmissions {
		t.log.V(1).Info("Gave up on message", "peer", ex.s.peer, "exchange", ex.key.id, "counter", r.counter)
		ex.fail(ErrNoAck)
		return
	}
	ex.transmit(r)
}

// acked handles an acknowledgement of counter.
func (ex *Exchange) acked(counter uint32) {
	if ex.out == nil || ex.out.counter != counter {
		return
	}
	ex.out.timer.Stop()
	ex.out = nil
	if ex.closed {
		ex.remove()
	}
	ex.signal()
}

// needAck records that the exchange owes an acknowledgement of counter, and sends it on its own
// if no message carries it within the standalone ack timeout.
func (ex *Exchange) needAck(counter uint32) {
	if ex.pendingAck && ex.ackCounter != counter {
		ex.flushAck() // only one can ride along: send the older one now
	}
	ex.pendingAck, ex.ackCounter = true, counter
	if ex.ackTimer == nil {
		var timer *time.Timer
		timer = time.AfterFunc(ex.s.mrp.StandaloneAckTimeout, func() {
			ex.s.t.mu.Lock()
			defer ex.s.t.mu.Unlock()
			if ex.ackTimer == timer { // not stopped and replaced while this waited for the lock
				ex.flushAck()
			}
		})
		ex.ackTimer = timer
	}
}

// flushAck sends the acknowledgement the exchange owes, if any, on its own.
func (ex *Exchange) flushAck() {
	if ex.pendingAck {
		ex.s.sendStandaloneAck(ex.key, ex.ackCounter)
	}
	ex.clearAck()
}

func (ex *Exchange) clearAck() {
	ex.pendingAck = false
	if ex.ackTimer != nil {
		ex.ackTimer.Stop()
		ex.ackTimer = nil
	}
}

// fail ends the exchange with err.
func (ex *Exchange) fail(err error) {
	if ex.err != nil {
		return
	}
	ex.err = err
	if ex.out != nil {
		ex.out.timer.Stop()
		ex.out = nil
	}
	ex.clearAck()
	ex.remove()
	ex.signal()
}

func (ex *Exchange) remove() {
	if ex.s.exchanges[ex.key] == ex {
		delete(ex.s.exchanges, ex.key)
	}
}
