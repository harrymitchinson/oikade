package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/cert"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

const (
	commissionerNode = 0x0000_0000_0001_1122
	bridgeNode       = 0x0000_0000_0000_0A0B
	testFabricID     = 0x0000_0000_0000_0F0F
)

// commissioner is an in-process commissioner: a fabric's root CA, and a transport to reach the
// node from.
type commissioner struct {
	t    *testing.T
	tr   *transport.Transport
	key  *ecdsa.PrivateKey
	rcac *cert.Certificate
	ipk  []byte
}

func newCommissioner(t *testing.T) *commissioner {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rcac, err := cert.NewRCAC(key, 1, testFabricID, time.Now().Add(-time.Hour).Truncate(time.Second), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ipk := make([]byte, 16)
	_, _ = rand.Read(ipk)
	return &commissioner{t: t, tr: client(t), key: key, rcac: rcac, ipk: ipk}
}

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func addr(n *Node) netip.AddrPort { return netip.AddrPortFrom(netip.IPv6Loopback(), n.cfg.Port) }

func (cm *commissioner) client(s *transport.Session) *im.Client {
	c := &im.Client{Session: s}
	c.Register(cm.tr)
	return c
}

func (cm *commissioner) invoke(c *im.Client, cluster, command uint32, fields func(w *tlv.Writer)) map[uint8]any {
	cm.t.Helper()
	res, err := c.Invoke(ctxFor(cm.t), im.CommandPath{Endpoint: 0, Cluster: cluster, Command: command}, fields, false, 0)
	if err != nil {
		cm.t.Fatalf("invoke %#x/%#x: %v", cluster, command, err)
	}
	if res.Response == nil {
		cm.t.Fatalf("invoke %#x/%#x: no response, status %#x", cluster, command, res.Status)
	}
	v, err := im.DecodeValue(res.Fields)
	if err != nil {
		cm.t.Fatal(err)
	}
	return v.(map[uint8]any)
}

// commission runs commissioning as a commissioner does: PASE, the fail-safe, attestation, CSR,
// AddTrustedRootCertificate and AddNOC, then CommissioningComplete over CASE. It returns the
// CASE client.
func (cm *commissioner) commission(n *Node) *im.Client {
	t := cm.t
	t.Helper()
	s, err := securechannel.EstablishPASE(ctxFor(t), cm.tr, addr(n), testSetup.Passcode, nil)
	if err != nil {
		t.Fatalf("PASE: %v", err)
	}
	pase := cm.client(s)
	arm := func(c *im.Client) {
		f := cm.invoke(c, clusters.IDGeneralCommissioning, 0x00, func(w *tlv.Writer) {
			w.PutUint(tlv.Context(0), 60)
			w.PutUint(tlv.Context(1), 1)
		})
		if f[0] != uint64(0) {
			t.Fatalf("ArmFailSafe: %v", f)
		}
	}
	arm(pase)

	nonce := make([]byte, attestation.NonceSize)
	_, _ = rand.Read(nonce)
	dac := cm.invoke(pase, clusters.IDOperationalCredentials, 0x02, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 1) })[0].([]byte)
	pai := cm.invoke(pase, clusters.IDOperationalCredentials, 0x02, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 2) })[0].([]byte)
	f := cm.invoke(pase, clusters.IDOperationalCredentials, 0x00, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), nonce) })
	trust, err := attestation.TestTrust()
	if err != nil {
		t.Fatal(err)
	}
	result, err := trust.Verify(dac, pai, f[0].([]byte), f[1].([]byte), s.AttestationChallenge(), nonce)
	if err != nil {
		t.Fatalf("attestation: %v", err)
	}

	_, _ = rand.Read(nonce)
	f = cm.invoke(pase, clusters.IDOperationalCredentials, 0x04, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), nonce) })
	csr, err := attestation.VerifyNOCSR(result.DAC, f[0].([]byte), f[1].([]byte), s.AttestationChallenge(), nonce)
	if err != nil {
		t.Fatalf("NOCSR: %v", err)
	}
	pub, err := cert.ParseCSR(csr)
	if err != nil {
		t.Fatal(err)
	}
	noc, err := cert.NewNOC(cm.rcac, cm.key, pub, cert.NOCSubject{NodeID: bridgeNode, FabricID: testFabricID}, cm.rcac.NotBefore, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rcacTLV, _ := cm.rcac.Encode()
	nocTLV, _ := noc.Encode()
	if res, err := pase.Invoke(ctxFor(t), im.CommandPath{Cluster: clusters.IDOperationalCredentials, Command: 0x0B}, func(w *tlv.Writer) {
		w.PutBytes(tlv.Context(0), rcacTLV)
	}, false, 0); err != nil || res.Status != im.StatusSuccess {
		t.Fatalf("AddTrustedRootCertificate: %+v, %v", res, err)
	}
	f = cm.invoke(pase, clusters.IDOperationalCredentials, 0x06, func(w *tlv.Writer) {
		w.PutBytes(tlv.Context(0), nocTLV)
		w.PutBytes(tlv.Context(2), cm.ipk)
		w.PutUint(tlv.Context(3), commissionerNode)
		w.PutUint(tlv.Context(4), uint64(attestation.TestVendorID))
	})
	if f[0] != uint64(0) {
		t.Fatalf("AddNOC: %v", f)
	}

	admin := cm.caseClient(n)
	if f := cm.invoke(admin, clusters.IDGeneralCommissioning, 0x04, nil); f[0] != uint64(0) {
		t.Fatalf("CommissioningComplete: %v", f)
	}
	return admin
}

// caseClient opens a CASE session to the node as the commissioner's admin node.
func (cm *commissioner) caseClient(n *Node) *im.Client {
	t := cm.t
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	noc, err := cert.NewNOC(cm.rcac, cm.key, &key.PublicKey, cert.NOCSubject{NodeID: commissionerNode, FabricID: testFabricID}, cm.rcac.NotBefore, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rcacTLV, _ := cm.rcac.Encode()
	nocTLV, _ := noc.Encode()
	f := &securechannel.Fabric{Index: 1, RCAC: rcacTLV, NOC: nocTLV, Key: key, IPK: cm.ipk}
	s, err := securechannel.EstablishCASE(ctxFor(t), cm.tr, addr(n), f, bridgeNode, securechannel.CASEConfig{})
	if err != nil {
		t.Fatalf("CASE: %v", err)
	}
	return cm.client(s)
}

func (f *fakeMDNS) operational() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, svc := range f.services {
		if svc.Type == mdns.OperationalType {
			names = append(names, svc.Instance)
		}
	}
	return names
}

func (n *Node) secureSessions(mode transport.AuthMode) int {
	count := 0
	for _, s := range n.sessions.list() {
		if s.AuthMode() == mode {
			count++
		}
	}
	return count
}

// TestCommissionAndRestart commissions a node in process, restarts it on the same data, reaches
// it again over CASE, and removes the fabric, which opens the initial window again.
func TestCommissionAndRestart(t *testing.T) {
	dir := t.TempDir()
	n, fake, stop := start(t, Config{DataDir: dir})
	waitFor(t, "the initial window", func() bool { return fake.commissionable() != nil })
	cm := newCommissioner(t)
	admin := cm.commission(n)

	waitFor(t, "the window to close and the fabric to be advertised", func() bool {
		return fake.commissionable() == nil && !n.WindowOpen() && len(fake.operational()) == 1
	})
	if name := fake.operational()[0]; !strings.HasSuffix(name, "-0000000000000A0B") {
		t.Errorf("operational instance %q", name)
	}
	waitFor(t, "EndPASE to close the PASE session", func() bool { return n.secureSessions(transport.AuthPASE) == 0 })

	// A peer's CloseSession ends its session.
	ex, err := admin.Session.NewExchange()
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Send(ctxFor(t), securechannel.StatusReport{
		General: securechannel.GeneralSuccess, Protocol: transport.SecureChannel, Code: securechannel.CodeCloseSession,
	}.Message()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the node to close the CASE session", func() bool { return n.secureSessions(transport.AuthCASE) == 0 })

	// After a restart the node is still on the fabric: it advertises it, doesn't open a window,
	// and answers CASE.
	port := n.cfg.Port
	stop()
	n, fake, _ = start(t, Config{DataDir: dir, Port: port})
	waitFor(t, "the fabric to be advertised after the restart", func() bool { return len(fake.operational()) == 1 })
	if fake.commissionable() != nil || n.WindowOpen() {
		t.Error("a commissioned node opened its window after a restart")
	}
	admin = cm.caseClient(n)
	reports, err := admin.Read(ctxFor(t), im.AttributePath{Endpoint: 0, Cluster: clusters.IDOperationalCredentials, Attribute: 0x0005})
	if err != nil || len(reports) != 1 {
		t.Fatalf("read CurrentFabricIndex: %+v, %v", reports, err)
	}

	// Removing the last fabric withdraws it and opens the initial window again.
	f := cm.invoke(admin, clusters.IDOperationalCredentials, 0x0A, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 1) })
	if f[0] != uint64(0) {
		t.Fatalf("RemoveFabric: %v", f)
	}
	waitFor(t, "the window to reopen", func() bool {
		return len(fake.operational()) == 0 && fake.commissionable() != nil && n.WindowOpen()
	})
}
