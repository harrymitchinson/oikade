// Package setup encodes a Matter onboarding payload as the QR code string ("MT:…") and the
// manual pairing code that a commissioner such as the Home app takes (Matter Core Specification,
// 5.1).
package setup

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

// Flow is the commissioning flow.
type Flow uint8

// Commissioning flows.
const (
	FlowStandard   Flow = 0 // ready as soon as it's powered
	FlowUserIntent Flow = 1
	FlowCustom     Flow = 2
)

// Discovery capabilities, a bitmap of how a commissioner can find the device.
const (
	DiscoverySoftAP    uint8 = 1 << 0
	DiscoveryBLE       uint8 = 1 << 1
	DiscoveryOnNetwork uint8 = 1 << 2
)

// Field limits.
const (
	MaxDiscriminator = 1<<12 - 1
	MaxPasscode      = 99999998
)

// Payload is what a commissioner needs to find and commission a device.
type Payload struct {
	Version       uint8 // 3 bits; 0
	VendorID      uint16
	ProductID     uint16
	Flow          Flow
	Discovery     uint8  // Discovery* bits
	Discriminator uint16 // 12 bits
	Passcode      uint32 // 27 bits
}

// ValidPasscode reports whether p may be a setup passcode (Core spec, 5.1.7.1): 1 to 99999998,
// and not one of the trivial values.
func ValidPasscode(p uint32) bool {
	switch p {
	case 0, 11111111, 22222222, 33333333, 44444444, 55555555, 66666666, 77777777, 88888888,
		99999999, 12345678, 87654321:
		return false
	}
	return p <= MaxPasscode
}

func (p *Payload) validate() error {
	switch {
	case p.Version > 7:
		return errors.New("matter: setup payload version over 7")
	case p.Flow > FlowCustom:
		return fmt.Errorf("matter: commissioning flow %d", p.Flow)
	case p.Discriminator > MaxDiscriminator:
		return fmt.Errorf("matter: discriminator %d over 12 bits", p.Discriminator)
	case !ValidPasscode(p.Passcode):
		return errors.New("matter: invalid setup passcode")
	}
	return nil
}

// QRCode returns the payload's QR code string: "MT:" and the Base38 encoding of its fields,
// packed least significant bit first.
func (p *Payload) QRCode() (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	var b bitWriter
	b.put(uint64(p.Version), 3)
	b.put(uint64(p.VendorID), 16)
	b.put(uint64(p.ProductID), 16)
	b.put(uint64(p.Flow), 2)
	b.put(uint64(p.Discovery), 8)
	b.put(uint64(p.Discriminator), 12)
	b.put(uint64(p.Passcode), 27)
	b.put(0, 4) // padding
	return "MT:" + Base38(b.bytes[:]), nil
}

// ManualCode returns the payload's manual pairing code: 11 digits, or 21 with the vendor and
// product IDs for the custom flow. It carries only the discriminator's top 4 bits.
func (p *Payload) ManualCode() (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	short := uint32(p.Discriminator >> 8)
	custom := p.Flow != FlowStandard
	var first = short >> 2
	if custom {
		first |= 1 << 2
	}
	s := fmt.Sprintf("%d%05d%04d", first, (short&3)<<14|p.Passcode&(1<<14-1), p.Passcode>>14)
	if custom {
		s += fmt.Sprintf("%05d%05d", p.VendorID, p.ProductID)
	}
	return s + strconv.Itoa(Verhoeff(s)), nil
}

type bitWriter struct {
	bytes [11]byte
	n     int
}

func (w *bitWriter) put(v uint64, bits int) {
	for i := range bits {
		if v>>i&1 != 0 {
			w.bytes[w.n/8] |= 1 << (w.n % 8)
		}
		w.n++
	}
}

const base38Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ-."

// Base38 encodes b as Matter does (Core spec, 5.1.3.1): each 3 bytes, little endian, as 5
// characters, least significant first, and a final 1 or 2 bytes as 2 or 4.
func Base38(b []byte) string {
	out := make([]byte, 0, (len(b)+2)/3*5)
	for len(b) > 0 {
		n := min(3, len(b))
		var v uint32
		for i := n - 1; i >= 0; i-- {
			v = v<<8 | uint32(b[i])
		}
		chars := [4]int{0, 2, 4, 5}[n]
		for range chars {
			out = append(out, base38Alphabet[v%38])
			v /= 38
		}
		b = b[n:]
	}
	return string(out)
}

// Verhoeff tables (dihedral group D5).
var (
	verhoeffD = [10][10]uint8{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{1, 2, 3, 4, 0, 6, 7, 8, 9, 5},
		{2, 3, 4, 0, 1, 7, 8, 9, 5, 6},
		{3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
		{4, 0, 1, 2, 3, 9, 5, 6, 7, 8},
		{5, 9, 8, 7, 6, 0, 4, 3, 2, 1},
		{6, 5, 9, 8, 7, 1, 0, 4, 3, 2},
		{7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
		{8, 7, 6, 5, 9, 3, 2, 1, 0, 4},
		{9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
	}
	verhoeffP = [8][10]uint8{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{1, 5, 7, 6, 2, 8, 3, 0, 9, 4},
		{5, 8, 0, 3, 7, 9, 6, 1, 4, 2},
		{8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
		{9, 4, 5, 3, 1, 2, 6, 8, 7, 0},
		{4, 2, 8, 6, 5, 7, 3, 9, 0, 1},
		{2, 7, 9, 3, 8, 0, 6, 4, 1, 5},
		{7, 0, 4, 6, 9, 1, 3, 2, 5, 8},
	}
	verhoeffInv = [10]uint8{0, 4, 3, 2, 1, 5, 6, 7, 8, 9}
)

// Verhoeff returns the Verhoeff check digit for a string of decimal digits.
func Verhoeff(digits string) int {
	var c uint8
	for i := range len(digits) {
		d := digits[len(digits)-1-i] - '0'
		c = verhoeffD[c][verhoeffP[(i+1)%8][d]]
	}
	return int(verhoeffInv[c])
}

// Random returns a random valid passcode and discriminator, for a node's own setup code.
func Random() (passcode uint32, discriminator uint16) {
	var b [6]byte
	for !ValidPasscode(passcode) {
		_, _ = rand.Read(b[:])
		passcode = binary.LittleEndian.Uint32(b[:4]) % (MaxPasscode + 1)
	}
	return passcode, binary.LittleEndian.Uint16(b[4:]) & MaxDiscriminator
}
