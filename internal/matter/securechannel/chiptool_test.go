package securechannel

import (
	"bytes"
	"crypto/ecdh"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

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

// unsecuredAt is each unsecured Secure Channel message with the opcode, from from, with where it
// is in msgs.
func unsecuredAt(t *testing.T, msgs []recorded, from string, op uint8) (payloads [][]byte, at []int) {
	t.Helper()
	for i, m := range msgs {
		if m.header.SessionID != 0 || m.from != from {
			continue
		}
		ph, n, err := transport.ParseProtocolHeader(m.payload)
		if err != nil {
			t.Fatal(err)
		}
		if ph.Protocol == transport.SecureChannel && ph.Opcode == op {
			payloads, at = append(payloads, m.payload[n:]), append(at, i)
		}
	}
	return payloads, at
}

// secureAfter is the first secure message from from, on the session, after msgs[after].
func secureAfter(t *testing.T, msgs []recorded, from string, session uint16, after int) recorded {
	t.Helper()
	for _, m := range msgs[after+1:] {
		if m.from == from && m.header.SessionID == session {
			return m
		}
	}
	t.Fatalf("no secure message from %s on session %d", from, session)
	return recorded{}
}

// checkSession checks a session's keys against its first message each way: the device's keys
// open chip-tool's, which is in the Interaction Model, and seal its reply to the bytes chip-tool
// authenticated.
func checkSession(t *testing.T, name string, sk SessionKeys, device, controller uint64, fromTool, fromOikade recorded) {
	t.Helper()
	sec, err := NewSecurity(sk, false, device, controller)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := sec.Open(&fromTool.header, fromTool.hdr, fromTool.payload)
	if err != nil {
		t.Fatalf("%s: Could not open chip-tool's first secure message: %v", name, err)
	}
	if tag := len(fromTool.payload) - len(plain); tag != 16 {
		t.Errorf("%s: chip-tool's message has a %d-byte tag, want 16", name, tag)
	}
	if ph, _, err := transport.ParseProtocolHeader(plain); err != nil || ph.Protocol != transport.InteractionModel {
		t.Errorf("%s: chip-tool's first secure message: %+v, %v; want the Interaction Model", name, ph, err)
	}
	peer, err := NewSecurity(sk, true, controller, device)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := peer.Open(&fromOikade.header, fromOikade.hdr, fromOikade.payload)
	if err != nil {
		t.Fatalf("%s: Could not open Oikade's first secure message as chip-tool: %v", name, err)
	}
	if got, _ := sec.Seal(&fromOikade.header, fromOikade.hdr, reply); !bytes.Equal(got, fromOikade.payload) {
		t.Errorf("%s: Oikade's first secure message sealed\n got % x\nwant % x", name, got, fromOikade.payload)
	}
}

// TestCASEAgainstChipTool replays Oikade's side of chip-tool commissioning it, which ends in a
// full CASE, and of chip-tool then resuming that CASE (testdata/case-chip-tool.json, recorded by
// TestCaptureChipToolCASE), with the ephemeral key Oikade drew then. It checks Oikade's keys
// against chip-tool's messages: they decrypt chip-tool's Sigma3, whose signature verifies, and
// Oikade's Sigma2, which chip-tool accepted; chip-tool's resumption MIC is the one Oikade
// expects; and each session's keys open chip-tool's first message on it and seal Oikade's reply
// exactly as chip-tool authenticated it. Each depends on CASE's HKDF info strings and salts, the
// transcript and the message's associated data and nonce, which only another implementation can
// check.
func TestCASEAgainstChipTool(t *testing.T) {
	c := readCapture(t, "case-chip-tool.json")
	if c.Fabric == nil || len(c.EphemeralKeys) == 0 {
		t.Fatal("capture has no fabric or ephemeral keys")
	}
	f, err := (&Fabric{Index: 1, RCAC: c.Fabric.RCAC, ICAC: c.Fabric.ICAC, NOC: c.Fabric.NOC, IPK: c.Fabric.IPK}).keys()
	if err != nil {
		t.Fatal(err)
	}
	msgs := c.messages(t)
	sigma1s, at1 := unsecuredAt(t, msgs, "chip-tool", OpSigma1)
	sigma2s, _ := unsecuredAt(t, msgs, "oikade", OpSigma2)
	sigma3s, at3 := unsecuredAt(t, msgs, "chip-tool", OpSigma3)
	resumes, atResume := unsecuredAt(t, msgs, "oikade", OpSigma2Resume)
	if len(sigma1s) != 2 || len(sigma2s) != 1 || len(sigma3s) != 1 || len(resumes) != 1 {
		t.Fatalf("%d Sigma1, %d Sigma2, %d Sigma3 and %d Sigma2_Resume; want a full CASE and a resumption",
			len(sigma1s), len(sigma2s), len(sigma3s), len(resumes))
	}

	full := replayFullCASE(t, c, f, msgs, sigma1s[0], sigma2s[0], sigma3s[0], at3[0])
	replayResumedCASE(t, f, msgs, full, sigma1s[1], resumes[0], atResume[0])
	if at1[1] > atResume[0] {
		t.Error("Sigma2_Resume came before the Sigma1 it answers")
	}
	if !chipToolReceived(c, "(S) Msg RX", "IM:ReportData") {
		t.Error("chip-tool's log doesn't show it receiving a report over CASE")
	}
}

// fullCASE is what a full CASE's replay found, which resuming it needs.
type fullCASE struct {
	secret       []byte
	resumptionID []byte // given in Sigma2
	controller   uint64 // chip-tool's node ID
}

// replayFullCASE checks the full CASE, and the session it established.
func replayFullCASE(t *testing.T, c *capture, f *fabricKeys, msgs []recorded, s1Bytes, s2Bytes, s3Bytes []byte, at3 int) fullCASE {
	t.Helper()
	// Sigma1, chip-tool's, for Oikade's node.
	s1, err := decodeSigma1(s1Bytes)
	if err != nil {
		t.Fatalf("chip-tool's Sigma1: %v", err)
	}
	if want := destinationID(f.ipk, s1.initiatorRandom, f.rcac.PublicKey, f.fabricID, f.nodeID); !bytes.Equal(s1.destinationID, want) || s1.resumptionID != nil {
		t.Errorf("chip-tool's first Sigma1: destination % x, resumption % x; want % x and none", s1.destinationID, s1.resumptionID, want)
	}

	// Sigma2, Oikade's: its ephemeral key is the one recorded, and its encrypted part opens with
	// S2K to Oikade's NOC and a signature that verifies, as chip-tool found.
	s2, err := decodeSigma2(s2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := (&sigma2{responderRandom: s2.responderRandom, responderSessionID: s2.responderSessionID,
		responderEphPubKey: s2.responderEphPubKey, encrypted2: s2.encrypted2}).encode(); !bytes.Equal(got, s2Bytes) {
		t.Errorf("Sigma2 re-encoded\n got % x\nwant % x", got, s2Bytes)
	}
	var eph *ecdh.PrivateKey
	for _, b := range c.EphemeralKeys {
		k, err := ecdh.P256().NewPrivateKey(b)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(k.PublicKey().Bytes(), s2.responderEphPubKey) {
			eph = k
		}
	}
	if eph == nil {
		t.Fatal("no recorded ephemeral key is Sigma2's")
	}
	peerEph, err := ecdh.P256().NewPublicKey(s1.initiatorEphPubKey)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := eph.ECDH(peerEph)
	if err != nil {
		t.Fatal(err)
	}
	s2k, err := sigma2Key(secret, f.ipk, s2.responderRandom, s2.responderEphPubKey, s1Bytes)
	if err != nil {
		t.Fatal(err)
	}
	aead, _ := newCCM(s2k)
	plain, err := aead.Open(nil, tbeData2Nonce, s2.encrypted2, nil)
	if err != nil {
		t.Fatalf("Could not open Sigma2 with S2K: %v", err)
	}
	t2, err := decodeTBEData(plain, true)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := f.verifyPeer(t2.noc, t2.icac, time.Time{})
	if err != nil || ours.nodeID != f.nodeID {
		t.Fatalf("Sigma2's NOC: %+v, %v", ours, err)
	}
	if !verifySignature(ours.key, tbsData(t2.noc, t2.icac, s2.responderEphPubKey, s1.initiatorEphPubKey), t2.signature) {
		t.Error("Sigma2's signature doesn't verify")
	}

	// Sigma3, chip-tool's: it opens with S3K, and chip-tool's signature verifies.
	enc3, err := decodeSigma3(s3Bytes)
	if err != nil {
		t.Fatal(err)
	}
	s3k, err := sigma3Key(secret, f.ipk, s1Bytes, s2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	aead, _ = newCCM(s3k)
	if plain, err = aead.Open(nil, tbeData3Nonce, enc3, nil); err != nil {
		t.Fatalf("Could not open chip-tool's Sigma3 with S3K: %v", err)
	}
	t3, err := decodeTBEData(plain, false)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := f.verifyPeer(t3.noc, t3.icac, time.Time{})
	if err != nil {
		t.Fatalf("chip-tool's NOC: %v", err)
	}
	if !verifySignature(controller.key, tbsData(t3.noc, t3.icac, s1.initiatorEphPubKey, s2.responderEphPubKey), t3.signature) {
		t.Error("chip-tool's Sigma3 signature doesn't verify")
	}

	// The session.
	sk, err := caseSessionKeys(secret, f.ipk, s1Bytes, s2Bytes, s3Bytes)
	if err != nil {
		t.Fatal(err)
	}
	checkSession(t, "CASE", sk, f.nodeID, controller.nodeID,
		secureAfter(t, msgs, "chip-tool", s2.responderSessionID, at3),
		secureAfter(t, msgs, "oikade", s1.initiatorSessionID, at3))

	return fullCASE{secret: secret, resumptionID: t2.resumptionID, controller: controller.nodeID}
}

// replayResumedCASE checks the resumption of a full CASE, and the session it established.
func replayResumedCASE(t *testing.T, f *fabricKeys, msgs []recorded, full fullCASE, sigma1Bytes, resumeBytes []byte, atResume int) {
	t.Helper()
	secret := full.secret
	// Resumption: chip-tool's second Sigma1 resumes with the ID from Sigma2, and its MIC is the
	// one Oikade expects.
	r1, err := decodeSigma1(sigma1Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r1.resumptionID, full.resumptionID) {
		t.Fatalf("chip-tool resumed % x, want % x from Sigma2", r1.resumptionID, full.resumptionID)
	}
	if mic, err := resumeMIC(secret, r1.initiatorRandom, r1.resumptionID, sigma1ResumeInfo, resume1Nonce); err != nil || !bytes.Equal(mic, r1.resumeMIC) {
		t.Errorf("chip-tool's resumption MIC % x, want % x (%v)", r1.resumeMIC, mic, err)
	}
	// Sigma2_Resume, Oikade's, which chip-tool accepted.
	r2, err := decodeSigma2Resume(resumeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := (&sigma2Resume{resumptionID: r2.resumptionID, resumeMIC: r2.resumeMIC, responderSessionID: r2.responderSessionID}).encode(); !bytes.Equal(got, resumeBytes) {
		t.Errorf("Sigma2_Resume re-encoded\n got % x\nwant % x", got, resumeBytes)
	}
	if mic, err := resumeMIC(secret, r1.initiatorRandom, r2.resumptionID, sigma2ResumeInfo, resume2Nonce); err != nil || !bytes.Equal(mic, r2.resumeMIC) {
		t.Errorf("Sigma2_Resume's MIC % x, want % x (%v)", r2.resumeMIC, mic, err)
	}
	resumed, err := resumedSessionKeys(secret, r1.initiatorRandom, r1.resumptionID)
	if err != nil {
		t.Fatal(err)
	}
	checkSession(t, "resumed CASE", resumed, f.nodeID, full.controller,
		secureAfter(t, msgs, "chip-tool", r2.responderSessionID, atResume),
		secureAfter(t, msgs, "oikade", r1.initiatorSessionID, atResume))
}
