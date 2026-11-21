package im

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"oikade.dev/internal/logging"
	"oikade.dev/internal/matter/tlv"
	"oikade.dev/internal/matter/transport"
)

var recordSeeds = flag.Bool("record-seeds", false, "rewrite testdata/fuzz/FuzzDecoders from a Client and Server exchange (TestRecordFuzzSeeds)")

// seedDir holds FuzzDecoders' seeds, which plain go test replays.
const seedDir = "testdata/fuzz/FuzzDecoders"

var opcodeNames = map[uint8]string{
	OpStatusResponse: "status-response", OpReadRequest: "read-request", OpSubscribeRequest: "subscribe-request",
	OpSubscribeResponse: "subscribe-response", OpReportData: "report-data", OpWriteRequest: "write-request",
	OpWriteResponse: "write-response", OpInvokeRequest: "invoke-request", OpInvokeResponse: "invoke-response",
	OpTimedRequest: "timed-request",
}

// recordingConn keeps the Interaction Model payload of every datagram it reads and writes, which a
// plain session leaves unencrypted.
type recordingConn struct {
	net.PacketConn
	mu       sync.Mutex
	payloads map[uint8][][]byte
}

func (c *recordingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, a, err := c.PacketConn.ReadFrom(p)
	if err == nil {
		c.add(p[:n])
	}
	return n, a, err
}

func (c *recordingConn) WriteTo(p []byte, a net.Addr) (int, error) {
	c.add(p)
	return c.PacketConn.WriteTo(p, a)
}

func (c *recordingConn) add(p []byte) {
	_, n, err := transport.ParseHeader(p)
	if err != nil {
		return
	}
	ph, m, err := transport.ParseProtocolHeader(p[n:])
	if err != nil || ph.Protocol != transport.InteractionModel {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads[ph.Opcode] = append(c.payloads[ph.Opcode], append([]byte(nil), p[n+m:]...))
}

// TestRecordFuzzSeeds runs a read, a write, an invoke, a timed invoke and a subscription between a
// Client and a Server, and writes every Interaction Model message they sent as FuzzDecoders' seeds:
//
//	go test ./internal/matter/im -run TestRecordFuzzSeeds -record-seeds
func TestRecordFuzzSeeds(t *testing.T) {
	if !*recordSeeds {
		t.Skip("-record-seeds rewrites the seeds")
	}
	on := false
	c := &Cluster{
		ID: 6, Revision: 1,
		Attributes: []Attribute{{ID: 0, Read: func(_ *Request, w *tlv.Writer, tag tlv.Tag) error {
			w.PutBool(tag, on)
			return nil
		}, Write: func(_ *Request, r *tlv.Reader, _ WriteOp) Status {
			on, _ = r.Bool()
			return StatusSuccess
		}}},
		Commands: []Command{
			{ID: 1, Invoke: func(*Request, *tlv.Reader) (*Response, error) {
				return &Response{Command: 1, Fields: func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 7) }}, nil
			}},
			{ID: 2, Timed: true, Invoke: func(*Request, *tlv.Reader) (*Response, error) { return nil, nil }},
		},
	}
	node := &Node{}
	if err := node.AddEndpoint(&Endpoint{ID: 1, Clusters: []*Cluster{c}}); err != nil {
		t.Fatal(err)
	}
	conn := &recordingConn{PacketConn: listen(t), payloads: map[uint8][][]byte{}}
	dev, cli := serveOn(t, conn), serveOn(t, listen(t))
	(&Server{Node: node, Log: logging.Test(t)}).Register(dev)
	did, _ := dev.AllocateSessionID()
	cid, _ := cli.AllocateSessionID()
	dev.AddSecureSession(transport.SecureSessionConfig{
		LocalSessionID: did, PeerSessionID: cid, Auth: transport.AuthPASE,
		Peer: cli.Addr().(*net.UDPAddr).AddrPort(), Security: plain{},
	})
	client := &Client{Session: cli.AddSecureSession(transport.SecureSessionConfig{
		LocalSessionID: cid, PeerSessionID: did, Peer: dev.Addr().(*net.UDPAddr).AddrPort(), Security: plain{},
	})}
	client.Register(cli)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	onOff := AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0}
	if _, err := client.Write(ctx, onOff, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, true) }); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(ctx, onOff, AttributePath{WildEndpoint: true, WildCluster: true, WildAttribute: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Invoke(ctx, CommandPath{Endpoint: 1, Cluster: 6, Command: 1}, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 1) }, false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Invoke(ctx, CommandPath{Endpoint: 1, Cluster: 6, Command: 2}, nil, true, 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := client.Subscribe(ctx, 0, 60, onOff); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	for op, payloads := range conn.payloads {
		for n, p := range payloads {
			name := filepath.Join(seedDir, fmt.Sprintf("%s-%d", opcodeNames[op], n+1))
			if err := os.WriteFile(name, fmt.Appendf(nil, "go test fuzz v1\n[]byte(%q)\n", p), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
