package securechannel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// A capture is a handshake between connectedhomeip's chip-tool and Oikade, recorded by
// TestCaptureChipToolPASE into testdata, with the randomness Oikade's side used. The tests that
// read it (vectors_test.go) replay Oikade's side and check it against what chip-tool sent.
type capture struct {
	// Source is where chip-tool came from.
	Source     string   `json:"source"`
	Passcode   uint32   `json:"passcode"`
	Salt       hexBytes `json:"salt"`
	Iterations int      `json:"iterations"`
	// Random is each random value Oikade's side drew, in order, and Scalars each scalar.
	Random  []hexBytes `json:"random"`
	Scalars []hexBytes `json:"scalars"`
	// Datagrams are every UDP datagram, in the order Oikade's side saw them.
	Datagrams []datagram `json:"datagrams"`
	// ChipToolLog is chip-tool's log of the messages it sent and received, which shows that it
	// authenticated Oikade's.
	ChipToolLog []string `json:"chipToolLog"`
}

// ansi matches the terminal colours chip-tool logs in.
var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

type datagram struct {
	From string   `json:"from"` // "chip-tool" or "oikade"
	Data hexBytes `json:"data"`
}

type hexBytes []byte

func (b hexBytes) MarshalJSON() ([]byte, error) { return json.Marshal(hex.EncodeToString(b)) }

func (b *hexBytes) UnmarshalJSON(j []byte) error {
	var s string
	if err := json.Unmarshal(j, &s); err != nil {
		return err
	}
	d, err := hex.DecodeString(s)
	*b = d
	return err
}

func readCapture(t *testing.T, name string) *capture {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var c capture
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// capturingConn records every datagram it reads and writes.
type capturingConn struct {
	net.PacketConn
	mu  sync.Mutex
	all []datagram
}

func (c *capturingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, a, err := c.PacketConn.ReadFrom(p)
	if err == nil {
		c.add("chip-tool", p[:n])
	}
	return n, a, err
}

func (c *capturingConn) WriteTo(p []byte, a net.Addr) (int, error) {
	c.add("oikade", p)
	return c.PacketConn.WriteTo(p, a)
}

func (c *capturingConn) add(from string, p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.all = append(c.all, datagram{From: from, Data: append([]byte(nil), p...)})
}

// secureFrom reports whether a secure datagram has come from each side.
func (c *capturingConn) secureFrom() (chipTool, oikade bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.all {
		if h, _, err := transport.ParseHeader(d.Data); err == nil && h.SessionID != 0 {
			chipTool = chipTool || d.From == "chip-tool"
			oikade = oikade || d.From == "oikade"
		}
	}
	return chipTool, oikade
}

// TestCaptureChipToolPASE records chip-tool commissioning Oikade's PASE responder, up to the first
// secure message each way, into testdata/pase-chip-tool.json. It runs only where chip-tool is,
// named by OIKADE_CHIP_TOOL, and OIKADE_CHIP_TOOL_SOURCE says where it came from. testdata/README.md
// says how to run it in the journeys' chip-tool image.
func TestCaptureChipToolPASE(t *testing.T) {
	chipTool := os.Getenv("OIKADE_CHIP_TOOL")
	if chipTool == "" {
		t.Skip("OIKADE_CHIP_TOOL is not set")
	}
	c := &capture{Source: os.Getenv("OIKADE_CHIP_TOOL_SOURCE"), Passcode: testPasscode, Salt: testSalt, Iterations: 1000}

	// The handshake draws in the Responder's goroutine.
	var drawn sync.Mutex
	realRandom, realScalar := random, randomScalar
	t.Cleanup(func() { random, randomScalar = realRandom, realScalar })
	random = func() []byte {
		b := realRandom()
		drawn.Lock()
		defer drawn.Unlock()
		c.Random = append(c.Random, b)
		return b
	}
	randomScalar = func() ([]byte, error) {
		k, err := realScalar()
		drawn.Lock()
		defer drawn.Unlock()
		c.Scalars = append(c.Scalars, k)
		return k, err
	}

	pc, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	conn := &capturingConn{PacketConn: pc}
	tr := transport.New(conn, transport.Options{Log: logging.Test(t)})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { _ = tr.Serve(ctx); close(served) }()
	defer func() { cancel(); <-served }()
	v, err := mcrypto.NewVerifier(c.Passcode, c.Salt, c.Iterations)
	if err != nil {
		t.Fatal(err)
	}
	(&Responder{Transport: tr, Verifier: v, Salt: c.Salt, Iterations: c.Iterations, Log: logging.Test(t)}).Register()

	// chip-tool commissions over the PASE session, starting with a read that Oikade's side has
	// no handler for, so it only acknowledges it. That acknowledgement is Oikade's first secure
	// message.
	toolCtx, stop := context.WithTimeout(ctx, time.Minute)
	defer stop()
	port := strconv.Itoa(pc.LocalAddr().(*net.UDPAddr).Port)
	cmd := exec.CommandContext(toolCtx, chipTool, "pairing", "already-discovered", "0x1122", strconv.Itoa(int(c.Passcode)),
		"::1", port, "--storage-directory", t.TempDir())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if fromTool, fromOikade := conn.secureFrom(); fromTool && fromOikade {
			break
		}
		if time.Now().After(deadline) {
			stop()
			_ = cmd.Wait()
			t.Fatalf("no secure message each way within 30s; chip-tool printed:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // for chip-tool to log what it received
	stop()
	_ = cmd.Wait()

	for line := range strings.Lines(ansi.ReplaceAllString(out.String(), "")) {
		if strings.Contains(line, "Msg RX") || strings.Contains(line, "Msg TX") || strings.Contains(line, "Rxd Ack") {
			c.ChipToolLog = append(c.ChipToolLog, strings.TrimSpace(line))
		}
	}
	drawn.Lock()
	defer drawn.Unlock()
	conn.mu.Lock()
	c.Datagrams = conn.all
	conn.mu.Unlock()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // chip-tool's log has <<< and >>>
	enc.SetIndent("", "\t")
	if err := enc.Encode(c); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/pase-chip-tool.json", b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("chip-tool printed:\n%s", out.String())
}
