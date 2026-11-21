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

	"oikade.dev/internal/logging"
	mcrypto "oikade.dev/internal/matter/crypto"
	"oikade.dev/internal/matter/transport"
)

// A capture is handshakes between connectedhomeip's chip-tool and Oikade, recorded into testdata
// by TestCaptureChipToolPASE and TestCaptureChipToolCASE, with the secrets Oikade's side drew. The
// tests that read it (chiptool_test.go) replay Oikade's side and check it against what chip-tool
// sent.
type capture struct {
	// Source is where chip-tool came from.
	Source     string   `json:"source"`
	Passcode   uint32   `json:"passcode"`
	Salt       hexBytes `json:"salt,omitempty"`
	Iterations int      `json:"iterations,omitempty"`
	// Random is each random value PASE drew on Oikade's side, in order, and Scalars each scalar.
	Random  []hexBytes `json:"random,omitempty"`
	Scalars []hexBytes `json:"scalars,omitempty"`
	// EphemeralKeys are the private keys of each full CASE that Oikade's side answered, in order. A
	// resumption draws none.
	EphemeralKeys []hexBytes `json:"ephemeralKeys,omitempty"`
	// Fabric is the fabric chip-tool commissioned Oikade's side onto, without its operational
	// key, which CASE's replay doesn't need.
	Fabric *capturedFabric `json:"fabric,omitempty"`
	// Datagrams are every UDP datagram, in the order Oikade's side saw them.
	Datagrams []datagram `json:"datagrams"`
	// ChipToolLog is chip-tool's log of the messages it sent and received, which shows that it
	// authenticated Oikade's.
	ChipToolLog []string `json:"chipToolLog"`
}

// ansi matches the terminal escapes in chip-tool's log: colours, and clearing the line.
var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

type capturedFabric struct {
	RCAC hexBytes `json:"rcac"`
	ICAC hexBytes `json:"icac,omitempty"`
	NOC  hexBytes `json:"noc"`
	IPK  hexBytes `json:"ipk"` // the epoch key
}

// write writes the capture to testdata/name.
func (c *capture) write(name string) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // chip-tool's log has <<< and >>>
	enc.SetIndent("", "\t")
	if err := enc.Encode(c); err != nil {
		return err
	}
	return os.WriteFile("testdata/"+name, b.Bytes(), 0o644)
}

// chipToolLog is the lines of chip-tool's output that say what it sent and received.
func chipToolLog(out string) []string {
	var lines []string
	for line := range strings.Lines(ansi.ReplaceAllString(out, "")) {
		if strings.Contains(line, "Msg RX") || strings.Contains(line, "Msg TX") || strings.Contains(line, "Rxd Ack") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

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
// named by OIKADE_CHIP_TOOL, and OIKADE_CHIP_TOOL_SOURCE says where it came from.
// testdata/README.md says how to run it in the journeys' chip-tool image.
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
	ctx, cancel := context.WithCancel(t.Context())
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

	c.ChipToolLog = chipToolLog(out.String())
	drawn.Lock()
	defer drawn.Unlock()
	conn.mu.Lock()
	c.Datagrams = conn.all
	conn.mu.Unlock()
	if err := c.write("pase-chip-tool.json"); err != nil {
		t.Fatal(err)
	}
	t.Logf("chip-tool printed:\n%s", out.String())
}
