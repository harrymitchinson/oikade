package securechannel

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// recorded is one message of a capture.
type recorded struct {
	from    string
	header  transport.Header
	hdr     []byte // the encoded message header, the additional data of a secure message
	payload []byte // the rest: protocol header and application payload, sealed if secure
}

// messages splits a capture's datagrams into messages, leaving out unsecured standalone
// acknowledgements.
func (c *capture) messages(t *testing.T) []recorded {
	t.Helper()
	var all []recorded
	for _, d := range c.Datagrams {
		h, n, err := transport.ParseHeader(d.Data)
		if err != nil {
			t.Fatal(err)
		}
		m := recorded{from: d.From, header: h, hdr: d.Data[:n], payload: d.Data[n:]}
		if h.SessionID == 0 {
			if ph, _, err := transport.ParseProtocolHeader(m.payload); err != nil {
				t.Fatal(err)
			} else if ph.Protocol == transport.SecureChannel && ph.Opcode == transport.OpStandaloneAck {
				continue
			}
		}
		all = append(all, m)
	}
	return all
}

// unsecured is the payload of the unsecured Secure Channel message with the opcode, which must
// come from from.
func unsecured(t *testing.T, msgs []recorded, from string, op uint8) []byte {
	t.Helper()
	for _, m := range msgs {
		if m.header.SessionID != 0 {
			continue
		}
		ph, n, err := transport.ParseProtocolHeader(m.payload)
		if err != nil {
			t.Fatal(err)
		}
		if ph.Protocol == transport.SecureChannel && ph.Opcode == op {
			if m.from != from {
				t.Fatalf("opcode %#x came from %s, want %s", op, m.from, from)
			}
			return m.payload[n:]
		}
	}
	t.Fatalf("no opcode %#x in the capture", op)
	return nil
}

// firstSecure is the first secure message from from.
func firstSecure(t *testing.T, msgs []recorded, from string) recorded {
	t.Helper()
	for _, m := range msgs {
		if m.header.SessionID != 0 && m.from == from {
			return m
		}
	}
	t.Fatalf("no secure message from %s", from)
	return recorded{}
}

// TestPASEAgainstChipTool replays Oikade's side of a PASE handshake with connectedhomeip's
// chip-tool (testdata/pase-chip-tool.json, recorded by TestCaptureChipToolPASE), with the random
// values it drew then, and checks it against what chip-tool sent: chip-tool's messages decode
// and encode to the same bytes, Oikade's come out byte for byte as they did, chip-tool's
// confirmation (Pake3) is the one Oikade expects, and the session keys open chip-tool's first
// secure message and seal Oikade's acknowledgement exactly as chip-tool authenticated it. Each of
// those depends on the transcript's context, the HKDF info strings and which key is which
// direction, all of which only another implementation can check.
func TestPASEAgainstChipTool(t *testing.T) {
	c := readCapture(t, "pase-chip-tool.json")
	if len(c.Random) != 1 || len(c.Scalars) != 1 {
		t.Fatalf("capture has %d random values and %d scalars, want 1 each", len(c.Random), len(c.Scalars))
	}
	msgs := c.messages(t)
	fromTool, fromOikade := firstSecure(t, msgs, "chip-tool"), firstSecure(t, msgs, "oikade")

	// PBKDFParamRequest, chip-tool's.
	reqBytes := unsecured(t, msgs, "chip-tool", OpPBKDFParamRequest)
	req, err := decodePBKDFParamRequest(reqBytes)
	if err != nil {
		t.Fatalf("chip-tool's PBKDFParamRequest: %v", err)
	}
	if req.passcodeID != 0 || req.hasPBKDFParameters || req.params == nil {
		t.Errorf("chip-tool's PBKDFParamRequest: %+v, want passcode ID 0, no PBKDF parameters, and its session parameters", req)
	}
	if req.initiatorSessionID != fromOikade.header.SessionID {
		t.Errorf("chip-tool's session ID %d, but Oikade's first secure message is to %d", req.initiatorSessionID, fromOikade.header.SessionID)
	}

	// PBKDFParamResponse, Oikade's, to the session ID chip-tool then used.
	respBytes := unsecured(t, msgs, "oikade", OpPBKDFParamResponse)
	resp := &pbkdfParamResponse{
		initiatorRandom: req.initiatorRandom, responderRandom: c.Random[0],
		responderSessionID: fromTool.header.SessionID, iterations: uint32(c.Iterations), salt: c.Salt,
	}
	if got := resp.encode(); !bytes.Equal(got, respBytes) {
		t.Errorf("PBKDFParamResponse\n got % x\nwant % x", got, respBytes)
	}

	// Pake1, chip-tool's.
	pake1 := unsecured(t, msgs, "chip-tool", OpPake1)
	f, err := decodePake(pake1, mcrypto.PointSize)
	if err != nil {
		t.Fatalf("chip-tool's Pake1: %v", err)
	}
	shareX := f[0]
	if got := encodePake(shareX); !bytes.Equal(got, pake1) {
		t.Errorf("Pake1 re-encoded\n got % x\nwant % x", got, pake1)
	}

	// Pake2, Oikade's.
	v, err := mcrypto.NewVerifier(c.Passcode, c.Salt, c.Iterations)
	if err != nil {
		t.Fatal(err)
	}
	tr := mcrypto.Transcript{Context: paseContext(reqBytes, respBytes)}
	shareY, keys, err := tr.VerifierFinish(v, c.Scalars[0], shareX)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := encodePake(shareY, keys.CB), unsecured(t, msgs, "oikade", OpPake2); !bytes.Equal(got, want) {
		t.Errorf("Pake2\n got % x\nwant % x", got, want)
	}

	// Pake3, chip-tool's: its confirmation, from its own transcript.
	pake3 := unsecured(t, msgs, "chip-tool", OpPake3)
	f, err = decodePake(pake3, sha256.Size)
	if err != nil {
		t.Fatalf("chip-tool's Pake3: %v", err)
	}
	if !bytes.Equal(f[0], keys.CA) {
		t.Errorf("chip-tool's confirmation cA % x, want % x: the SPAKE2+ transcripts differ", f[0], keys.CA)
	}
	if got := encodePake(keys.CA); !bytes.Equal(got, pake3) {
		t.Errorf("Pake3\n got % x\nwant % x", got, pake3)
	}

	// The session: the device opens chip-tool's first secure message, a ReadRequest...
	sk, err := deriveSessionKeys(keys.Ke, nil, paseKeysInfo)
	if err != nil {
		t.Fatal(err)
	}
	device, err := NewSecurity(sk, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := device.Open(&fromTool.header, fromTool.hdr, fromTool.payload)
	if err != nil {
		t.Fatalf("Could not open chip-tool's first secure message: %v", err)
	}
	ph, _, err := transport.ParseProtocolHeader(plain)
	if err != nil || ph.Protocol != transport.InteractionModel || ph.Opcode != 0x02 {
		t.Errorf("chip-tool's first secure message: %+v, %v; want an Interaction Model ReadRequest", ph, err)
	}

	// ...and seals its acknowledgement to the bytes chip-tool authenticated.
	commissioner, err := NewSecurity(sk, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := commissioner.Open(&fromOikade.header, fromOikade.hdr, fromOikade.payload)
	if err != nil {
		t.Fatalf("Could not open Oikade's first secure message as chip-tool: %v", err)
	}
	if ph, _, err := transport.ParseProtocolHeader(ack); err != nil || ph.Opcode != transport.OpStandaloneAck || ph.AckCounter != fromTool.header.Counter {
		t.Errorf("Oikade's first secure message: %+v, %v; want the acknowledgement of counter %d", ph, err, fromTool.header.Counter)
	}
	if got, _ := device.Seal(&fromOikade.header, fromOikade.hdr, ack); !bytes.Equal(got, fromOikade.payload) {
		t.Errorf("acknowledgement sealed\n got % x\nwant % x", got, fromOikade.payload)
	}
	if !chipToolReceived(c, "(S) Msg RX", "StandaloneAck") {
		t.Error("chip-tool's log doesn't show it receiving Oikade's secure acknowledgement")
	}
}

// chipToolReceived reports whether a line of chip-tool's log has all of parts.
func chipToolReceived(c *capture, parts ...string) bool {
next:
	for _, line := range c.ChipToolLog {
		for _, p := range parts {
			if !strings.Contains(line, p) {
				continue next
			}
		}
		return true
	}
	return false
}
