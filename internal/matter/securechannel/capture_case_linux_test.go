//go:build linux

package securechannel_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/node"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// TestCaptureChipToolCASE records chip-tool commissioning an Oikade node, which ends in a full
// CASE, and then, once chip-tool has expired that session, reading from it, which resumes it, into
// testdata/case-chip-tool.json. It runs only where chip-tool is, named by OIKADE_CHIP_TOOL, and
// needs to sniff the node's port, as root. testdata/README.md says how to run it in the journeys'
// chip-tool image.
func TestCaptureChipToolCASE(t *testing.T) {
	chipTool := os.Getenv("OIKADE_CHIP_TOOL")
	if chipTool == "" {
		t.Skip("OIKADE_CHIP_TOOL is not set")
	}
	c := &securechannel.Capture{Source: os.Getenv("OIKADE_CHIP_TOOL_SOURCE"), Passcode: 20202021}
	var keys sync.Mutex
	securechannel.KeepEphemeralKeys(t, func(k *ecdh.PrivateKey) {
		keys.Lock()
		defer keys.Unlock()
		c.EphemeralKeys = append(c.EphemeralKeys, k.Bytes())
	})
	sn := sniff(t, transport.Port)

	listening := make(chan struct{})
	n, err := node.New(node.Config{
		DataDir: t.TempDir(), Setup: node.Setup{Passcode: c.Passcode, Discriminator: 3840},
		Listening: func() { close(listening) }, Log: logging.Test(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan error, 1)
	go func() { ran <- n.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-ran; err != nil {
			t.Errorf("Node stopped: %v", err)
		}
	}()
	select {
	case <-listening:
	case err := <-ran:
		t.Fatal(err)
	}
	payload := n.Payload()
	code, err := payload.ManualCode()
	if err != nil {
		t.Fatal(err)
	}

	// One chip-tool process, so that expiring the session from commissioning makes the read
	// resume it: a new process starts its fabric over, and doesn't resume.
	toolCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	cmd := exec.CommandContext(toolCtx, chipTool, "interactive", "start", "--storage-directory", t.TempDir())
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stop(); _ = cmd.Wait() }()
	run := func(command, want string) {
		t.Helper()
		if _, err := io.WriteString(stdin, command+"\n"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Minute)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("chip-tool %s didn't print %q:\n%s", command, want, out.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	run("pairing code 0x1122 "+code, "Device commissioning completed with success")
	run("sessionmanagement expire-case-sessions 0x1122", "expire-case-sessions")
	time.Sleep(time.Second)
	run("basicinformation read vendor-id 0x1122 0", "CASE_Sigma2Resume")
	run("basicinformation read product-id 0x1122 0", "ProductID")
	time.Sleep(time.Second) // for the last acknowledgements
	_, _ = io.WriteString(stdin, "quit\n")
	c.Datagrams = sn.stop()

	fabrics := n.Fabrics().Fabrics()
	if len(fabrics) != 1 {
		t.Fatalf("%d fabrics, want chip-tool's", len(fabrics))
	}
	f := fabrics[0]
	c.Fabric = &securechannel.CapturedFabric{RCAC: f.RCAC, ICAC: f.ICAC, NOC: f.NOC, IPK: f.IPK}
	c.ChipToolLog = securechannel.ChipToolLog(out.String())
	keys.Lock()
	defer keys.Unlock()
	if err := c.Write("case-chip-tool.json"); err != nil {
		t.Fatal(err)
	}
}

// sniffer records the UDP datagrams to and from one port, on every interface.
type sniffer struct {
	port uint16
	stop func() []securechannel.Datagram
	mu   sync.Mutex
	all  []securechannel.Datagram
}

func sniff(t *testing.T, port uint16) *sniffer {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		t.Fatalf("Could not open a packet socket, which needs root: %v", err)
	}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &syscall.Timeval{Usec: 100000}); err != nil {
		t.Fatal(err)
	}
	s := &sniffer{port: port}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1<<16)
		for {
			select {
			case <-quit:
				return
			default:
			}
			n, from, err := syscall.Recvfrom(fd, buf, 0)
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				t.Errorf("Could not sniff: %v", err)
				return
			}
			// A packet to this host on loopback is seen going out and coming in: keep it once.
			if ll, ok := from.(*syscall.SockaddrLinklayer); ok && ll.Pkttype == syscall.PACKET_OUTGOING {
				continue
			}
			s.add(buf[:n])
		}
	}()
	s.stop = func() []securechannel.Datagram {
		close(quit)
		<-done
		_ = syscall.Close(fd)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.all
	}
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			s.stop()
		}
	})
	return s
}

// add keeps an IP packet if it's a UDP datagram between the port and another.
func (s *sniffer) add(pkt []byte) {
	var udp []byte
	switch {
	case len(pkt) >= 48 && pkt[0]>>4 == 6 && pkt[6] == syscall.IPPROTO_UDP:
		udp = pkt[40:]
	case len(pkt) >= 28 && pkt[0]>>4 == 4 && pkt[9] == syscall.IPPROTO_UDP:
		udp = pkt[int(pkt[0]&0x0f)*4:]
	default:
		return
	}
	if len(udp) < 8 {
		return
	}
	src, dst, length := binary.BigEndian.Uint16(udp), binary.BigEndian.Uint16(udp[2:]), int(binary.BigEndian.Uint16(udp[4:]))
	if length < 8 || length > len(udp) {
		return
	}
	var from string
	switch {
	case src == s.port && dst != s.port:
		from = "oikade"
	case dst == s.port && src != s.port:
		from = "chip-tool"
	default:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = append(s.all, securechannel.Datagram{From: from, Data: append([]byte(nil), udp[8:length]...)})
}

// syncBuffer is a bytes.Buffer that chip-tool's output goes to while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
