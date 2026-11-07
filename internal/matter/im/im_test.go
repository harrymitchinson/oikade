package im

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// plain is a Security that doesn't encrypt, so a test can read what goes over the wire.
type plain struct{}

func (plain) Seal(_ *transport.Header, _, p []byte) ([]byte, error) {
	return append([]byte(nil), p...), nil
}
func (plain) Open(_ *transport.Header, _, s []byte) ([]byte, error) { return s, nil }

// countingConn counts the Report Data messages it sends.
type countingConn struct {
	net.PacketConn
	mu      sync.Mutex
	reports int
}

func (c *countingConn) WriteTo(p []byte, a net.Addr) (int, error) {
	if _, n, err := transport.ParseHeader(p); err == nil {
		if ph, _, err := transport.ParseProtocolHeader(p[n:]); err == nil && ph.Protocol == transport.InteractionModel && ph.Opcode == OpReportData {
			c.mu.Lock()
			c.reports++
			c.mu.Unlock()
		}
	}
	return c.PacketConn.WriteTo(p, a)
}

func serveOn(t *testing.T, conn net.PacketConn) *transport.Transport {
	tr := transport.New(conn, transport.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tr.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return tr
}

func listen(t *testing.T) net.PacketConn {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

// pair serves node and returns a client on a plain secure session to it.
func pair(t *testing.T, node *Node) (*Client, *countingConn) {
	c, conn, _, _ := pairServer(t, node)
	return c, conn
}

// pairServer pairs over a session the device sees as PASE, which may do anything.
func pairServer(t *testing.T, node *Node) (*Client, *countingConn, *Server, *transport.Transport) {
	return pairAs(t, node, nil, transport.SecureSessionConfig{Auth: transport.AuthPASE})
}

// pairAs pairs over a session that the device sees as dev describes, served with access.
func pairAs(t *testing.T, node *Node, access AccessControl, devCfg transport.SecureSessionConfig) (*Client, *countingConn, *Server, *transport.Transport) {
	t.Helper()
	return pairWith(t, &Server{Node: node, Access: access, Log: logging.Test(t)}, devCfg)
}

// pairWith is pairAs, served by srv.
func pairWith(t *testing.T, srv *Server, devCfg transport.SecureSessionConfig) (*Client, *countingConn, *Server, *transport.Transport) {
	t.Helper()
	conn := &countingConn{PacketConn: listen(t)}
	dev, cli := serveOn(t, conn), serveOn(t, listen(t))
	srv.Register(dev)
	did, _ := dev.AllocateSessionID()
	cid, _ := cli.AllocateSessionID()
	devCfg.LocalSessionID, devCfg.PeerSessionID = did, cid
	devCfg.Peer, devCfg.Security = cli.Addr().(*net.UDPAddr).AddrPort(), plain{}
	dev.AddSecureSession(devCfg)
	s := cli.AddSecureSession(transport.SecureSessionConfig{LocalSessionID: cid, PeerSessionID: did, Peer: dev.Addr().(*net.UDPAddr).AddrPort(), Security: plain{}})
	c := &Client{Session: s}
	c.Register(cli)
	return c, conn, srv, dev
}

func TestReadIsChunked(t *testing.T) {
	long := strings.Repeat("x", 30)
	c := &Cluster{ID: 0xFFF1FC00, Revision: 1}
	for i := range 100 {
		c.Attributes = append(c.Attributes, Attribute{ID: uint32(i), Read: func(_ *Request, w *tlv.Writer, tag tlv.Tag) error {
			w.PutUTF8(tag, long)
			return nil
		}})
	}
	node := &Node{}
	_ = node.AddEndpoint(&Endpoint{ID: 1, Clusters: []*Cluster{c}})
	client, conn := pair(t, node)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reports, err := client.Read(ctx, AttributePath{WildEndpoint: true, WildCluster: true, WildAttribute: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 100+len(globalAttributes) {
		t.Errorf("%d reports, want %d", len(reports), 100+len(globalAttributes))
	}
	for i, r := range reports[:100] {
		if r.Path.Attribute != uint32(i) {
			t.Fatalf("report %d is attribute %d: out of order", i, r.Path.Attribute)
		}
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.reports < 4 {
		t.Errorf("%d Report Data messages for about 5 KB of reports", conn.reports)
	}
}

// TestLargeListIsSplit reads a list too large for one report: it arrives as an empty list and
// then one append per item, and the client puts it back together.
func TestLargeListIsSplit(t *testing.T) {
	item := strings.Repeat("y", 30)
	c := &Cluster{ID: 0xFFF1FC00, Revision: 1, Attributes: []Attribute{{ID: 0, Read: func(_ *Request, w *tlv.Writer, tag tlv.Tag) error {
		w.StartArray(tag)
		for range 100 {
			w.PutUTF8(tlv.Anonymous, item)
		}
		w.End()
		return nil
	}}}}
	node := &Node{}
	if err := node.AddEndpoint(&Endpoint{ID: 1, Clusters: []*Cluster{c}}); err != nil {
		t.Fatal(err)
	}
	client, conn := pair(t, node)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reports, err := client.Read(ctx, AttributePath{Endpoint: 1, Cluster: c.ID, Attribute: 0})
	if err != nil || len(reports) != 1 || reports[0].Status != StatusSuccess {
		t.Fatalf("%+v, %v", reports, err)
	}
	v, err := DecodeValue(reports[0].Data)
	if list, ok := v.([]any); err != nil || !ok || len(list) != 100 || list[99] != item {
		t.Errorf("list of %d, %v", len(list), err)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.reports < 3 {
		t.Errorf("a 3 KB list took %d Report Data messages", conn.reports)
	}
}

func TestOversizedAttribute(t *testing.T) {
	c := &Cluster{ID: 0xFFF1FC00, Revision: 1, Attributes: []Attribute{{ID: 0, Read: func(_ *Request, w *tlv.Writer, tag tlv.Tag) error {
		w.PutBytes(tag, make([]byte, 2000))
		return nil
	}}}}
	node := &Node{}
	_ = node.AddEndpoint(&Endpoint{ID: 1, Clusters: []*Cluster{c}})
	client, _ := pair(t, node)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reports, err := client.Read(ctx, AttributePath{Endpoint: 1, Cluster: c.ID, Attribute: 0})
	if err != nil || len(reports) != 1 || reports[0].Status != StatusResourceExhausted {
		t.Errorf("%+v, %v; want RESOURCE_EXHAUSTED", reports, err)
	}
}

func TestMalformedRequests(t *testing.T) {
	client, _ := pair(t, &Node{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, payload := range map[string][]byte{
		"not TLV":             {0xFF},
		"attributes not list": {0x15, 0x24, 0x00, 0x01, 0x18},
		"repeated field":      {0x15, 0x36, 0x00, 0x18, 0x36, 0x00, 0x18, 0x18},
		"trailing bytes":      {0x15, 0x18, 0x00},
	} {
		ex, _ := client.Session.NewExchange()
		if err := ex.Send(ctx, imMessage(OpReadRequest, payload)); err != nil {
			t.Fatal(err)
		}
		m, err := ex.Recv(ctx)
		ex.Close()
		if err != nil || m.Opcode != OpStatusResponse {
			t.Errorf("%s: %+v, %v", name, m, err)
			continue
		}
		if st, _ := decodeStatusResponse(m.Payload); st != StatusInvalidAction {
			t.Errorf("%s: status %#x, want INVALID_ACTION", name, st)
		}
	}
}

func FuzzDecoders(f *testing.F) {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	putPaths(&w, tlv.Context(0), []AttributePath{{Endpoint: 1, Cluster: 6, Attribute: 0}, {WildEndpoint: true, WildCluster: true, WildAttribute: true}})
	w.End()
	f.Add(w.Bytes())
	f.Add([]byte{0x15, 0x36, 0x02, 0x15, 0x37, 0x00, 0x24, 0x00, 0x01, 0x18, 0x35, 0x01, 0x18, 0x18, 0x18, 0x18})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeReadRequest(b, false)
		_, _ = decodeReadRequest(b, true)
		_, _ = decodeWriteRequest(b)
		_, _ = decodeInvokeRequest(b)
		_, _ = decodeTimedRequest(b)
		_, _ = decodeStatusResponse(b)
		_, _, _, _, _ = decodeReportData(b)
		_, _ = DecodeValue(b)
	})
}

func onOffNode() (*Node, *Cluster) {
	on := false
	c := &Cluster{ID: 6, Revision: 1, Attributes: []Attribute{{ID: 0, Read: func(_ *Request, w *tlv.Writer, tag tlv.Tag) error {
		w.PutBool(tag, on)
		return nil
	}}}}
	node := &Node{}
	_ = node.AddEndpoint(&Endpoint{ID: 1, Clusters: []*Cluster{c}})
	return node, c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestUnknownSubscriptionIsEnded: a client that has forgotten a subscription answers its next
// report with INVALID_SUBSCRIPTION, and the server ends it.
func TestUnknownSubscriptionIsEnded(t *testing.T) {
	node, c := onOffNode()
	client, _, srv, _ := pairServer(t, node)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, id, _, _, err := client.Subscribe(ctx, 0, 60, AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0})
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	delete(client.subs, id)
	client.mu.Unlock()
	node.Update(func() { c.Changed(0) })
	waitFor(t, "the server to end the subscription", func() bool { return srv.Subscriptions() == 0 })
}

// TestSubscriptionEndsWithItsSession closes the server's end of the session: its subscriptions
// end at once, not at the next report.
func TestSubscriptionEndsWithItsSession(t *testing.T) {
	node, _ := onOffNode()
	client, _, srv, dev := pairServer(t, node)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, _, _, err := client.Subscribe(ctx, 0, 3600, AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0}); err != nil {
		t.Fatal(err)
	}
	if srv.Subscriptions() != 1 {
		t.Fatalf("%d subscriptions right after Subscribe returned", srv.Subscriptions())
	}
	srv.mu.Lock()
	var s *transport.Session
	for _, sub := range srv.subs {
		s = sub.session
	}
	srv.mu.Unlock()
	dev.RemoveSession(s)
	waitFor(t, "the subscription to end", func() bool { return srv.Subscriptions() == 0 })
}
