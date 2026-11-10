// Package nodetest is an in-process Matter commissioner, for tests that need a controller to pair
// with a node and talk to it, whether the node is in the test's process or a host's.
package nodetest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/netip"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/cert"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

const (
	commissionerNode = 0x0000_0000_0001_1122
	// BridgeNode is the node ID a Commissioner gives what it commissions.
	BridgeNode   = 0x0000_0000_0000_0A0B
	testFabricID = 0x0000_0000_0000_0F0F
)

// Commissioner is an in-process commissioner: a fabric's root CA, and a transport to reach the
// node from.
type Commissioner struct {
	t    testing.TB
	tr   *transport.Transport
	key  *ecdsa.PrivateKey
	rcac *cert.Certificate
	ipk  []byte

	admin *securechannel.Fabric // the admin node's operational identity, once made
}

// New returns a Commissioner with a fabric of its own.
func New(t testing.TB) *Commissioner {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rcac, err := cert.NewRCAC(key, 1, testFabricID, time.Now().Add(-time.Hour).Truncate(time.Second), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ipk := make([]byte, 16)
	_, _ = rand.Read(ipk)
	return &Commissioner{t: t, tr: listen(t), key: key, rcac: rcac, ipk: ipk}
}

func listen(t testing.TB) *transport.Transport {
	t.Helper()
	tr, err := transport.Listen("[::1]:0", transport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tr.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return tr
}

// Context bounds one call to a node.
func Context(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Addr is a node on this machine.
func Addr(port uint16) netip.AddrPort { return netip.AddrPortFrom(netip.IPv6Loopback(), port) }

func (cm *Commissioner) client(s *transport.Session) *im.Client {
	c := &im.Client{Session: s}
	c.Register(cm.tr)
	return c
}

// Invoke calls a command with a response on the root endpoint.
func (cm *Commissioner) Invoke(c *im.Client, cluster, command uint32, fields func(w *tlv.Writer)) map[uint8]any {
	cm.t.Helper()
	res, err := c.Invoke(Context(cm.t), im.CommandPath{Endpoint: 0, Cluster: cluster, Command: command}, fields, false, 0)
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

// Commission runs commissioning as a commissioner does: PASE, the fail-safe, attestation, CSR,
// AddTrustedRootCertificate and AddNOC, then CommissioningComplete over CASE. It returns the
// CASE client.
func (cm *Commissioner) Commission(addr netip.AddrPort, passcode uint32) *im.Client {
	t := cm.t
	t.Helper()
	s, err := securechannel.EstablishPASE(Context(t), cm.tr, addr, passcode, nil)
	if err != nil {
		t.Fatalf("PASE: %v", err)
	}
	pase := cm.client(s)
	arm := func(c *im.Client) {
		f := cm.Invoke(c, clusters.IDGeneralCommissioning, 0x00, func(w *tlv.Writer) {
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
	dac := cm.Invoke(pase, clusters.IDOperationalCredentials, 0x02, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 1) })[0].([]byte)
	pai := cm.Invoke(pase, clusters.IDOperationalCredentials, 0x02, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 2) })[0].([]byte)
	f := cm.Invoke(pase, clusters.IDOperationalCredentials, 0x00, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), nonce) })
	trust, err := attestation.TestTrust()
	if err != nil {
		t.Fatal(err)
	}
	result, err := trust.Verify(dac, pai, f[0].([]byte), f[1].([]byte), s.AttestationChallenge(), nonce)
	if err != nil {
		t.Fatalf("attestation: %v", err)
	}

	_, _ = rand.Read(nonce)
	f = cm.Invoke(pase, clusters.IDOperationalCredentials, 0x04, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), nonce) })
	csr, err := attestation.VerifyNOCSR(result.DAC, f[0].([]byte), f[1].([]byte), s.AttestationChallenge(), nonce)
	if err != nil {
		t.Fatalf("NOCSR: %v", err)
	}
	pub, err := cert.ParseCSR(csr)
	if err != nil {
		t.Fatal(err)
	}
	noc, err := cert.NewNOC(cm.rcac, cm.key, pub, cert.NOCSubject{NodeID: BridgeNode, FabricID: testFabricID}, cm.rcac.NotBefore, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rcacTLV, _ := cm.rcac.Encode()
	nocTLV, _ := noc.Encode()
	if res, err := pase.Invoke(Context(t), im.CommandPath{Cluster: clusters.IDOperationalCredentials, Command: 0x0B}, func(w *tlv.Writer) {
		w.PutBytes(tlv.Context(0), rcacTLV)
	}, false, 0); err != nil || res.Status != im.StatusSuccess {
		t.Fatalf("AddTrustedRootCertificate: %+v, %v", res, err)
	}
	f = cm.Invoke(pase, clusters.IDOperationalCredentials, 0x06, func(w *tlv.Writer) {
		w.PutBytes(tlv.Context(0), nocTLV)
		w.PutBytes(tlv.Context(2), cm.ipk)
		w.PutUint(tlv.Context(3), commissionerNode)
		w.PutUint(tlv.Context(4), uint64(attestation.TestVendorID))
	})
	if f[0] != uint64(0) {
		t.Fatalf("AddNOC: %v", f)
	}

	admin := cm.CASE(addr)
	if f := cm.Invoke(admin, clusters.IDGeneralCommissioning, 0x04, nil); f[0] != uint64(0) {
		t.Fatalf("CommissioningComplete: %v", f)
	}
	return admin
}

// CASE opens a CASE session to the node as the commissioner's admin node.
func (cm *Commissioner) CASE(addr netip.AddrPort) *im.Client {
	t := cm.t
	t.Helper()
	s, err := securechannel.EstablishCASE(Context(t), cm.tr, addr, cm.adminFabric(), BridgeNode, securechannel.CASEConfig{})
	if err != nil {
		t.Fatalf("CASE: %v", err)
	}
	return cm.client(s)
}

// AcceptCASE answers CASE as the admin node, as a controller does for a node that resumes its
// subscriptions.
func (cm *Commissioner) AcceptCASE() {
	(&securechannel.CASEResponder{Transport: cm.tr, Fabrics: adminTable{cm.adminFabric()}, Log: logging.Test(cm.t)}).Register()
}

type adminTable struct{ f *securechannel.Fabric }

func (a adminTable) Fabrics() []*securechannel.Fabric { return []*securechannel.Fabric{a.f} }

// adminFabric is the admin node's operational identity on the commissioner's fabric, made once.
func (cm *Commissioner) adminFabric() *securechannel.Fabric {
	if cm.admin != nil {
		return cm.admin
	}
	t := cm.t
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	noc, err := cert.NewNOC(cm.rcac, cm.key, &key.PublicKey, cert.NOCSubject{NodeID: commissionerNode, FabricID: testFabricID}, cm.rcac.NotBefore, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rcacTLV, _ := cm.rcac.Encode()
	nocTLV, _ := noc.Encode()
	cm.admin = &securechannel.Fabric{Index: 1, RCAC: rcacTLV, NOC: nocTLV, Key: key, IPK: cm.ipk}
	return cm.admin
}
