// Package crypto holds the cryptography Matter needs beyond Go's standard library: AES-CCM, and
// SPAKE2+ with Matter's key schedule (Matter Core Specification, chapter 3).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
)

// Matter's AES-CCM parameters (Core spec, 3.6): AES-128, a 13-byte nonce and a 16-byte tag.
const (
	SymmetricKeySize = 16
	NonceSize        = 13
	TagSize          = 16
)

var errOpen = errors.New("matter: message authentication failed")

// ccm is AES in CCM mode (NIST SP 800-38C, RFC 3610).
type ccm struct {
	block     cipher.Block
	tagSize   int
	nonceSize int
	maxLen    uint64 // the largest message the length field holds
}

// NewCCM returns AES-CCM with the given key, tag size and nonce size. Matter uses
// NewCCM(key, TagSize, NonceSize).
func NewCCM(key []byte, tagSize, nonceSize int) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if tagSize < 4 || tagSize > 16 || tagSize%2 != 0 {
		return nil, fmt.Errorf("matter: CCM tag size %d", tagSize)
	}
	if nonceSize < 7 || nonceSize > 13 {
		return nil, fmt.Errorf("matter: CCM nonce size %d", nonceSize)
	}
	c := &ccm{block: block, tagSize: tagSize, nonceSize: nonceSize, maxLen: 1<<62 - 1}
	if l := 15 - nonceSize; l < 8 {
		c.maxLen = 1<<(8*l) - 1
	}
	return c, nil
}

func (c *ccm) NonceSize() int { return c.nonceSize }
func (c *ccm) Overhead() int  { return c.tagSize }

func (c *ccm) Seal(dst, nonce, plaintext, ad []byte) []byte {
	if len(nonce) != c.nonceSize {
		panic("matter: CCM nonce of the wrong size")
	}
	if uint64(len(plaintext)) > c.maxLen {
		panic("matter: CCM message too long")
	}
	tag := c.mac(nonce, plaintext, ad)
	ret, out := sliceForAppend(dst, len(plaintext)+c.tagSize)
	c.ctr(nonce, out, plaintext, tag[:c.tagSize], out[len(plaintext):])
	return ret
}

func (c *ccm) Open(dst, nonce, ciphertext, ad []byte) ([]byte, error) {
	if len(nonce) != c.nonceSize {
		return nil, errOpen
	}
	if len(ciphertext) < c.tagSize || uint64(len(ciphertext)-c.tagSize) > c.maxLen {
		return nil, errOpen
	}
	n := len(ciphertext) - c.tagSize
	ret, out := sliceForAppend(dst, n)
	var tag [16]byte
	c.ctr(nonce, out, ciphertext[:n], ciphertext[n:], tag[:c.tagSize])
	want := c.mac(nonce, out, ad)
	if subtle.ConstantTimeCompare(tag[:c.tagSize], want[:c.tagSize]) != 1 {
		clear(out)
		return nil, errOpen
	}
	return ret, nil
}

// mac is the CBC-MAC over B0, the encoded additional data and the message.
func (c *ccm) mac(nonce, msg, ad []byte) [16]byte {
	l := 15 - c.nonceSize
	var b0 [16]byte
	b0[0] = byte((c.tagSize-2)/2<<3 | (l - 1))
	if len(ad) > 0 {
		b0[0] |= 1 << 6
	}
	copy(b0[1:], nonce)
	putLength(b0[1+c.nonceSize:], uint64(len(msg)))

	var x [16]byte
	c.block.Encrypt(x[:], b0[:])
	if len(ad) > 0 {
		var head []byte
		if len(ad) < 0xFF00 {
			head = binary.BigEndian.AppendUint16(nil, uint16(len(ad)))
		} else {
			head = binary.BigEndian.AppendUint32([]byte{0xFF, 0xFE}, uint32(len(ad)))
		}
		c.cbc(&x, append(head, ad...))
	}
	c.cbc(&x, msg)
	return x
}

// cbc continues the CBC-MAC x over b, zero-padded to a whole block.
func (c *ccm) cbc(x *[16]byte, b []byte) {
	for len(b) > 0 {
		n := min(16, len(b))
		subtle.XORBytes(x[:n], x[:n], b[:n])
		c.block.Encrypt(x[:], x[:])
		b = b[n:]
	}
}

// ctr encrypts in into out with counter blocks A1, A2 and so on, and the tag tagIn into tagOut
// with A0.
func (c *ccm) ctr(nonce, out, in, tagIn, tagOut []byte) {
	l := 15 - c.nonceSize
	var a [16]byte
	a[0] = byte(l - 1)
	copy(a[1:], nonce)
	var s [16]byte
	c.block.Encrypt(s[:], a[:]) // S0, for the tag
	subtle.XORBytes(tagOut, tagIn, s[:len(tagIn)])
	for i := uint64(1); len(in) > 0; i++ {
		putLength(a[1+c.nonceSize:], i)
		c.block.Encrypt(s[:], a[:])
		n := subtle.XORBytes(out, in, s[:min(16, len(in))])
		in, out = in[n:], out[n:]
	}
}

// putLength writes v big-endian into all of b.
func putLength(b []byte, v uint64) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}

// sliceForAppend extends in by n bytes, and returns the whole slice and the new part.
func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	return head, head[len(in):]
}
