package clusters

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"

	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/cert"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/fabric"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

const (
	commissionerNode = 0x0000_0000_0001_1122
	deviceNode       = 0x0000_0000_0000_0A0B
	otherNode        = 0x0000_0000_0000_3344
	testFabricID     = 0x0000_0000_0000_0F0F
)

// node is a bridge with the commissioning clusters, as a device serves it.
type node struct {
	dir     string
	table   *fabric.Table
	comm    *Commissioning
	bridge  *Bridge
	server  *im.Server
	devTr   *transport.Transport
	cliTr   *transport.Transport
	devAddr net.Addr

	resumption *securechannel.ResumptionStore

	mu        sync.Mutex
	added     []uint8
	removed   []uint8
	updated   []uint8
	endedPASE int
}

func newNode(t *testing.T, dir string) *node {
	t.Helper()
	n := &node{dir: dir, devTr: serve(t), cliTr: serve(t), resumption: securechannel.NewResumptionStore(8)}
	n.devAddr = n.devTr.Addr()
	var err error
	if n.table, err = fabric.Open(fabric.DirStore{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	creds, err := attestation.Test()
	if err != nil {
		t.Fatal(err)
	}
	n.comm, err = NewCommissioning(CommissioningConfig{
		Fabrics: n.table, Attestation: creds, Log: testr.New(t),
		Hooks: Hooks{
			FabricAdded: func(i uint8) {
				n.mu.Lock()
				defer n.mu.Unlock()
				n.added = append(n.added, i)
			},
			FabricRemoved: func(i uint8) {
				n.resumption.RemoveFabric(i)
				n.mu.Lock()
				defer n.mu.Unlock()
				n.removed = append(n.removed, i)
			},
			FabricUpdated: func(i uint8) {
				n.mu.Lock()
				defer n.mu.Unlock()
				n.updated = append(n.updated, i)
			},
			EndPASE: func() {
				n.mu.Lock()
				defer n.mu.Unlock()
				n.endedPASE++
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	n.bridge = NewBridge(Info{VendorName: "Oikade", VendorID: attestation.TestVendorID, ProductName: "Bridge", ProductID: attestation.TestProductID}, n.comm.Clusters()...)
	onoff := NewOnOff()
	if _, err := n.bridge.AddDevice(2, DeviceOnOffLight, "Porch", "porch-1", onoff.Cluster()); err != nil {
		t.Fatal(err)
	}
	n.server = &im.Server{Node: n.bridge.Node, Access: n.comm, Log: testr.New(t)}
	n.server.Register(n.devTr)
	v, err := mcrypto.NewVerifier(passcode, salt, 1000)
	if err != nil {
		t.Fatal(err)
	}
	(&securechannel.Responder{Transport: n.devTr, Verifier: v, Salt: salt, Iterations: 1000, Log: testr.New(t)}).Register()
	(&securechannel.CASEResponder{Transport: n.devTr, Fabrics: n.table.CASE(), Resumption: n.resumption, Log: testr.New(t)}).Register()
	return n
}

// pase opens a PASE session to the node, as a commissioner does.
func (n *node) pase(t *testing.T) *im.Client {
	t.Helper()
	s, err := securechannel.EstablishPASE(timeout(t), n.cliTr, n.devAddr.(*net.UDPAddr).AddrPort(), passcode, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &im.Client{Session: s}
	c.Register(n.cliTr)
	return c
}

// caseSession runs CASE to the node as nodeID, with a NOC from cm on the fabric cm commissioned.
func (n *node) caseSession(t *testing.T, cm *commissioner, nodeID uint64) *im.Client {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	noc, err := cert.NewNOC(cm.rcac, cm.key, &key.PublicKey, cert.NOCSubject{NodeID: nodeID, FabricID: testFabricID}, cm.rcac.NotBefore, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rcacTLV, _ := cm.rcac.Encode()
	nocTLV, _ := noc.Encode()
	f := &securechannel.Fabric{Index: 1, RCAC: rcacTLV, NOC: nocTLV, Key: key, IPK: cm.ipk}
	s, err := securechannel.EstablishCASE(timeout(t), n.cliTr, n.devAddr.(*net.UDPAddr).AddrPort(), f, deviceNode, securechannel.CASEConfig{})
	if err != nil {
		t.Fatalf("CASE as %#x: %v", nodeID, err)
	}
	c := &im.Client{Session: s}
	c.Register(n.cliTr)
	return c
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func invoke(t *testing.T, c *im.Client, cluster, command uint32, fields func(w *tlv.Writer), timed bool) *im.InvokeResult {
	t.Helper()
	res, err := c.Invoke(timeout(t), im.CommandPath{Endpoint: 0, Cluster: cluster, Command: command}, fields, timed, 2000)
	if err != nil {
		t.Fatalf("invoke %#x/%#x: %v", cluster, command, err)
	}
	return res
}

// response returns a command response's fields.
func response(t *testing.T, res *im.InvokeResult, command uint32) map[uint8]any {
	t.Helper()
	if res.Response == nil || res.Response.Command != command {
		t.Fatalf("got %+v, want response %#x", res, command)
	}
	v, err := im.DecodeValue(res.Fields)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[uint8]any)
}

func readAttr(t *testing.T, c *im.Client, cluster, attr uint32) im.AttributeReport {
	t.Helper()
	reports, err := c.Read(timeout(t), im.AttributePath{Endpoint: 0, Cluster: cluster, Attribute: attr})
	if err != nil || len(reports) != 1 {
		t.Fatalf("read %#x/%#x: %+v, %v", cluster, attr, reports, err)
	}
	return reports[0]
}

func armFailSafe(t *testing.T, c *im.Client, seconds uint64) uint64 {
	t.Helper()
	res := invoke(t, c, IDGeneralCommissioning, 0x00, func(w *tlv.Writer) {
		w.PutUint(tlv.Context(0), seconds)
		w.PutUint(tlv.Context(1), 1)
	}, false)
	return response(t, res, 0x01)[0].(uint64)
}

// commissioner is a fabric's administrator, with its root CA.
type commissioner struct {
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
	return &commissioner{key: key, rcac: rcac, ipk: random(16)}
}

// commission runs commissioning up to AddNOC over PASE session c, verifying the node's
// attestation against the test PAA, and returns the new fabric's index.
func (cm *commissioner) commission(t *testing.T, c *im.Client) uint8 {
	t.Helper()
	if code := armFailSafe(t, c, 60); code != commissioningOK {
		t.Fatalf("ArmFailSafe: %d", code)
	}
	res := invoke(t, c, IDGeneralCommissioning, 0x02, func(w *tlv.Writer) {
		w.PutUint(tlv.Context(0), 0)
		w.PutUTF8(tlv.Context(1), "GB")
		w.PutUint(tlv.Context(2), 2)
	}, false)
	if code := response(t, res, 0x03)[0].(uint64); code != commissioningOK {
		t.Fatalf("SetRegulatoryConfig: %d", code)
	}

	// Attestation.
	chain := func(kind uint64) []byte {
		res := invoke(t, c, IDOperationalCredentials, 0x02, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), kind) }, false)
		return response(t, res, 0x03)[0].([]byte)
	}
	dac, pai := chain(1), chain(2)
	nonce := random(attestation.NonceSize)
	res = invoke(t, c, IDOperationalCredentials, 0x00, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), nonce) }, false)
	f := response(t, res, 0x01)
	trust, err := attestation.TestTrust()
	if err != nil {
		t.Fatal(err)
	}
	challenge := c.Session.AttestationChallenge()
	result, err := trust.Verify(dac, pai, f[0].([]byte), f[1].([]byte), challenge, nonce)
	if err != nil {
		t.Fatalf("attestation: %v", err)
	}
	if result.VendorID != attestation.TestVendorID || result.ProductID != attestation.TestProductID {
		t.Errorf("attested as %#04x/%#04x", result.VendorID, result.ProductID)
	}

	// CSR, then the NOC for its key.
	nonce = random(attestation.NonceSize)
	res = invoke(t, c, IDOperationalCredentials, 0x04, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), nonce) }, false)
	f = response(t, res, 0x05)
	csr, err := attestation.VerifyNOCSR(result.DAC, f[0].([]byte), f[1].([]byte), challenge, nonce)
	if err != nil {
		t.Fatalf("NOCSR: %v", err)
	}
	pub, err := cert.ParseCSR(csr)
	if err != nil {
		t.Fatal(err)
	}
	noc, err := cert.NewNOC(cm.rcac, cm.key, pub, cert.NOCSubject{NodeID: deviceNode, FabricID: testFabricID}, cm.rcac.NotBefore, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rcacTLV, _ := cm.rcac.Encode()
	nocTLV, _ := noc.Encode()

	res = invoke(t, c, IDOperationalCredentials, 0x0B, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), rcacTLV) }, false)
	if res.Status != im.StatusSuccess {
		t.Fatalf("AddTrustedRootCertificate: %#x", res.Status)
	}
	res = invoke(t, c, IDOperationalCredentials, 0x06, func(w *tlv.Writer) {
		w.PutBytes(tlv.Context(0), nocTLV)
		w.PutBytes(tlv.Context(2), cm.ipk)
		w.PutUint(tlv.Context(3), commissionerNode)
		w.PutUint(tlv.Context(4), uint64(attestation.TestVendorID))
	}, false)
	f = response(t, res, 0x08)
	if f[0].(uint64) != nocOK {
		t.Fatalf("AddNOC: status %d", f[0])
	}
	return uint8(f[1].(uint64))
}

// writeACL replaces the accessing fabric's ACL: the commissioner administers, and otherNode may
// view endpoint 2.
func writeACL(t *testing.T, c *im.Client) im.Status {
	t.Helper()
	st, err := c.Write(timeout(t), im.AttributePath{Endpoint: 0, Cluster: IDAccessControl, Attribute: 0}, func(w *tlv.Writer, tag tlv.Tag) {
		w.StartArray(tag)
		w.StartStructure(tlv.Anonymous)
		w.PutUint(tlv.Context(1), uint64(im.PrivilegeAdminister))
		w.PutUint(tlv.Context(2), uint64(transport.AuthCASE))
		w.StartArray(tlv.Context(3))
		w.PutUint(tlv.Anonymous, commissionerNode)
		w.End()
		w.PutNull(tlv.Context(4))
		w.PutUint(tagFabricIndex, 9) // ignored: the accessing fabric's
		w.End()
		w.StartStructure(tlv.Anonymous)
		w.PutUint(tlv.Context(1), uint64(im.PrivilegeView))
		w.PutUint(tlv.Context(2), uint64(transport.AuthCASE))
		w.StartArray(tlv.Context(3))
		w.PutUint(tlv.Anonymous, otherNode)
		w.End()
		w.StartArray(tlv.Context(4))
		w.StartStructure(tlv.Anonymous)
		w.PutNull(tlv.Context(0))
		w.PutUint(tlv.Context(1), 2)
		w.PutNull(tlv.Context(2))
		w.End()
		w.End()
		w.End()
		w.End()
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestCommission commissions the bridge in process: PASE, attestation verified against the test
// PAA, CSR, AddTrustedRootCertificate, AddNOC, an ACL write, then CommissioningComplete over CASE.
// Then the fabric's ACL decides what CASE sessions may do, and the fabric survives a restart.
func TestCommission(t *testing.T) {
	dir := t.TempDir()
	n := newNode(t, dir)
	paseClient := n.pase(t)
	cm := newCommissioner(t)
	index := cm.commission(t, paseClient)
	if index != 1 {
		t.Errorf("fabric index %d", index)
	}
	waitFor(t, "FabricAdded after AddNOC", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return slices.Equal(n.added, []uint8{1})
	})
	if st := writeACL(t, paseClient); st != im.StatusSuccess {
		t.Fatalf("ACL write: %#x", st)
	}

	// CommissioningComplete needs CASE.
	res := invoke(t, paseClient, IDGeneralCommissioning, 0x04, nil, false)
	if code := response(t, res, 0x05)[0].(uint64); code != commissioningInvalidAuth {
		t.Errorf("CommissioningComplete over PASE: %d", code)
	}
	admin := n.caseSession(t, cm, commissionerNode)
	if code := armFailSafe(t, admin, 60); code != commissioningOK {
		t.Errorf("re-arming over CASE: %d", code)
	}
	res = invoke(t, admin, IDGeneralCommissioning, 0x04, nil, false)
	if code := response(t, res, 0x05)[0].(uint64); code != commissioningOK {
		t.Fatalf("CommissioningComplete: %d", code)
	}
	waitFor(t, "EndPASE after CommissioningComplete", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.endedPASE == 1
	})

	// The fabric, as the admin reads it.
	fabrics, err := im.DecodeValue(readAttr(t, admin, IDOperationalCredentials, 0x0001).Data)
	if err != nil {
		t.Fatal(err)
	}
	list := fabrics.([]any)
	if len(list) != 1 {
		t.Fatalf("fabrics %+v", list)
	}
	fd := list[0].(map[uint8]any)
	if fd[3] != uint64(testFabricID) || fd[4] != uint64(deviceNode) || fd[2] != uint64(attestation.TestVendorID) || fd[0xFE] != uint64(index) {
		t.Errorf("fabric descriptor %+v", fd)
	}
	acl, _ := im.DecodeValue(readAttr(t, admin, IDAccessControl, 0).Data)
	if entries := acl.([]any); len(entries) != 2 || entries[1].(map[uint8]any)[0xFE] != uint64(index) {
		t.Errorf("ACL %+v", acl)
	}
	if v, _ := im.DecodeValue(readAttr(t, admin, IDOperationalCredentials, 0x0005).Data); v != uint64(index) {
		t.Errorf("CurrentFabricIndex %v", v)
	}

	checkAccess(t, n, cm, index)

	// After a restart, the fabric and its ACL are there.
	again, err := fabric.Open(fabric.DirStore{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Fabrics(), n.table.Fabrics()) {
		t.Errorf("after a restart:\n%+v\nwant\n%+v", again.Fabrics(), n.table.Fabrics())
	}
	f, _ := again.Fabric(index)
	if len(f.ACL) != 2 || f.NodeID != deviceNode || !bytes.Equal(f.IPK, cm.ipk) {
		t.Errorf("reloaded fabric %+v", f)
	}

	// RemoveFabric removes it, and says so. It ends the fail-safe its admin armed.
	if code := armFailSafe(t, admin, 60); code != commissioningOK {
		t.Fatalf("ArmFailSafe: %d", code)
	}
	res = invoke(t, admin, IDOperationalCredentials, 0x0A, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), uint64(index)) }, false)
	if response(t, res, 0x08)[0] != uint64(nocOK) {
		t.Errorf("RemoveFabric: %+v", res)
	}
	waitFor(t, "FabricRemoved", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return reflect.DeepEqual(n.removed, []uint8{index})
	})
	if again, _ := fabric.Open(fabric.DirStore{Dir: dir}); len(again.Fabrics()) != 0 {
		t.Error("removed fabric is still saved")
	}
	n.bridge.Node.View(func() {
		if n.comm.failSafe.armed {
			t.Error("the removed fabric's fail-safe is still armed")
		}
	})
}

// checkAccess checks what writeACL grants: otherNode may view endpoint 2, and nothing else; a
// node not in the ACL may do nothing.
func checkAccess(t *testing.T, n *node, cm *commissioner, index uint8) {
	t.Helper()
	viewer := n.caseSession(t, cm, otherNode)
	reports, err := viewer.Read(timeout(t), im.AttributePath{Endpoint: 2, Cluster: IDOnOff, Attribute: 0})
	if err != nil || len(reports) != 1 || reports[0].Status != im.StatusSuccess {
		t.Errorf("viewer reading endpoint 2: %+v, %v", reports, err)
	}
	if r := readAttr(t, viewer, IDBasicInformation, 0x05); r.Status != im.StatusUnsupportedAccess {
		t.Errorf("viewer reading endpoint 0: %#x", r.Status)
	}
	if res := invoke(t, viewer, IDGeneralCommissioning, 0x04, nil, false); res.Status != im.StatusUnsupportedAccess {
		t.Errorf("viewer invoking: %#x", res.Status)
	}
	stranger := n.caseSession(t, cm, 0x0000_0000_0000_5555)
	if r := readAttr(t, stranger, IDBasicInformation, 0x05); r.Status != im.StatusUnsupportedAccess {
		t.Errorf("stranger reading: %#x", r.Status)
	}
	st, err := stranger.Write(timeout(t), im.AttributePath{Endpoint: 0, Cluster: IDBasicInformation, Attribute: 0x05}, func(w *tlv.Writer, tag tlv.Tag) { w.PutUTF8(tag, "x") })
	if err != nil || st != im.StatusUnsupportedAccess {
		t.Errorf("stranger writing: %#x, %v", st, err)
	}
	if res := invoke(t, stranger, IDOperationalCredentials, 0x0A, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), uint64(index)) }, false); res.Status != im.StatusUnsupportedAccess {
		t.Errorf("stranger invoking: %#x", res.Status)
	}
	if all, err := stranger.Read(timeout(t), wildcard()); err != nil || len(all) != 0 {
		t.Errorf("stranger's wildcard read: %d reports, %v", len(all), err)
	}
}

// TestFailSafeRollsBack lets the fail-safe expire after AddNOC: the fabric is gone, never saved,
// and the PASE session is off it.
func TestFailSafeRollsBack(t *testing.T) {
	dir := t.TempDir()
	n := newNode(t, dir)
	c := n.pase(t)
	cm := newCommissioner(t)
	index := cm.commission(t, c)
	if st := writeACL(t, c); st != im.StatusSuccess {
		t.Fatalf("ACL write: %#x", st)
	}
	if len(n.table.Fabrics()) != 1 {
		t.Fatal("no pending fabric")
	}
	if code := armFailSafe(t, c, 1); code != commissioningOK {
		t.Fatalf("ArmFailSafe: %d", code)
	}
	waitFor(t, "the fail-safe to expire", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.endedPASE == 1 && reflect.DeepEqual(n.removed, []uint8{index})
	})
	if f := n.table.Fabrics(); len(f) != 0 {
		t.Errorf("fabrics after rollback: %+v", f)
	}
	if again, _ := fabric.Open(fabric.DirStore{Dir: dir}); len(again.Fabrics()) != 0 {
		t.Error("rolled-back fabric was saved")
	}
	if v, _ := im.DecodeValue(readAttr(t, c, IDOperationalCredentials, 0x0005).Data); v != uint64(0) {
		t.Errorf("CurrentFabricIndex after rollback: %v", v)
	}
	if v, _ := im.DecodeValue(readAttr(t, c, IDGeneralCommissioning, 0x0000).Data); v != uint64(0) {
		t.Errorf("Breadcrumb after rollback: %v", v)
	}
	// Commissioning can start again.
	if index := newCommissioner(t).commission(t, c); index == 0 {
		t.Error("no fabric the second time")
	}
}

// TestCommissioningOrder: the operational credentials commands need an armed fail-safe, and
// AddNOC a CSR first.
func TestCommissioningOrder(t *testing.T) {
	n := newNode(t, t.TempDir())
	c := n.pase(t)
	res := invoke(t, c, IDOperationalCredentials, 0x04, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), random(32)) }, false)
	if res.Status != im.StatusFailsafeRequired {
		t.Errorf("CSRRequest without a fail-safe: %#x", res.Status)
	}
	res = invoke(t, c, IDGeneralCommissioning, 0x04, nil, false)
	if code := response(t, res, 0x05)[0].(uint64); code != commissioningNoFailSafe {
		t.Errorf("CommissioningComplete without a fail-safe: %d", code)
	}
	armFailSafe(t, c, 60)
	res = invoke(t, c, IDOperationalCredentials, 0x06, func(w *tlv.Writer) {
		w.PutBytes(tlv.Context(0), []byte{1})
		w.PutBytes(tlv.Context(2), random(16))
		w.PutUint(tlv.Context(3), commissionerNode)
		w.PutUint(tlv.Context(4), 0xFFF1)
	}, false)
	if response(t, res, 0x08)[0] != uint64(nocMissingCSR) {
		t.Errorf("AddNOC without a CSR: %+v", res)
	}
}

// TestCommissioningWindow opens an enhanced window, refuses a second, and revokes it.
func TestCommissioningWindow(t *testing.T) {
	n := newNode(t, t.TempDir())
	var mu sync.Mutex
	var opened []Window
	closed := 0
	n.comm.hooks.OpenWindow = func(w Window) { mu.Lock(); opened = append(opened, w); mu.Unlock() }
	n.comm.hooks.CloseWindow = func() { mu.Lock(); closed++; mu.Unlock() }
	c := n.pase(t)
	v, _ := mcrypto.NewVerifier(12345678, random(16), 1000)
	open := func(verifier []byte) *im.InvokeResult {
		return invoke(t, c, IDAdministratorCommissioning, 0x00, func(w *tlv.Writer) {
			w.PutUint(tlv.Context(0), 180)
			w.PutBytes(tlv.Context(1), verifier)
			w.PutUint(tlv.Context(2), 3840)
			w.PutUint(tlv.Context(3), 1000)
			w.PutBytes(tlv.Context(4), random(16))
		}, true)
	}
	if res := open(v.Bytes()[:10]); res.ClusterStatus == nil || *res.ClusterStatus != adminPAKEParameterError {
		t.Errorf("short verifier: %+v", res)
	}
	if res := open(v.Bytes()); res.Status != im.StatusSuccess {
		t.Fatalf("open: %+v", res)
	}
	if v, _ := im.DecodeValue(readAttr(t, c, IDAdministratorCommissioning, 0).Data); v != uint64(windowEnhanced) {
		t.Errorf("WindowStatus %v", v)
	}
	if res := open(v.Bytes()); res.ClusterStatus == nil || *res.ClusterStatus != adminBusy {
		t.Errorf("second open: %+v", res)
	}
	if res := invoke(t, c, IDAdministratorCommissioning, 0x02, nil, false); res.Status != im.StatusNeedsTimedInteraction {
		t.Errorf("untimed revoke: %#x", res.Status)
	}
	if res := invoke(t, c, IDAdministratorCommissioning, 0x02, nil, true); res.Status != im.StatusSuccess {
		t.Errorf("revoke: %+v", res)
	}
	if res := invoke(t, c, IDAdministratorCommissioning, 0x02, nil, true); res.ClusterStatus == nil || *res.ClusterStatus != adminWindowNotOpen {
		t.Errorf("second revoke: %+v", res)
	}
	waitFor(t, "the hooks", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(opened) == 1 && closed == 1
	})
	if opened[0].Discriminator != 3840 || opened[0].Iterations != 1000 || !bytes.Equal(opened[0].Verifier.Bytes(), v.Bytes()) {
		t.Errorf("opened %+v", opened[0])
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestACLMatching(t *testing.T) {
	cluster, endpoint, light := IDOnOff, uint16(2), DeviceOnOffLight.ID
	e := &im.Endpoint{ID: 2, DeviceTypes: []im.DeviceType{DeviceOnOffLight}}
	cat := uint64(0xFFFF_FFFD_00AB_0002) // CAT 0xAB, version 2
	for name, tc := range map[string]struct {
		ace  fabric.ACE
		subj im.Subject
		priv im.Privilege
		want bool
	}{
		"node":               {fabric.ACE{Privilege: im.PrivilegeOperate, AuthMode: transport.AuthCASE, Subjects: []uint64{7}}, im.Subject{NodeID: 7}, im.PrivilegeOperate, true},
		"other node":         {fabric.ACE{Privilege: im.PrivilegeOperate, AuthMode: transport.AuthCASE, Subjects: []uint64{7}}, im.Subject{NodeID: 8}, im.PrivilegeView, false},
		"privilege too low":  {fabric.ACE{Privilege: im.PrivilegeView, AuthMode: transport.AuthCASE}, im.Subject{NodeID: 7}, im.PrivilegeOperate, false},
		"CAT, newer version": {fabric.ACE{Privilege: im.PrivilegeView, AuthMode: transport.AuthCASE, Subjects: []uint64{cat}}, im.Subject{NodeID: 7, CATs: []uint32{0x00AB_0003}}, im.PrivilegeView, true},
		"CAT, older version": {fabric.ACE{Privilege: im.PrivilegeView, AuthMode: transport.AuthCASE, Subjects: []uint64{cat}}, im.Subject{NodeID: 7, CATs: []uint32{0x00AB_0001}}, im.PrivilegeView, false},
		"cluster target":     {fabric.ACE{Privilege: im.PrivilegeView, AuthMode: transport.AuthCASE, Targets: []fabric.Target{{Cluster: &cluster}}}, im.Subject{NodeID: 7}, im.PrivilegeView, true},
		"endpoint target":    {fabric.ACE{Privilege: im.PrivilegeView, AuthMode: transport.AuthCASE, Targets: []fabric.Target{{Endpoint: &endpoint}}}, im.Subject{NodeID: 7}, im.PrivilegeView, true},
		"device type target": {fabric.ACE{Privilege: im.PrivilegeView, AuthMode: transport.AuthCASE, Targets: []fabric.Target{{DeviceType: &light, Cluster: &cluster}}}, im.Subject{NodeID: 7}, im.PrivilegeView, true},
		"other target":       {fabric.ACE{Privilege: im.PrivilegeView, AuthMode: transport.AuthCASE, Targets: []fabric.Target{{Cluster: new(uint32)}}}, im.Subject{NodeID: 7}, im.PrivilegeView, false},
		"group entry, CASE":  {fabric.ACE{Privilege: im.PrivilegeView, AuthMode: fabric.AuthGroup}, im.Subject{NodeID: 7}, im.PrivilegeView, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := tc.subj
			s.AuthMode = transport.AuthCASE
			got := aclAllows([]fabric.ACE{tc.ace}, &s, e, IDOnOff, tc.priv)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEmptyACLListsGrantAll: an entry's empty subjects or targets are read as null, which grants
// every subject or target, so they mean the same before and after a restart.
func TestEmptyACLListsGrantAll(t *testing.T) {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutUint(tlv.Context(1), uint64(im.PrivilegeOperate))
	w.PutUint(tlv.Context(2), uint64(transport.AuthCASE))
	w.StartArray(tlv.Context(3))
	w.End()
	w.StartArray(tlv.Context(4))
	w.End()
	w.End()
	r := tlv.NewReader(w.Bytes())
	if err := r.Next(); err != nil {
		t.Fatal(err)
	}
	e, st := readACE(r)
	if st != im.StatusSuccess || e.Subjects != nil || e.Targets != nil {
		t.Errorf("readACE: %+v, %#x", e, st)
	}
}

// TestInitialWindow: the node's own window reads as not open in Administrator Commissioning,
// refuses an administrator's second window, and RevokeCommissioning or its timeout closes it.
func TestInitialWindow(t *testing.T) {
	n := newNode(t, t.TempDir())
	var mu sync.Mutex
	var opened []Window
	closed := 0
	n.comm.hooks.OpenWindow = func(w Window) { mu.Lock(); opened = append(opened, w); mu.Unlock() }
	n.comm.hooks.CloseWindow = func() { mu.Lock(); closed++; mu.Unlock() }
	hooks := func(o, c int) func() bool {
		return func() bool { mu.Lock(); defer mu.Unlock(); return len(opened) == o && closed == c }
	}

	n.comm.OpenInitialWindow(time.Minute)
	waitFor(t, "the window to open", hooks(1, 0))
	if !opened[0].Basic || !n.comm.WindowOpen() {
		t.Errorf("opened %+v, open %v", opened[0], n.comm.WindowOpen())
	}
	n.comm.OpenInitialWindow(time.Minute) // already open: nothing happens
	c := n.pase(t)
	if v, _ := im.DecodeValue(readAttr(t, c, IDAdministratorCommissioning, 0).Data); v != uint64(windowClosed) {
		t.Errorf("WindowStatus %v", v)
	}
	if v, _ := im.DecodeValue(readAttr(t, c, IDAdministratorCommissioning, 1).Data); v != nil {
		t.Errorf("AdminFabricIndex %v", v)
	}
	if res := invoke(t, c, IDAdministratorCommissioning, 0x01, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 180) }, true); res.ClusterStatus == nil || *res.ClusterStatus != adminBusy {
		t.Errorf("an administrator's window while the initial one is open: %+v", res)
	}
	if res := invoke(t, c, IDAdministratorCommissioning, 0x02, nil, true); res.Status != im.StatusSuccess {
		t.Errorf("revoke: %+v", res)
	}
	waitFor(t, "the window to close", hooks(1, 1))
	if n.comm.WindowOpen() {
		t.Error("open after revoke")
	}

	n.comm.OpenInitialWindow(50 * time.Millisecond)
	waitFor(t, "the window to time out", hooks(2, 2))
}

// TestUpdateNOC: UpdateNOC asks the node to advertise the new node ID, and so does the fail-safe
// rolling it back.
func TestUpdateNOC(t *testing.T) {
	n := newNode(t, t.TempDir())
	cm := newCommissioner(t)
	c := n.pase(t)
	index := cm.commission(t, c)
	if st := writeACL(t, c); st != im.StatusSuccess {
		t.Fatalf("ACL write: %#x", st)
	}
	admin := n.caseSession(t, cm, commissionerNode)
	if code := armFailSafe(t, admin, 60); code != commissioningOK {
		t.Fatalf("ArmFailSafe: %d", code)
	}
	if code := response(t, invoke(t, admin, IDGeneralCommissioning, 0x04, nil, false), 0x05)[0].(uint64); code != commissioningOK {
		t.Fatalf("CommissioningComplete: %d", code)
	}

	const newNode = 0x0000_0000_0000_0B0C
	if code := armFailSafe(t, admin, 60); code != commissioningOK {
		t.Fatalf("ArmFailSafe: %d", code)
	}
	nonce := random(attestation.NonceSize)
	res := invoke(t, admin, IDOperationalCredentials, 0x04, func(w *tlv.Writer) {
		w.PutBytes(tlv.Context(0), nonce)
		w.PutBool(tlv.Context(1), true)
	}, false)
	elements := response(t, res, 0x05)[0].([]byte)
	decoded, err := im.DecodeValue(elements)
	if err != nil {
		t.Fatal(err)
	}
	csr := decoded.(map[uint8]any)[1].([]byte)
	pub, err := cert.ParseCSR(csr)
	if err != nil {
		t.Fatal(err)
	}
	noc, err := cert.NewNOC(cm.rcac, cm.key, pub, cert.NOCSubject{NodeID: newNode, FabricID: testFabricID}, cm.rcac.NotBefore, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	nocTLV, _ := noc.Encode()
	res = invoke(t, admin, IDOperationalCredentials, 0x07, func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), nocTLV) }, false)
	if f := response(t, res, 0x08); f[0] != uint64(nocOK) {
		t.Fatalf("UpdateNOC: %+v", f)
	}
	waitFor(t, "FabricUpdated after UpdateNOC", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return slices.Equal(n.updated, []uint8{index})
	})
	if f, _ := n.table.Fabric(index); f.NodeID != newNode {
		t.Errorf("pending node ID %#x", f.NodeID)
	}

	// Rolling back restores the old node ID, and says so.
	if code := armFailSafe(t, admin, 0); code != commissioningOK {
		t.Fatalf("ArmFailSafe(0): %d", code)
	}
	waitFor(t, "FabricUpdated after the rollback", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return slices.Equal(n.updated, []uint8{index, index})
	})
	if f, _ := n.table.Fabric(index); f.NodeID != deviceNode {
		t.Errorf("node ID after the rollback %#x", f.NodeID)
	}
}
