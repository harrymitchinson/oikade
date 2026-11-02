package securechannel

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"

	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

const testPasscode = 20202021

var (
	testSalt = []byte("SPAKE2P Key Salt")
	echo     = transport.ProtocolID{Vendor: 0xFFF1, ID: 1}
)

type node struct {
	t    *transport.Transport
	conn *recordingConn
}

// recordingConn records what it sends.
type recordingConn struct {
	net.PacketConn
	sent chan []byte
}

func (c *recordingConn) WriteTo(p []byte, a net.Addr) (int, error) {
	select {
	case c.sent <- append([]byte(nil), p...):
	default:
	}
	return c.PacketConn.WriteTo(p, a)
}

func newNode(t *testing.T) *node {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn := &recordingConn{PacketConn: pc, sent: make(chan []byte, 1024)}
	tr := transport.New(conn, transport.Options{Log: testr.New(t)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tr.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &node{t: tr, conn: conn}
}

func (n *node) addr() netip.AddrPort { return n.t.Addr().(*net.UDPAddr).AddrPort() }

// newDevice is a device answering PASE for testPasscode, and echoing over secure sessions.
func newDevice(t *testing.T, sessions *atomic.Int32) *node {
	t.Helper()
	dev := newNode(t)
	v, err := mcrypto.NewVerifier(testPasscode, testSalt, 1000)
	if err != nil {
		t.Fatal(err)
	}
	(&Responder{
		Transport: dev.t, Verifier: v, Salt: testSalt, Iterations: 1000, Log: testr.New(t),
		Params:    &SessionParams{Idle: 500 * time.Millisecond, Active: 300 * time.Millisecond, ActiveThreshold: 4 * time.Second},
		OnSession: func(*transport.Session) { sessions.Add(1) },
	}).Register()
	dev.t.Handle(echo, []uint8{1}, func(ctx context.Context, ex *transport.Exchange, m transport.Message) {
		defer ex.Close()
		if !ex.Session().Secure() {
			return
		}
		_ = ex.Send(ctx, transport.Message{Protocol: echo, Opcode: 2, Payload: append([]byte("echo:"), m.Payload...)})
	})
	return dev
}

func timeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPASE(t *testing.T) {
	var sessions atomic.Int32
	dev, cli := newDevice(t, &sessions), newNode(t)

	s, err := EstablishPASE(timeout(t), cli.t, dev.addr(), testPasscode, &SessionParams{Idle: time.Second})
	if err != nil {
		t.Fatalf("EstablishPASE: %v", err)
	}
	// The device reports the session once the commissioner acknowledges its success status.
	for deadline := time.Now().Add(3 * time.Second); sessions.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if !s.Secure() || sessions.Load() != 1 || dev.t.SecureSessions() != 1 {
		t.Fatalf("secure %v, device sessions %d/%d", s.Secure(), sessions.Load(), dev.t.SecureSessions())
	}

	for len(cli.conn.sent) > 0 {
		<-cli.conn.sent
	}
	ex, err := s.NewExchange()
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	if err := ex.Send(timeout(t), transport.Message{Protocol: echo, Opcode: 1, Payload: []byte("over AES-CCM")}); err != nil {
		t.Fatal(err)
	}
	m, err := ex.Recv(timeout(t))
	if err != nil || string(m.Payload) != "echo:over AES-CCM" {
		t.Fatalf("Recv: %q, %v", m.Payload, err)
	}
	if bytes.Contains(<-cli.conn.sent, []byte("over AES-CCM")) {
		t.Error("the secure message was sent in the clear")
	}
}

func TestPASEWrongPasscode(t *testing.T) {
	var sessions atomic.Int32
	dev, cli := newDevice(t, &sessions), newNode(t)

	// The commissioner finds the device's confirmation wrong first, and tells the device.
	if _, err := EstablishPASE(timeout(t), cli.t, dev.addr(), testPasscode+1, nil); !errors.Is(err, ErrPASEFailed) {
		t.Fatalf("EstablishPASE: %v, want ErrPASEFailed", err)
	}
	time.Sleep(50 * time.Millisecond)
	if sessions.Load() != 0 || dev.t.SecureSessions() != 0 || cli.t.SecureSessions() != 0 {
		t.Errorf("sessions opened: device %d/%d, commissioner %d", sessions.Load(), dev.t.SecureSessions(), cli.t.SecureSessions())
	}

	// The device still takes the right passcode afterwards.
	if _, err := EstablishPASE(timeout(t), cli.t, dev.addr(), testPasscode, nil); err != nil {
		t.Errorf("EstablishPASE with the right passcode after a wrong one: %v", err)
	}
}

// TestPASEDeviceRejectsConfirmation is a commissioner with the wrong passcode that ignores the
// device's confirmation and sends Pake3 anyway: the device must answer with the spec's failure,
// and open no session.
func TestPASEDeviceRejectsConfirmation(t *testing.T) {
	var sessions atomic.Int32
	dev, cli := newDevice(t, &sessions), newNode(t)
	ctx := timeout(t)

	ex, _ := cli.t.NewUnsecuredSession(dev.addr()).NewExchange()
	defer ex.Close()
	req := (&pbkdfParamRequest{initiatorRandom: random(), initiatorSessionID: 7}).encode()
	if err := ex.Send(ctx, message(OpPBKDFParamRequest, req)); err != nil {
		t.Fatal(err)
	}
	respBytes, err := expect(ctx, ex, OpPBKDFParamResponse)
	if err != nil {
		t.Fatal(err)
	}
	w0, w1, _ := mcrypto.PBKDFScalars(testPasscode+1, testSalt, 1000)
	x, _ := mcrypto.RandomScalar()
	shareX := mcrypto.ProverShare(w0, x)
	if err := ex.Send(ctx, message(OpPake1, encodePake(shareX))); err != nil {
		t.Fatal(err)
	}
	pake2, err := expect(ctx, ex, OpPake2)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := decodePake(pake2, mcrypto.PointSize, 32)
	keys, err := mcrypto.Transcript{Context: paseContext(req, respBytes)}.ProverFinish(w0, w1, x, shareX, f[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Send(ctx, message(OpPake3, encodePake(keys.CA))); err != nil {
		t.Fatal(err)
	}
	_, err = expect(ctx, ex, OpStatusReport+1) // anything but the status report it sends
	var sr StatusReport
	if !errors.As(err, &sr) {
		t.Fatalf("after Pake3: %v, want a status report", err)
	}
	if sr.General != GeneralFailure || sr.Protocol != transport.SecureChannel || sr.Code != CodeInvalidParameter {
		t.Errorf("status %+v, want FAILURE / INVALID_PARAMETER", sr)
	}
	if sessions.Load() != 0 || dev.t.SecureSessions() != 0 {
		t.Errorf("device opened a session: %d/%d", sessions.Load(), dev.t.SecureSessions())
	}
}

func TestPASEBusy(t *testing.T) {
	var sessions atomic.Int32
	dev, cli := newDevice(t, &sessions), newNode(t)

	// Start a handshake and leave it after the first message.
	ex, _ := cli.t.NewUnsecuredSession(dev.addr()).NewExchange()
	req := &pbkdfParamRequest{initiatorRandom: random(), initiatorSessionID: 7}
	if err := ex.Send(timeout(t), message(OpPBKDFParamRequest, req.encode())); err != nil {
		t.Fatal(err)
	}
	if _, err := expect(timeout(t), ex, OpPBKDFParamResponse); err != nil {
		t.Fatal(err)
	}

	_, err := EstablishPASE(timeout(t), cli.t, dev.addr(), testPasscode, nil)
	var sr StatusReport
	if !errors.As(err, &sr) || sr.General != GeneralBusy || sr.Code != CodeBusy {
		t.Errorf("a second PASE: %v, want BUSY", err)
	}
}

// TestSessionNonce checks the nonce layout (security flags, counter, sender's node ID) against
// connectedhomeip's Matter AES-CCM vector, whose nonce is one: flags 0, counter 0x01000080 and
// node 0x7856341200000112.
func TestSessionNonce(t *testing.T) {
	want, _ := hex.DecodeString("00800000011201000012345678")
	if got := nonce(0, 0x01000080, 0x7856341200000112); !bytes.Equal(got, want) {
		t.Errorf("nonce = % x, want % x", got, want)
	}
}

// TestSessionReservedFlags checks that a peer setting reserved security flag bits, which
// receivers must ignore, still authenticates: the nonce uses the byte as sent.
func TestSessionReservedFlags(t *testing.T) {
	keys := SessionKeys{I2R: make([]byte, 16), R2I: make([]byte, 16)}
	sender, _ := NewSecurity(keys, true, 0, 0)
	receiver, _ := NewSecurity(keys, false, 0, 0)
	h := &transport.Header{SessionID: 5, Counter: 9}
	hdr := h.Append(nil)
	hdr[3] |= 0x18 // reserved bits
	sealed, _ := sender.Seal(h, hdr, []byte("payload"))
	parsed, _, err := transport.ParseHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := receiver.Open(&parsed, hdr, sealed); err != nil || string(got) != "payload" {
		t.Errorf("Open = %q, %v", got, err)
	}
}

func TestMessagesRoundTrip(t *testing.T) {
	req := &pbkdfParamRequest{
		initiatorRandom: random(), initiatorSessionID: 0x1234, passcodeID: 0, hasPBKDFParameters: true,
		params: &SessionParams{Idle: 5 * time.Second, Active: 300 * time.Millisecond, ActiveThreshold: 4 * time.Second},
	}
	got, err := decodePBKDFParamRequest(req.encode())
	if err != nil || !bytes.Equal(got.initiatorRandom, req.initiatorRandom) || got.initiatorSessionID != 0x1234 ||
		!got.hasPBKDFParameters || *got.params != *req.params {
		t.Errorf("request round trip: %+v, %v", got, err)
	}
	resp := &pbkdfParamResponse{initiatorRandom: random(), responderRandom: random(), responderSessionID: 9, iterations: 2000, salt: testSalt}
	gotResp, err := decodePBKDFParamResponse(resp.encode())
	if err != nil || gotResp.iterations != 2000 || !bytes.Equal(gotResp.salt, testSalt) || gotResp.params != nil {
		t.Errorf("response round trip: %+v, %v", gotResp, err)
	}
}

func TestMessagesRejected(t *testing.T) {
	short := &pbkdfParamRequest{initiatorRandom: make([]byte, 31), initiatorSessionID: 1}
	zeroID := &pbkdfParamRequest{initiatorRandom: random()}
	for name, b := range map[string][]byte{
		"empty":           {},
		"not a structure": {0x04, 0x01},
		"short random":    short.encode(),
		"session ID 0":    zeroID.encode(),
		"missing fields":  encodePake(make([]byte, 32)),
		"repeated field":  {0x15, 0x30, 0x01, 0x00, 0x30, 0x01, 0x00, 0x18},
		"trailing bytes":  append((&pbkdfParamRequest{initiatorRandom: random(), initiatorSessionID: 1}).encode(), 0x04, 0x01),
		"few iterations":  (&pbkdfParamResponse{initiatorRandom: random(), responderRandom: random(), responderSessionID: 1, iterations: 999, salt: testSalt}).encode(),
		"salt too short":  (&pbkdfParamResponse{initiatorRandom: random(), responderRandom: random(), responderSessionID: 1, iterations: 1000, salt: testSalt[:15]}).encode(),
	} {
		_, errReq := decodePBKDFParamRequest(b)
		_, errResp := decodePBKDFParamResponse(b)
		if !errors.Is(errReq, ErrInvalidMessage) || !errors.Is(errResp, ErrInvalidMessage) {
			t.Errorf("%s: request %v, response %v; want ErrInvalidMessage", name, errReq, errResp)
		}
	}
	if _, err := decodePake(encodePake(make([]byte, 64)), mcrypto.PointSize); !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("a 64-byte share: %v", err)
	}
}

func TestStatusReport(t *testing.T) {
	sr := StatusReport{General: GeneralFailure, Protocol: transport.ProtocolID{Vendor: 0xFFF1, ID: 2}, Code: 3, Data: []byte{9}}
	m := sr.Message()
	want, _ := hex.DecodeString("0100" + "0200f1ff" + "0300" + "09")
	if !bytes.Equal(m.Payload, want) || m.Opcode != OpStatusReport {
		t.Errorf("encoded % x, want % x", m.Payload, want)
	}
	got, err := ParseStatusReport(m.Payload)
	if err != nil || got.General != sr.General || got.Protocol != sr.Protocol || got.Code != sr.Code || !bytes.Equal(got.Data, sr.Data) {
		t.Errorf("parsed %+v, %v", got, err)
	}
}
