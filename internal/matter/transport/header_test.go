package transport

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	n := 0
	for _, src := range []bool{false, true} {
		for dest := range 3 { // none, node, group
			for _, ext := range [][]byte{nil, {}, {1, 2, 3}} {
				for _, typ := range []SessionType{UnicastSession, GroupSession} {
					for _, pc := range [][2]bool{{false, false}, {true, false}, {false, true}} {
						h := Header{
							SessionID: 0xBEEF, SessionType: typ, Privacy: pc[0], Control: pc[1],
							Counter: 0x01020304, Extensions: ext,
						}
						if src {
							h.HasSource, h.Source = true, 0x1122334455667788
						}
						switch dest {
						case 1:
							h.HasDestNode, h.DestNode = true, 0x8877665544332211
						case 2:
							h.HasDestGroup, h.DestGroup = true, 0xABCD
						}
						b := h.Append(nil)
						got, m, err := ParseHeader(append(b, 0xFF)) // a payload byte follows
						if err != nil || m != len(b) || !reflect.DeepEqual(got, h) {
							t.Errorf("% x: parsed %+v, %d, %v; want %+v, %d", b, got, m, err, h, len(b))
						}
						n++
					}
				}
			}
		}
	}
	if n != 108 {
		t.Fatalf("tried %d combinations", n)
	}
}

func TestHeaderLayout(t *testing.T) {
	// An unsecured message from an initiator: S set, no destination.
	h := Header{Counter: 0x11223344, HasSource: true, Source: 0x0102030405060708}
	want := "04 0000 00 44332211 0807060504030201"
	if got := hex.EncodeToString(h.Append(nil)); got != strings.ReplaceAll(want, " ", "") {
		t.Errorf("encoded %s, want %s", got, want)
	}
	// A secure message to group 0x0102 with extensions.
	h = Header{SessionID: 0x0A0B, SessionType: GroupSession, Counter: 1, HasDestGroup: true, DestGroup: 0x0102, Extensions: []byte{0xEE}}
	want = "02 0b0a 21 01000000 0201 0100ee"
	if got := hex.EncodeToString(h.Append(nil)); got != strings.ReplaceAll(want, " ", "") {
		t.Errorf("encoded %s, want %s", got, want)
	}
}

func TestProtocolHeaderRoundTrip(t *testing.T) {
	for flags := range 32 {
		p := ProtocolHeader{
			Initiator: flags&1 != 0, Ack: flags&2 != 0, Reliable: flags&4 != 0,
			Opcode: 0x42, ExchangeID: 0x1234, Protocol: ProtocolID{ID: 1},
		}
		if p.Ack {
			p.AckCounter = 0xCAFEF00D
		}
		if flags&8 != 0 {
			p.SecuredExtensions = []byte{9, 8}
		}
		if flags&16 != 0 {
			p.Protocol.Vendor = 0xFFF1
		}
		b := p.Append(nil)
		got, m, err := ParseProtocolHeader(append(b, 'x'))
		if err != nil || m != len(b) || !reflect.DeepEqual(got, p) {
			t.Errorf("% x: parsed %+v, %d, %v; want %+v", b, got, m, err, p)
		}
	}
}

func TestProtocolHeaderLayout(t *testing.T) {
	p := ProtocolHeader{Initiator: true, Reliable: true, Ack: true, AckCounter: 7, Opcode: 0x20, ExchangeID: 0x0102, Protocol: SecureChannel}
	want, _ := hex.DecodeString("07200201000007000000")
	if got := p.Append(nil); !bytes.Equal(got, want) {
		t.Errorf("encoded % x, want % x", got, want)
	}
}

func TestHeaderTruncated(t *testing.T) {
	full := (&Header{HasSource: true, Source: 1, HasDestNode: true, DestNode: 2, Extensions: []byte{1, 2}}).Append(nil)
	for i := range len(full) {
		if _, _, err := ParseHeader(full[:i]); !errors.Is(err, ErrMalformed) {
			t.Errorf("%d of %d bytes: %v, want ErrMalformed", i, len(full), err)
		}
	}
	pfull := (&ProtocolHeader{Ack: true, Protocol: ProtocolID{Vendor: 1, ID: 2}, SecuredExtensions: []byte{3}}).Append(nil)
	for i := range len(pfull) {
		if _, _, err := ParseProtocolHeader(pfull[:i]); !errors.Is(err, ErrMalformed) {
			t.Errorf("protocol header, %d of %d bytes: %v, want ErrMalformed", i, len(pfull), err)
		}
	}
}

func TestHeaderRejects(t *testing.T) {
	for name, h := range map[string]string{
		"version 1":     "10 0000 00 01000000",
		"reserved DSIZ": "03 0000 00 01000000 0000000000000000",
	} {
		b, _ := hex.DecodeString(strings.ReplaceAll(h, " ", ""))
		if _, _, err := ParseHeader(b); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
}

func FuzzParseHeaders(f *testing.F) {
	f.Add((&Header{HasSource: true, Source: 1, Extensions: []byte{1}}).Append(nil))
	f.Add((&ProtocolHeader{Ack: true, Protocol: ProtocolID{Vendor: 1}}).Append(nil))
	f.Fuzz(func(t *testing.T, b []byte) {
		// Reserved flag bits don't survive, so compare what was parsed, not the bytes.
		if h, _, err := ParseHeader(b); err == nil {
			if got, _, err := ParseHeader(h.Append(nil)); err != nil || !reflect.DeepEqual(got, h) {
				t.Errorf("header %+v re-parsed as %+v, %v", h, got, err)
			}
		}
		if p, _, err := ParseProtocolHeader(b); err == nil {
			if got, _, err := ParseProtocolHeader(p.Append(nil)); err != nil || !reflect.DeepEqual(got, p) {
				t.Errorf("protocol header %+v re-parsed as %+v, %v", p, got, err)
			}
		}
	})
}
