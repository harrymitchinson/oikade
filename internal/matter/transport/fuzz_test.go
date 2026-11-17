package transport

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
)

// discardConn is a socket that sends nowhere and has nothing to read.
type discardConn struct{}

func (discardConn) ReadFrom([]byte) (int, net.Addr, error)    { return 0, nil, net.ErrClosed }
func (discardConn) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }
func (discardConn) Close() error                              { return nil }
func (discardConn) LocalAddr() net.Addr                       { return &net.UDPAddr{} }
func (discardConn) SetDeadline(time.Time) error               { return nil }
func (discardConn) SetReadDeadline(time.Time) error           { return nil }
func (discardConn) SetWriteDeadline(time.Time) error          { return nil }

// plainSecurity seals nothing, so that the fuzzer reaches what follows decryption on a secure
// session, as a peer holding the session's keys would.
type plainSecurity struct{}

func (plainSecurity) Seal(_ *Header, _, payload []byte) ([]byte, error) {
	return slices.Clone(payload), nil
}
func (plainSecurity) Open(_ *Header, _, sealed []byte) ([]byte, error) { return sealed, nil }

// FuzzReceive feeds Transport.receive a datagram, as any host on the network can send one before
// authenticating, twice, since a peer retransmits. It must not panic: Serve contains a panic, but
// one costs the session it came on.
func FuzzReceive(f *testing.F) {
	const secureID = 1
	message := func(h Header, ph ProtocolHeader, payload ...byte) []byte {
		return append(ph.Append(h.Append(nil)), payload...)
	}
	// A PBKDFParamRequest that starts an exchange on a new unsecured session, an acknowledgement on
	// it, and a message on a session with no exchange.
	f.Add(message(Header{HasSource: true, Source: 0x1122334455667788, Counter: 1},
		ProtocolHeader{Initiator: true, Reliable: true, Protocol: SecureChannel, Opcode: 0x20, ExchangeID: 7},
		0x15, 0x18))
	f.Add(message(Header{HasSource: true, Source: 0x1122334455667788, Counter: 2},
		ProtocolHeader{Ack: true, AckCounter: 9, Protocol: SecureChannel, Opcode: OpStandaloneAck, ExchangeID: 7}))
	f.Add(message(Header{HasDestNode: true, DestNode: 0x1122334455667788, Counter: 3},
		ProtocolHeader{Reliable: true, Protocol: SecureChannel, Opcode: 0x21, ExchangeID: 7}))
	// On the secure session: a Read Request that starts an exchange, and one that no Handler takes.
	f.Add(message(Header{SessionID: secureID, Counter: 100},
		ProtocolHeader{Initiator: true, Reliable: true, Protocol: InteractionModel, Opcode: 0x02, ExchangeID: 8},
		0x15, 0x18))
	f.Add(message(Header{SessionID: secureID, Counter: 101},
		ProtocolHeader{Initiator: true, Reliable: true, Protocol: InteractionModel, Opcode: 0x08, ExchangeID: 9}))
	f.Add(message(Header{SessionID: 2, Counter: 1}, ProtocolHeader{Protocol: InteractionModel, Opcode: 0x02}))

	peer := netip.MustParseAddrPort("[fd00::1]:5540")
	f.Fuzz(func(t *testing.T, pkt []byte) {
		tr := New(discardConn{}, Options{Log: logging.Discard()})
		tr.Handle(SecureChannel, []uint8{0x20}, func(_ context.Context, ex *Exchange, _ Message) { ex.Close() })
		tr.Handle(InteractionModel, []uint8{0x02}, func(_ context.Context, ex *Exchange, _ Message) { ex.Close() })
		tr.AddSecureSession(SecureSessionConfig{LocalSessionID: secureID, PeerSessionID: 2, Peer: peer, Security: plainSecurity{}})
		for range 2 {
			var on *Session
			tr.receive(slices.Clone(pkt), peer, &on)
		}
		tr.handling.Wait()
		tr.shutdown()
	})
}
