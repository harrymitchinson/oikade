package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrMalformed is wrapped by every error from decoding a header.
var ErrMalformed = errors.New("malformed Matter message")

// SessionType is the Session Type field of a message's security flags.
type SessionType uint8

// Session types.
const (
	UnicastSession SessionType = 0
	GroupSession   SessionType = 1
)

// Message flags (Core spec, 4.4.1.1).
const (
	flagSource    = 1 << 2 // S: the source node ID is present
	flagDSIZMask  = 0x03   // DSIZ: what the destination field holds
	dsizNone      = 0
	dsizNode      = 1
	dsizGroup     = 2
	versionMask   = 0xF0
	versionShift  = 4
	headerVersion = 0
)

// Security flags (4.4.1.3).
const (
	secPrivacy    = 1 << 7
	secControl    = 1 << 6
	secExtensions = 1 << 5
	secTypeMask   = 0x03
)

// Header is a message header (Core spec, 4.4.1): the part of a message sent in the clear, and the
// additional authenticated data of a secure message.
type Header struct {
	SessionID   uint16
	SessionType SessionType
	Privacy     bool // P: the header is obfuscated. Not supported
	Control     bool // C: a control message. Not supported
	Counter     uint32

	HasSource bool
	Source    uint64

	HasDestNode  bool
	DestNode     uint64
	HasDestGroup bool
	DestGroup    uint16

	// Extensions are the message extensions, which no version of the spec defines yet. nil means
	// absent; an empty slice is sent as present with length 0.
	Extensions []byte
}

// Append appends the encoded header to b.
func (h *Header) Append(b []byte) []byte {
	flags := byte(headerVersion << versionShift)
	if h.HasSource {
		flags |= flagSource
	}
	switch {
	case h.HasDestNode:
		flags |= dsizNode
	case h.HasDestGroup:
		flags |= dsizGroup
	}
	sec := byte(h.SessionType) & secTypeMask
	if h.Privacy {
		sec |= secPrivacy
	}
	if h.Control {
		sec |= secControl
	}
	if h.Extensions != nil {
		sec |= secExtensions
	}
	b = append(b, flags)
	b = binary.LittleEndian.AppendUint16(b, h.SessionID)
	b = append(b, sec)
	b = binary.LittleEndian.AppendUint32(b, h.Counter)
	if h.HasSource {
		b = binary.LittleEndian.AppendUint64(b, h.Source)
	}
	switch {
	case h.HasDestNode:
		b = binary.LittleEndian.AppendUint64(b, h.DestNode)
	case h.HasDestGroup:
		b = binary.LittleEndian.AppendUint16(b, h.DestGroup)
	}
	if h.Extensions != nil {
		b = binary.LittleEndian.AppendUint16(b, uint16(len(h.Extensions)))
		b = append(b, h.Extensions...)
	}
	return b
}

// ParseHeader decodes the message header at the start of b, and returns it with its length.
func ParseHeader(b []byte) (Header, int, error) {
	var h Header
	if len(b) < 8 {
		return h, 0, fmt.Errorf("%w: %d bytes is too short for a header", ErrMalformed, len(b))
	}
	flags := b[0]
	if v := flags >> versionShift; v != headerVersion {
		return h, 0, fmt.Errorf("%w: header version %d", ErrMalformed, v)
	}
	h.SessionID = binary.LittleEndian.Uint16(b[1:])
	sec := b[3]
	h.SessionType = SessionType(sec & secTypeMask)
	h.Privacy = sec&secPrivacy != 0
	h.Control = sec&secControl != 0
	h.Counter = binary.LittleEndian.Uint32(b[4:])
	off := 8

	need := func(n int, what string) error {
		if len(b)-off < n {
			return fmt.Errorf("%w: truncated %s", ErrMalformed, what)
		}
		return nil
	}
	if flags&flagSource != 0 {
		if err := need(8, "source node ID"); err != nil {
			return h, 0, err
		}
		h.HasSource, h.Source = true, binary.LittleEndian.Uint64(b[off:])
		off += 8
	}
	switch flags & flagDSIZMask {
	case dsizNone:
	case dsizNode:
		if err := need(8, "destination node ID"); err != nil {
			return h, 0, err
		}
		h.HasDestNode, h.DestNode = true, binary.LittleEndian.Uint64(b[off:])
		off += 8
	case dsizGroup:
		if err := need(2, "destination group ID"); err != nil {
			return h, 0, err
		}
		h.HasDestGroup, h.DestGroup = true, binary.LittleEndian.Uint16(b[off:])
		off += 2
	default:
		return h, 0, fmt.Errorf("%w: reserved DSIZ", ErrMalformed)
	}
	if sec&secExtensions != 0 {
		ext, n, err := lengthPrefixed(b[off:], "message extensions")
		if err != nil {
			return h, 0, err
		}
		h.Extensions = ext
		off += n
	}
	return h, off, nil
}

// ProtocolID names a protocol: a vendor (0 for the Matter standard) and the vendor's protocol.
type ProtocolID struct {
	Vendor uint16
	ID     uint16
}

// The standard protocols (Core spec, Table 15).
var (
	SecureChannel    = ProtocolID{ID: 0x0000}
	InteractionModel = ProtocolID{ID: 0x0001}
)

// OpStandaloneAck is the Secure Channel opcode of an MRP standalone acknowledgement.
const OpStandaloneAck = 0x10

// Exchange flags (4.4.3.1).
const (
	exInitiator  = 1 << 0
	exAck        = 1 << 1
	exReliable   = 1 << 2
	exSecuredExt = 1 << 3
	exVendor     = 1 << 4
)

// ProtocolHeader is the protocol header at the start of a message's payload (Core spec, 4.4.3),
// encrypted in a secure message.
type ProtocolHeader struct {
	Initiator  bool // I: sent by the exchange's initiator
	Ack        bool // A: AckCounter acknowledges a message
	Reliable   bool // R: the sender wants an acknowledgement
	Opcode     uint8
	ExchangeID uint16
	Protocol   ProtocolID
	AckCounter uint32

	// SecuredExtensions are the secured extensions, which no version of the spec defines yet. nil
	// means absent.
	SecuredExtensions []byte
}

// Append appends the encoded protocol header to b.
func (p *ProtocolHeader) Append(b []byte) []byte {
	var flags byte
	if p.Initiator {
		flags |= exInitiator
	}
	if p.Ack {
		flags |= exAck
	}
	if p.Reliable {
		flags |= exReliable
	}
	if p.SecuredExtensions != nil {
		flags |= exSecuredExt
	}
	if p.Protocol.Vendor != 0 {
		flags |= exVendor
	}
	b = append(b, flags, p.Opcode)
	b = binary.LittleEndian.AppendUint16(b, p.ExchangeID)
	if p.Protocol.Vendor != 0 {
		b = binary.LittleEndian.AppendUint16(b, p.Protocol.Vendor)
	}
	b = binary.LittleEndian.AppendUint16(b, p.Protocol.ID)
	if p.Ack {
		b = binary.LittleEndian.AppendUint32(b, p.AckCounter)
	}
	if p.SecuredExtensions != nil {
		b = binary.LittleEndian.AppendUint16(b, uint16(len(p.SecuredExtensions)))
		b = append(b, p.SecuredExtensions...)
	}
	return b
}

// ParseProtocolHeader decodes the protocol header at the start of b, and returns it with its
// length. The application payload follows it.
func ParseProtocolHeader(b []byte) (ProtocolHeader, int, error) {
	var p ProtocolHeader
	if len(b) < 6 {
		return p, 0, fmt.Errorf("%w: %d bytes is too short for a protocol header", ErrMalformed, len(b))
	}
	flags := b[0]
	p.Initiator = flags&exInitiator != 0
	p.Ack = flags&exAck != 0
	p.Reliable = flags&exReliable != 0
	p.Opcode = b[1]
	p.ExchangeID = binary.LittleEndian.Uint16(b[2:])
	off := 4
	if flags&exVendor != 0 {
		p.Protocol.Vendor = binary.LittleEndian.Uint16(b[off:])
		off += 2
		if len(b)-off < 2 {
			return p, 0, fmt.Errorf("%w: truncated protocol ID", ErrMalformed)
		}
	}
	p.Protocol.ID = binary.LittleEndian.Uint16(b[off:])
	off += 2
	if p.Ack {
		if len(b)-off < 4 {
			return p, 0, fmt.Errorf("%w: truncated acknowledged message counter", ErrMalformed)
		}
		p.AckCounter = binary.LittleEndian.Uint32(b[off:])
		off += 4
	}
	if flags&exSecuredExt != 0 {
		ext, n, err := lengthPrefixed(b[off:], "secured extensions")
		if err != nil {
			return p, 0, err
		}
		p.SecuredExtensions = ext
		off += n
	}
	return p, off, nil
}

// lengthPrefixed reads a 2-byte length and that many bytes.
func lengthPrefixed(b []byte, what string) ([]byte, int, error) {
	if len(b) < 2 {
		return nil, 0, fmt.Errorf("%w: truncated %s length", ErrMalformed, what)
	}
	n := int(binary.LittleEndian.Uint16(b))
	if len(b)-2 < n {
		return nil, 0, fmt.Errorf("%w: %s are %d bytes, %d left", ErrMalformed, what, n, len(b)-2)
	}
	return b[2 : 2+n : 2+n], 2 + n, nil
}
