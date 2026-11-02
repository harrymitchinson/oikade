package securechannel

import (
	"errors"
	"fmt"
	"io"
	"time"

	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// ErrInvalidMessage wraps every error from decoding a Secure Channel message.
var ErrInvalidMessage = errors.New("matter: invalid secure channel message")

const randomSize = 32

// SessionParams are what a node says of itself when establishing a session (Core spec,
// 4.12.8): its MRP intervals.
type SessionParams struct {
	Idle            time.Duration
	Active          time.Duration
	ActiveThreshold time.Duration
}

// MRP turns the parameters into a transport.MRPConfig. Absent ones stay zero, so the transport's
// defaults fill them.
func (p *SessionParams) MRP() transport.MRPConfig {
	if p == nil {
		return transport.MRPConfig{}
	}
	return transport.MRPConfig{Idle: p.Idle, Active: p.Active, ActiveThreshold: p.ActiveThreshold}
}

func (p *SessionParams) put(w *tlv.Writer, tag uint8) {
	if p == nil {
		return
	}
	w.StartStructure(tlv.Context(tag))
	if p.Idle > 0 {
		w.PutUint(tlv.Context(1), uint64(p.Idle.Milliseconds()))
	}
	if p.Active > 0 {
		w.PutUint(tlv.Context(2), uint64(p.Active.Milliseconds()))
	}
	if p.ActiveThreshold > 0 {
		w.PutUint(tlv.Context(3), uint64(p.ActiveThreshold.Milliseconds()))
	}
	w.End()
}

func readSessionParams(r *tlv.Reader) (*SessionParams, error) {
	p := &SessionParams{}
	ms := func(r *tlv.Reader, max uint64) (time.Duration, error) {
		v, err := r.Uint()
		if err != nil {
			return 0, err
		}
		if v > max {
			return 0, fmt.Errorf("%w: session parameter %d ms", ErrInvalidMessage, v)
		}
		return time.Duration(v) * time.Millisecond, nil
	}
	err := readFields(r, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 1:
			p.Idle, err = ms(r, 3600000) // the spec caps both intervals at an hour
		case 2:
			p.Active, err = ms(r, 3600000)
		case 3:
			p.ActiveThreshold, err = ms(r, 0xFFFF)
		}
		return err // later fields (revisions, versions) are ignored
	})
	return p, err
}

type pbkdfParamRequest struct {
	initiatorRandom    []byte
	initiatorSessionID uint16
	passcodeID         uint16
	hasPBKDFParameters bool
	params             *SessionParams
}

func (m *pbkdfParamRequest) encode() []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), m.initiatorRandom)
	w.PutUint(tlv.Context(2), uint64(m.initiatorSessionID))
	w.PutUint(tlv.Context(3), uint64(m.passcodeID))
	w.PutBool(tlv.Context(4), m.hasPBKDFParameters)
	m.params.put(&w, 5)
	w.End()
	return w.Bytes()
}

func decodePBKDFParamRequest(b []byte) (*pbkdfParamRequest, error) {
	m := &pbkdfParamRequest{}
	err := decodeStruct(b, []uint8{1, 2, 3, 4}, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 1:
			m.initiatorRandom, err = fixedBytes(r, randomSize)
		case 2:
			m.initiatorSessionID, err = sessionID(r)
		case 3:
			m.passcodeID, err = uint16Field(r)
		case 4:
			m.hasPBKDFParameters, err = r.Bool()
		case 5:
			m.params, err = readSessionParams(r)
		}
		return err
	})
	return m, err
}

type pbkdfParamResponse struct {
	initiatorRandom    []byte
	responderRandom    []byte
	responderSessionID uint16
	iterations         uint32 // with salt, absent if the initiator said it has them
	salt               []byte
	params             *SessionParams
}

func (m *pbkdfParamResponse) encode() []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), m.initiatorRandom)
	w.PutBytes(tlv.Context(2), m.responderRandom)
	w.PutUint(tlv.Context(3), uint64(m.responderSessionID))
	if m.salt != nil {
		w.StartStructure(tlv.Context(4))
		w.PutUint(tlv.Context(1), uint64(m.iterations))
		w.PutBytes(tlv.Context(2), m.salt)
		w.End()
	}
	m.params.put(&w, 5)
	w.End()
	return w.Bytes()
}

func decodePBKDFParamResponse(b []byte) (*pbkdfParamResponse, error) {
	m := &pbkdfParamResponse{}
	err := decodeStruct(b, []uint8{1, 2, 3}, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 1:
			m.initiatorRandom, err = fixedBytes(r, randomSize)
		case 2:
			m.responderRandom, err = fixedBytes(r, randomSize)
		case 3:
			m.responderSessionID, err = sessionID(r)
		case 4:
			return readFields(r, func(r *tlv.Reader, tag uint8) (err error) {
				switch tag {
				case 1:
					var v uint64
					if v, err = r.Uint(); err == nil {
						if v < mcrypto.MinIterations || v > mcrypto.MaxIterations {
							return fmt.Errorf("%w: %d PBKDF2 iterations", ErrInvalidMessage, v)
						}
						m.iterations = uint32(v)
					}
				case 2:
					m.salt, err = r.Bytes()
					if err == nil && (len(m.salt) < mcrypto.MinSaltSize || len(m.salt) > mcrypto.MaxSaltSize) {
						return fmt.Errorf("%w: PBKDF2 salt of %d bytes", ErrInvalidMessage, len(m.salt))
					}
					m.salt = append([]byte(nil), m.salt...)
				}
				return err
			})
		case 5:
			m.params, err = readSessionParams(r)
		}
		return err
	})
	return m, err
}

// pake1, pake2 and pake3 carry SPAKE2+'s shares and confirmations.
func encodePake(fields ...[]byte) []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	for i, f := range fields {
		w.PutBytes(tlv.Context(uint8(i+1)), f)
	}
	w.End()
	return w.Bytes()
}

// decodePake reads a Pake message whose fields 1 to len(sizes) are octet strings of those sizes.
func decodePake(b []byte, sizes ...int) ([][]byte, error) {
	out := make([][]byte, len(sizes))
	required := make([]uint8, len(sizes))
	for i := range sizes {
		required[i] = uint8(i + 1)
	}
	err := decodeStruct(b, required, func(r *tlv.Reader, tag uint8) (err error) {
		if i := int(tag) - 1; i >= 0 && i < len(sizes) {
			out[i], err = fixedBytes(r, sizes[i])
		}
		return err
	})
	return out, err
}

// decodeStruct reads an anonymous structure, calling field for each context-tagged element. It
// fails if a tag repeats, or if any required tag is missing.
func decodeStruct(b []byte, required []uint8, field func(r *tlv.Reader, tag uint8) error) error {
	r := tlv.NewReader(b)
	if err := r.Next(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	if r.Type() != tlv.Structure {
		return fmt.Errorf("%w: not a structure", ErrInvalidMessage)
	}
	var seen [256]bool
	err := readFields(r, func(r *tlv.Reader, tag uint8) error {
		seen[tag] = true
		return field(r, tag)
	})
	if err != nil {
		return err
	}
	for _, tag := range required {
		if !seen[tag] {
			return fmt.Errorf("%w: field %d missing", ErrInvalidMessage, tag)
		}
	}
	return nil
}

// readFields enters the structure r is on, calls field once for each context-tagged element,
// and leaves it. A repeated tag is an error. Other tags are skipped.
func readFields(r *tlv.Reader, field func(r *tlv.Reader, tag uint8) error) error {
	if r.Type() != tlv.Structure {
		return fmt.Errorf("%w: not a structure", ErrInvalidMessage)
	}
	if err := r.Enter(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	var seen [256]bool
	for {
		err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidMessage, err)
		}
		tag := r.Tag()
		if tag.Kind != tlv.ContextTag {
			continue
		}
		if seen[tag.Number] {
			return fmt.Errorf("%w: field %d repeated", ErrInvalidMessage, tag.Number)
		}
		seen[tag.Number] = true
		if err := field(r, uint8(tag.Number)); err != nil {
			if !errors.Is(err, ErrInvalidMessage) {
				err = fmt.Errorf("%w: field %d: %w", ErrInvalidMessage, tag.Number, err)
			}
			return err
		}
	}
	if err := r.Exit(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	return nil
}

func fixedBytes(r *tlv.Reader, n int) ([]byte, error) {
	b, err := r.Bytes()
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrInvalidMessage, len(b), n)
	}
	return append([]byte(nil), b...), nil
}

func uint16Field(r *tlv.Reader) (uint16, error) {
	v, err := r.Uint()
	if err != nil {
		return 0, err
	}
	if v > 0xFFFF {
		return 0, fmt.Errorf("%w: %d is over 16 bits", ErrInvalidMessage, v)
	}
	return uint16(v), nil
}

func sessionID(r *tlv.Reader) (uint16, error) {
	v, err := uint16Field(r)
	if err == nil && v == 0 {
		return 0, fmt.Errorf("%w: session ID 0", ErrInvalidMessage)
	}
	return v, err
}
