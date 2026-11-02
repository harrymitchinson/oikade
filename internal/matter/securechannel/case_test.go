package securechannel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"

	"github.com/harrymitchinson/oikade/internal/matter/cert"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestOperationalIPK checks connectedhomeip's group operational key vectors
// (src/crypto/tests/TestChipCryptoPAL.cpp, TestGroup_OperationalKeyDerivation), the last of which
// is the spec's example.
func TestOperationalIPK(t *testing.T) {
	for _, v := range []struct{ epoch, cfid, want string }{
		{"a0a1a2a3a4a5a6a7a8a9aaabacadaeaf", "2906C908D115D362", "1f19ed3cef8a211baf306faeeee7aac6"},
		{"b0b1b2b3b4b5b6b7b8b9babbbcbdbebf", "2906C908D115D362", "aa979a48bd8cdf293a0709b9c1eb1930"},
		{"235bf7e62823d358dca4ba50b1535f4b", "87e1b004e235a130", "a6f5306baf6d050af23ba4bd6b9dd960"},
	} {
		cfid := unhex(t, v.cfid)
		var id uint64
		for _, b := range cfid {
			id = id<<8 | uint64(b)
		}
		got, err := operationalIPK(unhex(t, v.epoch), id)
		if err != nil || !bytes.Equal(got, unhex(t, v.want)) {
			t.Errorf("operationalIPK(%s, %s) = %x, %v, want %s", v.epoch, v.cfid, got, err, v.want)
		}
	}
	if _, err := operationalIPK(make([]byte, 15), 1); err == nil {
		t.Error("operationalIPK accepted a 15-byte epoch key")
	}
}

// TestDestinationID checks the spec's example, from connectedhomeip's
// src/protocols/secure_channel/tests/TestCASESession.cpp (DestinationIdTest).
func TestDestinationID(t *testing.T) {
	root := unhex(t, "044a9f42b1ca4840d37292bbc7f6a7e11e22200c976fc900dbc98a7a383a641cb8254a2e56d4e295a847943b4e3897c4a773e930277b4d9fbede8a052686bfacfa")
	ipk := unhex(t, "9bc61cd9c62a2df6d64dfcaa9dc472d4")
	random := unhex(t, "7e171231568dfa17206b3accf8faec2f4d21b580113196f47c7c4deb810a73dc")
	want := unhex(t, "dc35dd5fc9134cc5544538c9c3fc4297c1ec3370c839136a80e10796451d4c53")
	const fabricID, nodeID = 0x2906C908D115D362, 0xCD5544AA7B13EF14
	if got := destinationID(ipk, random, root, fabricID, nodeID); !bytes.Equal(got, want) {
		t.Errorf("destinationID = %x, want %x", got, want)
	}
	if got := destinationID(ipk, random, root, fabricID, nodeID+1); bytes.Equal(got, want) {
		t.Error("destinationID didn't change with the node ID")
	}
}

// testCA is a fabric's root, from which tests issue node certificates.
type testCA struct {
	key      *ecdsa.PrivateKey
	rcac     *cert.Certificate
	rcacTLV  []byte
	fabricID uint64
	ipk      []byte
	// With an intermediate, NOCs are issued by it.
	icacKey *ecdsa.PrivateKey
	icac    *cert.Certificate
	icacTLV []byte
}

var notBefore = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func encode(t *testing.T, c *cert.Certificate) []byte {
	t.Helper()
	b, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newCA(t *testing.T, intermediate bool) *testCA {
	t.Helper()
	const fabricID = 0xFAB1
	ca := &testCA{key: newKey(t), fabricID: fabricID, ipk: bytes.Repeat([]byte{0x5a}, 16)}
	var err error
	if ca.rcac, err = cert.NewRCAC(ca.key, 1, fabricID, notBefore, time.Time{}); err != nil {
		t.Fatal(err)
	}
	ca.rcacTLV = encode(t, ca.rcac)
	if intermediate {
		ca.icacKey = newKey(t)
		if ca.icac, err = cert.NewICAC(ca.rcac, ca.key, ca.icacKey, 2, fabricID, notBefore, time.Time{}); err != nil {
			t.Fatal(err)
		}
		ca.icacTLV = encode(t, ca.icac)
	}
	return ca
}

// fabric issues a node a NOC, valid until notAfter (zero for no expiry), and returns its Fabric.
func (ca *testCA) fabric(t *testing.T, index uint8, nodeID uint64, cats []uint32, notAfter time.Time) *Fabric {
	t.Helper()
	key := newKey(t)
	issuer, issuerKey := ca.rcac, ca.key
	if ca.icac != nil {
		issuer, issuerKey = ca.icac, ca.icacKey
	}
	noc, err := cert.NewNOC(issuer, issuerKey, &key.PublicKey, cert.NOCSubject{NodeID: nodeID, FabricID: ca.fabricID, CATs: cats}, notBefore, notAfter)
	if err != nil {
		t.Fatal(err)
	}
	return &Fabric{Index: index, RCAC: ca.rcacTLV, ICAC: ca.icacTLV, NOC: encode(t, noc), Key: key, IPK: ca.ipk}
}

type fabrics []*Fabric

func (f fabrics) Fabrics() []*Fabric { return f }

// opcodeConn records the Secure Channel opcodes sent in the clear.
type opcodeConn struct {
	net.PacketConn
	mu  sync.Mutex
	ops []uint8
}

func (c *opcodeConn) WriteTo(p []byte, a net.Addr) (int, error) {
	if h, n, err := transport.ParseHeader(p); err == nil && h.SessionID == 0 {
		if ph, _, err := transport.ParseProtocolHeader(p[n:]); err == nil && ph.Protocol == transport.SecureChannel && ph.Opcode != 0x10 {
			c.mu.Lock()
			c.ops = append(c.ops, ph.Opcode)
			c.mu.Unlock()
		}
	}
	return c.PacketConn.WriteTo(p, a)
}

func (c *opcodeConn) sent(op uint8) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.ops, op)
}

func (c *opcodeConn) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = nil
}

type caseNode struct {
	t    *transport.Transport
	conn *opcodeConn
}

func newCASENode(t *testing.T) *caseNode {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn := &opcodeConn{PacketConn: pc}
	tr := transport.New(conn, transport.Options{Log: testr.New(t)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tr.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &caseNode{t: tr, conn: conn}
}

// caseDevice is a bridge node answering CASE and serving the Interaction Model.
type caseDevice struct {
	*caseNode
	responder *CASEResponder
	sessions  chan *transport.Session
	now       atomic.Pointer[time.Time] // the device's clock, if set
}

func newCASEDevice(t *testing.T, fs ...*Fabric) *caseDevice {
	t.Helper()
	d := &caseDevice{caseNode: newCASENode(t), sessions: make(chan *transport.Session, 10)}
	node := &im.Node{}
	_ = node.AddEndpoint(&im.Endpoint{ID: 0, Clusters: []*im.Cluster{{
		ID: basicInformation, Revision: 1,
		Attributes: []im.Attribute{{ID: 1, Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error {
			w.PutUTF8(tag, "Oikade") // VendorName
			return nil
		}}},
	}}})
	(&im.Server{Node: node, Access: allowAll{}, Log: testr.New(t)}).Register(d.t)
	d.responder = &CASEResponder{
		Transport: d.t, Fabrics: fabrics(fs), Resumption: NewResumptionStore(0), Log: testr.New(t),
		Params:    &SessionParams{Idle: 500 * time.Millisecond},
		OnSession: func(s *transport.Session) { d.sessions <- s },
		Now: func() time.Time {
			if t := d.now.Load(); t != nil {
				return *t
			}
			return time.Now()
		},
	}
	d.responder.Register()
	return d
}

func (d *caseDevice) active() int {
	d.responder.mu.Lock()
	defer d.responder.mu.Unlock()
	return d.responder.active
}

func (d *caseDevice) addr() netip.AddrPort { return d.t.Addr().(*net.UDPAddr).AddrPort() }

func (d *caseDevice) session(t *testing.T) *transport.Session {
	t.Helper()
	select {
	case s := <-d.sessions:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("the device reported no session")
		return nil
	}
}

// basicInformation is the Basic Information cluster's ID.
const basicInformation = 0x0028

// allowAll lets every CASE session do anything.
type allowAll struct{}

func (allowAll) Allowed(*im.Subject, *im.Endpoint, uint32, im.Privilege) bool { return true }

// readVendor reads the bridge's vendor name over s.
func readVendor(t *testing.T, tr *transport.Transport, s *transport.Session) {
	t.Helper()
	c := &im.Client{Session: s}
	c.Register(tr)
	reports, err := c.Read(timeout(t), im.AttributePath{Endpoint: 0, Cluster: basicInformation, Attribute: 1})
	if err != nil || len(reports) != 1 {
		t.Fatalf("Read: %v, %v", reports, err)
	}
	if v, err := im.DecodeValue(reports[0].Data); err != nil || v != "Oikade" {
		t.Fatalf("vendor name %v, %v", v, err)
	}
}

const (
	deviceNode     = 0x0000_0000_0000_0B01
	controllerNode = 0x0000_0000_0001_B669
)

func TestCASE(t *testing.T) {
	for _, intermediate := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "intermediate"}[intermediate], func(t *testing.T) {
			ca := newCA(t, intermediate)
			cats := []uint32{0x0001_0001}
			dev := newCASEDevice(t, ca.fabric(t, 3, deviceNode, nil, time.Time{}))
			ctl := newCASENode(t)
			ctlFabric := ca.fabric(t, 1, controllerNode, cats, time.Time{})

			s, err := EstablishCASE(timeout(t), ctl.t, dev.addr(), ctlFabric, deviceNode, CASEConfig{})
			if err != nil {
				t.Fatalf("EstablishCASE: %v", err)
			}
			ds := dev.session(t)
			if s.PeerNodeID() != deviceNode || s.FabricIndex() != 1 || len(s.CATs()) != 0 {
				t.Errorf("controller's session: node %x, fabric %d, CATs %v", s.PeerNodeID(), s.FabricIndex(), s.CATs())
			}
			if ds.PeerNodeID() != controllerNode || ds.FabricIndex() != 3 || !slices.Equal(ds.CATs(), cats) {
				t.Errorf("device's session: node %x, fabric %d, CATs %v", ds.PeerNodeID(), ds.FabricIndex(), ds.CATs())
			}
			if s.AuthMode() != transport.AuthCASE || ds.AuthMode() != transport.AuthCASE ||
				len(s.AttestationChallenge()) != 16 || !bytes.Equal(s.AttestationChallenge(), ds.AttestationChallenge()) {
				t.Errorf("auth modes %d/%d, attestation challenges %x/%x", s.AuthMode(), ds.AuthMode(), s.AttestationChallenge(), ds.AttestationChallenge())
			}
			readVendor(t, ctl.t, s)
		})
	}
}

// statusCode returns the Secure Channel code of the status report err carries.
func statusCode(t *testing.T, err error) uint16 {
	t.Helper()
	var sr StatusReport
	if !errors.Is(err, ErrCASEFailed) || !errors.As(err, &sr) || sr.General != GeneralFailure {
		t.Fatalf("got %v, want a failure status report", err)
	}
	return sr.Code
}

func TestCASERefused(t *testing.T) {
	ca := newCA(t, false)
	devFabric := ca.fabric(t, 1, deviceNode, nil, time.Time{})

	t.Run("another root", func(t *testing.T) {
		// The same fabric ID and node IDs, but another root: the destination ID matches nothing.
		other := newCA(t, false)
		dev := newCASEDevice(t, devFabric)
		_, err := EstablishCASE(timeout(t), newCASENode(t).t, dev.addr(), other.fabric(t, 1, controllerNode, nil, time.Time{}), deviceNode, CASEConfig{})
		if code := statusCode(t, err); code != CodeNoSharedTrustRoots {
			t.Errorf("code %d, want NoSharedTrustRoots", code)
		}
	})
	t.Run("NOC from another root", func(t *testing.T) {
		// The controller names the device's root, but its NOC is from another.
		other := newCA(t, false)
		f := other.fabric(t, 1, controllerNode, nil, time.Time{})
		f.RCAC = ca.rcacTLV
		dev := newCASEDevice(t, devFabric)
		_, err := EstablishCASE(timeout(t), newCASENode(t).t, dev.addr(), f, deviceNode, CASEConfig{})
		if code := statusCode(t, err); code != CodeInvalidParameter {
			t.Errorf("code %d, want InvalidParameter", code)
		}
	})
	t.Run("initiator's wrong signature", func(t *testing.T) {
		f := ca.fabric(t, 1, controllerNode, nil, time.Time{})
		f.Key = newKey(t) // not the NOC's key
		dev := newCASEDevice(t, devFabric)
		_, err := EstablishCASE(timeout(t), newCASENode(t).t, dev.addr(), f, deviceNode, CASEConfig{})
		if code := statusCode(t, err); code != CodeInvalidParameter {
			t.Errorf("code %d, want InvalidParameter", code)
		}
	})
	t.Run("responder's wrong signature", func(t *testing.T) {
		bad := *devFabric
		bad.Key = newKey(t)
		dev := newCASEDevice(t, &bad)
		_, err := EstablishCASE(timeout(t), newCASENode(t).t, dev.addr(), ca.fabric(t, 1, controllerNode, nil, time.Time{}), deviceNode, CASEConfig{})
		if !errors.Is(err, ErrCASEFailed) || errors.As(err, new(StatusReport)) {
			t.Fatalf("got %v, want the controller to refuse the device", err)
		}
		// The controller tells the device it gave up.
		for deadline := time.Now().Add(3 * time.Second); dev.t.SecureSessions() != 0 || dev.active() != 0; {
			if time.Now().After(deadline) {
				t.Fatal("the device still holds the handshake")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	t.Run("wrong node", func(t *testing.T) {
		// The device is on the fabric, but isn't the node the controller asked for.
		dev := newCASEDevice(t, devFabric)
		_, err := EstablishCASE(timeout(t), newCASENode(t).t, dev.addr(), ca.fabric(t, 1, controllerNode, nil, time.Time{}), deviceNode+1, CASEConfig{})
		if code := statusCode(t, err); code != CodeNoSharedTrustRoots {
			t.Errorf("code %d, want NoSharedTrustRoots", code)
		}
	})
}

func TestCASEValidity(t *testing.T) {
	ca := newCA(t, false)
	expires := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	dev := newCASEDevice(t, ca.fabric(t, 1, deviceNode, nil, time.Time{}))
	ctl := ca.fabric(t, 1, controllerNode, nil, expires)

	for _, v := range []struct {
		name string
		now  time.Time
		ok   bool
	}{
		{"valid", expires.Add(-time.Hour), true},
		{"expired", expires.Add(time.Hour), false},
		{"clock never set", time.Unix(0, 0), true},
	} {
		t.Run(v.name, func(t *testing.T) {
			dev.now.Store(&v.now)
			_, err := EstablishCASE(timeout(t), newCASENode(t).t, dev.addr(), ctl, deviceNode, CASEConfig{})
			if v.ok && err != nil {
				t.Fatalf("EstablishCASE: %v", err)
			}
			if !v.ok && statusCode(t, err) != CodeInvalidParameter {
				t.Errorf("got %v, want InvalidParameter", err)
			}
		})
	}
}

func TestCASEResumption(t *testing.T) {
	ca := newCA(t, true)
	devFabric := ca.fabric(t, 2, deviceNode, nil, time.Time{})
	dev := newCASEDevice(t, devFabric)
	ctl := newCASENode(t)
	ctlFabric := ca.fabric(t, 1, controllerNode, []uint32{0x0002_0001}, time.Time{})
	store := NewResumptionStore(0)
	cfg := CASEConfig{Resumption: store}

	if _, err := EstablishCASE(timeout(t), ctl.t, dev.addr(), ctlFabric, deviceNode, cfg); err != nil {
		t.Fatalf("first CASE: %v", err)
	}
	dev.session(t)
	first, _ := store.byPeer(1, deviceNode)

	dev.conn.reset()
	s, err := EstablishCASE(timeout(t), ctl.t, dev.addr(), ctlFabric, deviceNode, cfg)
	if err != nil {
		t.Fatalf("resumed CASE: %v", err)
	}
	ds := dev.session(t)
	if !dev.conn.sent(OpSigma2Resume) || dev.conn.sent(OpSigma2) {
		t.Fatalf("the device sent %x, want Sigma2_Resume", dev.conn.ops)
	}
	if ds.PeerNodeID() != controllerNode || ds.FabricIndex() != 2 || !slices.Equal(ds.CATs(), []uint32{0x0002_0001}) {
		t.Errorf("device's resumed session: node %x, fabric %d, CATs %v", ds.PeerNodeID(), ds.FabricIndex(), ds.CATs())
	}
	if again, _ := store.byPeer(1, deviceNode); again.id == first.id {
		t.Error("resumption didn't rotate the resumption ID")
	}
	readVendor(t, ctl.t, s)

	// A resumption ID is good once: offering the first one again gets a full CASE.
	store.save(first)
	dev.conn.reset()
	if _, err := EstablishCASE(timeout(t), ctl.t, dev.addr(), ctlFabric, deviceNode, cfg); err != nil {
		t.Fatalf("CASE with a spent resumption ID: %v", err)
	}
	dev.session(t)
	if !dev.conn.sent(OpSigma2) || dev.conn.sent(OpSigma2Resume) {
		t.Fatalf("the device sent %x, want Sigma2", dev.conn.ops)
	}

	// A device that forgot us falls back to a full CASE.
	dev.responder.Resumption.RemoveFabric(2)
	dev.conn.reset()
	s, err = EstablishCASE(timeout(t), ctl.t, dev.addr(), ctlFabric, deviceNode, cfg)
	if err != nil {
		t.Fatalf("CASE with an unknown resumption ID: %v", err)
	}
	dev.session(t)
	if !dev.conn.sent(OpSigma2) || dev.conn.sent(OpSigma2Resume) {
		t.Fatalf("the device sent %x, want Sigma2", dev.conn.ops)
	}
	readVendor(t, ctl.t, s)
	if dev.responder.Resumption.Len() != 1 {
		t.Errorf("the device remembers %d peers, want 1", dev.responder.Resumption.Len())
	}

	// A resumption ID with a MIC from another secret falls back too.
	res, _ := store.byPeer(1, deviceNode)
	res.secret = bytes.Repeat([]byte{1}, 32)
	store.save(res)
	dev.conn.reset()
	if _, err := EstablishCASE(timeout(t), ctl.t, dev.addr(), ctlFabric, deviceNode, cfg); err != nil {
		t.Fatalf("CASE with a wrong resumption MIC: %v", err)
	}
	dev.session(t)
	if !dev.conn.sent(OpSigma2) {
		t.Fatalf("the device sent %x, want Sigma2", dev.conn.ops)
	}
}

// TestResumptionOtherFabric checks a resumption doesn't carry over to another fabric that took
// its index.
func TestResumptionOtherFabric(t *testing.T) {
	a, b := newCA(t, false), newCA(t, false)
	fa, err := a.fabric(t, 1, deviceNode, nil, time.Time{}).keys()
	if err != nil {
		t.Fatal(err)
	}
	fb, err := b.fabric(t, 1, deviceNode, nil, time.Time{}).keys()
	if err != nil {
		t.Fatal(err)
	}
	res := resumption{fabric: 1, rootKey: fa.rcac.PublicKey, fabricID: fa.fabricID}
	if !res.on(fa) || res.on(fb) {
		t.Errorf("on(own fabric) %v, on(other root, same index) %v", res.on(fa), res.on(fb))
	}
}

func TestCASEBusy(t *testing.T) {
	ca := newCA(t, false)
	dev := newCASEDevice(t, ca.fabric(t, 1, deviceNode, nil, time.Time{}))
	dev.responder.mu.Lock()
	dev.responder.active = maxCASEHandshakes
	dev.responder.mu.Unlock()
	_, err := EstablishCASE(timeout(t), newCASENode(t).t, dev.addr(), ca.fabric(t, 1, controllerNode, nil, time.Time{}), deviceNode, CASEConfig{})
	var sr StatusReport
	if !errors.As(err, &sr) || sr.General != GeneralBusy || sr.Code != CodeBusy || len(sr.Data) != 2 {
		t.Fatalf("got %v, want busy with a wait time", err)
	}
}

func TestResumptionStore(t *testing.T) {
	s := NewResumptionStore(2)
	for i := range 3 {
		s.save(resumption{id: [16]byte{byte(i)}, fabric: 1, node: uint64(i)})
	}
	if s.Len() != 2 {
		t.Fatalf("Len %d, want 2", s.Len())
	}
	if _, ok := s.byPeer(1, 0); ok {
		t.Error("the oldest entry wasn't dropped")
	}
	s.save(resumption{id: [16]byte{9}, fabric: 1, node: 2})
	if _, ok := s.take([]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, func(resumption) bool { return true }); ok || s.Len() != 2 {
		t.Error("a peer's new resumption didn't replace its old one")
	}
	if _, ok := s.take([]byte{9, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, func(resumption) bool { return false }); ok || s.Len() != 2 {
		t.Error("take took an entry it didn't accept")
	}
	if _, ok := s.take([]byte{9, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, func(resumption) bool { return true }); !ok || s.Len() != 1 {
		t.Error("take didn't remove the entry")
	}
	s.RemoveFabric(1)
	if s.Len() != 0 {
		t.Errorf("Len %d after RemoveFabric, want 0", s.Len())
	}
}

func TestCASEMessagesRoundTrip(t *testing.T) {
	s1 := &sigma1{
		initiatorRandom: random(), initiatorSessionID: 7, destinationID: random(),
		initiatorEphPubKey: bytes.Repeat([]byte{4}, 65), params: &SessionParams{Idle: time.Second},
		resumptionID: bytes.Repeat([]byte{1}, 16), resumeMIC: bytes.Repeat([]byte{2}, 16),
	}
	got, err := decodeSigma1(s1.encode())
	if err != nil || !bytes.Equal(got.resumeMIC, s1.resumeMIC) || got.params.Idle != time.Second {
		t.Fatalf("Sigma1: %+v, %v", got, err)
	}
	s1.resumeMIC = nil
	if _, err := decodeSigma1(s1.encode()); !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("Sigma1 with a resumption ID and no MIC: %v", err)
	}
	r := &sigma2Resume{resumptionID: bytes.Repeat([]byte{1}, 16), resumeMIC: bytes.Repeat([]byte{2}, 16), responderSessionID: 9}
	if got, err := decodeSigma2Resume(r.encode()); err != nil || got.responderSessionID != 9 {
		t.Errorf("Sigma2_Resume: %+v, %v", got, err)
	}
	tbe := &tbeData{noc: []byte{1}, signature: make([]byte, 64)}
	if _, err := decodeTBEData(tbe.encode(), true); !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("TBEData2 without a resumption ID: %v", err)
	}
}

// TestPASEAndCASEShareTransport checks both responders answer on one Transport, as a node that's
// being commissioned needs.
func TestPASEAndCASEShareTransport(t *testing.T) {
	ca := newCA(t, false)
	dev := newCASEDevice(t, ca.fabric(t, 1, deviceNode, nil, time.Time{}))
	v, err := mcrypto.NewVerifier(testPasscode, testSalt, 1000)
	if err != nil {
		t.Fatal(err)
	}
	(&Responder{Transport: dev.t, Verifier: v, Salt: testSalt, Iterations: 1000, Log: testr.New(t)}).Register()
	ctl := newCASENode(t)
	ps, err := EstablishPASE(timeout(t), ctl.t, dev.addr(), testPasscode, nil)
	if err != nil {
		t.Fatalf("EstablishPASE: %v", err)
	}
	if ps.AuthMode() != transport.AuthPASE || len(ps.AttestationChallenge()) != 16 || ps.FabricIndex() != 0 {
		t.Errorf("PASE session: auth %d, challenge %x, fabric %d", ps.AuthMode(), ps.AttestationChallenge(), ps.FabricIndex())
	}
	ps.SetFabricIndex(4)
	if ps.FabricIndex() != 4 {
		t.Errorf("FabricIndex %d after SetFabricIndex(4)", ps.FabricIndex())
	}
	if _, err := EstablishCASE(timeout(t), ctl.t, dev.addr(), ca.fabric(t, 1, controllerNode, nil, time.Time{}), deviceNode, CASEConfig{}); err != nil {
		t.Fatalf("EstablishCASE: %v", err)
	}
}
