// Package securechannel is Matter's Secure Channel protocol (Matter Core Specification, 4.11 to
// 4.14): status reports, the AES-CCM security of established sessions, PASE, which opens a
// session from a setup passcode for commissioning, and CASE, which opens one between two nodes of
// a fabric from their operational certificates.
package securechannel

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// Secure Channel opcodes (Core spec, Table 18).
const (
	OpPBKDFParamRequest  uint8 = 0x20
	OpPBKDFParamResponse uint8 = 0x21
	OpPake1              uint8 = 0x22
	OpPake2              uint8 = 0x23
	OpPake3              uint8 = 0x24
	OpSigma1             uint8 = 0x30
	OpSigma2             uint8 = 0x31
	OpSigma3             uint8 = 0x32
	OpSigma2Resume       uint8 = 0x33
	OpStatusReport       uint8 = 0x40
)

// General status codes (Core spec, Appendix D).
const (
	GeneralSuccess uint16 = 0
	GeneralFailure uint16 = 1
	GeneralBusy    uint16 = 8
)

// Secure Channel protocol status codes (Core spec, 4.11.1.3).
const (
	CodeSessionEstablishmentSuccess uint16 = 0
	CodeNoSharedTrustRoots          uint16 = 1
	CodeInvalidParameter            uint16 = 2
	CodeCloseSession                uint16 = 3
	CodeBusy                        uint16 = 4
)

// StatusReport is a Status Report message (Core spec, Appendix D).
type StatusReport struct {
	General  uint16
	Protocol transport.ProtocolID
	Code     uint16
	Data     []byte
}

// Message encodes the report as a Secure Channel message.
func (s StatusReport) Message() transport.Message {
	b := binary.LittleEndian.AppendUint16(nil, s.General)
	b = binary.LittleEndian.AppendUint32(b, uint32(s.Protocol.Vendor)<<16|uint32(s.Protocol.ID))
	b = binary.LittleEndian.AppendUint16(b, s.Code)
	return transport.Message{Protocol: transport.SecureChannel, Opcode: OpStatusReport, Payload: append(b, s.Data...)}
}

// ParseStatusReport decodes a Status Report's payload.
func ParseStatusReport(b []byte) (StatusReport, error) {
	if len(b) < 8 {
		return StatusReport{}, errors.New("matter: status report too short")
	}
	p := binary.LittleEndian.Uint32(b[2:])
	return StatusReport{
		General:  binary.LittleEndian.Uint16(b),
		Protocol: transport.ProtocolID{Vendor: uint16(p >> 16), ID: uint16(p)},
		Code:     binary.LittleEndian.Uint16(b[6:]),
		Data:     b[8:],
	}, nil
}

// Success reports whether s is a Secure Channel success.
func (s StatusReport) Success() bool {
	return s.General == GeneralSuccess && s.Protocol == transport.SecureChannel && s.Code == CodeSessionEstablishmentSuccess
}

// Error makes a StatusReport an error, for a peer that reported a failure.
func (s StatusReport) Error() string {
	return fmt.Sprintf("matter: peer reported status general=%d protocol=%#x/%#x code=%d", s.General, s.Protocol.Vendor, s.Protocol.ID, s.Code)
}

func secureChannelStatus(general, code uint16) StatusReport {
	return StatusReport{General: general, Protocol: transport.SecureChannel, Code: code}
}

// removeOnPanic takes a session out again if a panic interrupts its handshake before the node has it,
// so that a panic contained further up leaves no secure session the node doesn't know of. Defer it
// directly: recover works only there.
func removeOnPanic(tr *transport.Transport, s *transport.Session) {
	if r := recover(); r != nil {
		tr.RemoveSession(s)
		panic(r)
	}
}
