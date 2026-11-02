package securechannel

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"

	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// SessionKeys are an established session's keys (Core spec, 4.14.2.6 and 4.14.3.7).
type SessionKeys struct {
	I2R, R2I             []byte // initiator to responder, and back
	AttestationChallenge []byte
}

// deriveSessionKeys expands a shared secret into session keys.
func deriveSessionKeys(secret, salt []byte, info string) (SessionKeys, error) {
	k, err := hkdf.Key(sha256.New, secret, salt, info, 3*mcrypto.SymmetricKeySize)
	if err != nil {
		return SessionKeys{}, err
	}
	n := mcrypto.SymmetricKeySize
	return SessionKeys{I2R: k[:n], R2I: k[n : 2*n], AttestationChallenge: k[2*n:]}, nil
}

// aeadSecurity seals and opens a secure session's messages with AES-CCM. The nonce is the
// security flags, the message counter and the sender's node ID (Core spec, 4.8.1.1), and the
// message header is the additional data.
type aeadSecurity struct {
	seal, open          cipher.AEAD
	localNode, peerNode uint64
}

// NewSecurity returns the transport.Security for a session's keys, from one side's view.
func NewSecurity(keys SessionKeys, initiator bool, localNode, peerNode uint64) (transport.Security, error) {
	sealKey, openKey := keys.R2I, keys.I2R
	if initiator {
		sealKey, openKey = keys.I2R, keys.R2I
	}
	seal, err := mcrypto.NewCCM(sealKey, mcrypto.TagSize, mcrypto.NonceSize)
	if err != nil {
		return nil, err
	}
	open, err := mcrypto.NewCCM(openKey, mcrypto.TagSize, mcrypto.NonceSize)
	if err != nil {
		return nil, err
	}
	return &aeadSecurity{seal: seal, open: open, localNode: localNode, peerNode: peerNode}, nil
}

func nonce(h *transport.Header, node uint64) []byte {
	n := make([]byte, 0, mcrypto.NonceSize)
	n = append(n, h.SecurityFlags())
	n = binary.LittleEndian.AppendUint32(n, h.Counter)
	return binary.LittleEndian.AppendUint64(n, node)
}

func (a *aeadSecurity) Seal(h *transport.Header, hdr, payload []byte) ([]byte, error) {
	return a.seal.Seal(nil, nonce(h, a.localNode), payload, hdr), nil
}

func (a *aeadSecurity) Open(h *transport.Header, hdr, sealed []byte) ([]byte, error) {
	return a.open.Open(nil, nonce(h, a.peerNode), sealed, hdr)
}
