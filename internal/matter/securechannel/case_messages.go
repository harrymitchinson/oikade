package securechannel

import (
	"fmt"

	"github.com/harrymitchinson/oikade/internal/matter/cert"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

const (
	destinationIDSize = 32
	resumptionIDSize  = 16
	signatureSize     = 64
)

type sigma1 struct {
	initiatorRandom    []byte
	initiatorSessionID uint16
	destinationID      []byte
	initiatorEphPubKey []byte
	params             *SessionParams
	resumptionID       []byte // with resumeMIC, or neither
	resumeMIC          []byte
}

func (m *sigma1) encode() []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), m.initiatorRandom)
	w.PutUint(tlv.Context(2), uint64(m.initiatorSessionID))
	w.PutBytes(tlv.Context(3), m.destinationID)
	w.PutBytes(tlv.Context(4), m.initiatorEphPubKey)
	m.params.put(&w, 5)
	if m.resumptionID != nil {
		w.PutBytes(tlv.Context(6), m.resumptionID)
		w.PutBytes(tlv.Context(7), m.resumeMIC)
	}
	w.End()
	return w.Bytes()
}

func decodeSigma1(b []byte) (*sigma1, error) {
	m := &sigma1{}
	err := decodeStruct(b, []uint8{1, 2, 3, 4}, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 1:
			m.initiatorRandom, err = fixedBytes(r, randomSize)
		case 2:
			m.initiatorSessionID, err = sessionID(r)
		case 3:
			m.destinationID, err = fixedBytes(r, destinationIDSize)
		case 4:
			m.initiatorEphPubKey, err = fixedBytes(r, mcrypto.PointSize)
		case 5:
			m.params, err = readSessionParams(r)
		case 6:
			m.resumptionID, err = fixedBytes(r, resumptionIDSize)
		case 7:
			m.resumeMIC, err = fixedBytes(r, mcrypto.TagSize)
		}
		return err
	})
	if err == nil && (m.resumptionID == nil) != (m.resumeMIC == nil) {
		err = fmt.Errorf("%w: resumption ID without its MIC, or the reverse", ErrInvalidMessage)
	}
	return m, err
}

type sigma2 struct {
	responderRandom    []byte
	responderSessionID uint16
	responderEphPubKey []byte
	encrypted2         []byte
	params             *SessionParams
}

func (m *sigma2) encode() []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), m.responderRandom)
	w.PutUint(tlv.Context(2), uint64(m.responderSessionID))
	w.PutBytes(tlv.Context(3), m.responderEphPubKey)
	w.PutBytes(tlv.Context(4), m.encrypted2)
	m.params.put(&w, 5)
	w.End()
	return w.Bytes()
}

func decodeSigma2(b []byte) (*sigma2, error) {
	m := &sigma2{}
	err := decodeStruct(b, []uint8{1, 2, 3, 4}, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 1:
			m.responderRandom, err = fixedBytes(r, randomSize)
		case 2:
			m.responderSessionID, err = sessionID(r)
		case 3:
			m.responderEphPubKey, err = fixedBytes(r, mcrypto.PointSize)
		case 4:
			m.encrypted2, err = boundedBytes(r, mcrypto.TagSize+1, maxTBEData+mcrypto.TagSize)
		case 5:
			m.params, err = readSessionParams(r)
		}
		return err
	})
	return m, err
}

type sigma2Resume struct {
	resumptionID       []byte
	resumeMIC          []byte
	responderSessionID uint16
	params             *SessionParams
}

func (m *sigma2Resume) encode() []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), m.resumptionID)
	w.PutBytes(tlv.Context(2), m.resumeMIC)
	w.PutUint(tlv.Context(3), uint64(m.responderSessionID))
	m.params.put(&w, 4)
	w.End()
	return w.Bytes()
}

func decodeSigma2Resume(b []byte) (*sigma2Resume, error) {
	m := &sigma2Resume{}
	err := decodeStruct(b, []uint8{1, 2, 3}, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 1:
			m.resumptionID, err = fixedBytes(r, resumptionIDSize)
		case 2:
			m.resumeMIC, err = fixedBytes(r, mcrypto.TagSize)
		case 3:
			m.responderSessionID, err = sessionID(r)
		case 4:
			m.params, err = readSessionParams(r)
		}
		return err
	})
	return m, err
}

func encodeSigma3(encrypted3 []byte) []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), encrypted3)
	w.End()
	return w.Bytes()
}

func decodeSigma3(b []byte) ([]byte, error) {
	var enc []byte
	err := decodeStruct(b, []uint8{1}, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 1 {
			enc, err = boundedBytes(r, mcrypto.TagSize+1, maxTBEData+mcrypto.TagSize)
		}
		return err
	})
	return enc, err
}

// maxTBEData bounds the encrypted part of Sigma2 and Sigma3: two certificates, a signature, a
// resumption ID and room for future fields, as connectedhomeip allows.
const maxTBEData = 2*cert.MaxTLVLength + signatureSize + resumptionIDSize + 128

// tbeData is the encrypted part of Sigma2 (with a resumption ID) and of Sigma3 (without): the
// sender's certificates and its signature over tbsData.
type tbeData struct {
	noc, icac    []byte
	signature    []byte
	resumptionID []byte
}

func (m *tbeData) encode() []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), m.noc)
	if len(m.icac) > 0 {
		w.PutBytes(tlv.Context(2), m.icac)
	}
	w.PutBytes(tlv.Context(3), m.signature)
	if m.resumptionID != nil {
		w.PutBytes(tlv.Context(4), m.resumptionID)
	}
	w.End()
	return w.Bytes()
}

func decodeTBEData(b []byte, withResumption bool) (*tbeData, error) {
	m := &tbeData{}
	required := []uint8{1, 3}
	if withResumption {
		required = append(required, 4)
	}
	err := decodeStruct(b, required, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 1:
			m.noc, err = boundedBytes(r, 1, cert.MaxTLVLength)
		case 2:
			m.icac, err = boundedBytes(r, 1, cert.MaxTLVLength)
		case 3:
			m.signature, err = fixedBytes(r, signatureSize)
		case 4:
			if withResumption {
				m.resumptionID, err = fixedBytes(r, resumptionIDSize)
			}
		}
		return err
	})
	return m, err
}

// tbsData is what a CASE peer signs: its certificates, its ephemeral key and the other's.
func tbsData(noc, icac, senderKey, receiverKey []byte) []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBytes(tlv.Context(1), noc)
	if len(icac) > 0 {
		w.PutBytes(tlv.Context(2), icac)
	}
	w.PutBytes(tlv.Context(3), senderKey)
	w.PutBytes(tlv.Context(4), receiverKey)
	w.End()
	return w.Bytes()
}

func boundedBytes(r *tlv.Reader, low, high int) ([]byte, error) {
	b, err := r.Bytes()
	if err != nil {
		return nil, err
	}
	if len(b) < low || len(b) > high {
		return nil, fmt.Errorf("%w: %d bytes, want %d to %d", ErrInvalidMessage, len(b), low, high)
	}
	return append([]byte(nil), b...), nil
}
