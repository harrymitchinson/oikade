package fabric

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/cert"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// ca is a commissioner's root CA.
type ca struct {
	key      *ecdsa.PrivateKey
	rcac     *cert.Certificate
	der      []byte
	fabricID uint64
}

func newCA(t *testing.T, fabricID uint64) *ca {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rcac, err := cert.NewRCAC(key, 1, fabricID, time.Now().Add(-time.Hour).Truncate(time.Second), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	der, err := rcac.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return &ca{key: key, rcac: rcac, der: der, fabricID: fabricID}
}

func (c *ca) noc(t *testing.T, pub *ecdsa.PublicKey, nodeID uint64) []byte {
	t.Helper()
	noc, err := cert.NewNOC(c.rcac, c.key, pub, cert.NOCSubject{NodeID: nodeID, FabricID: c.fabricID}, time.Now().Add(-time.Hour).Truncate(time.Second), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := noc.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var ipk = []byte("0123456789abcdef")

// commission adds and commits a fabric from c.
func commission(t *testing.T, tb *Table, c *ca, nodeID uint64) uint8 {
	t.Helper()
	key, err := tb.NewOperationalKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.AddTrustedRoot(c.der); err != nil {
		t.Fatal(err)
	}
	index, err := tb.AddNOC(c.noc(t, &key.PublicKey, nodeID), nil, ipk, 0x1122, 0xFFF1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Commit(); err != nil {
		t.Fatal(err)
	}
	return index
}

// TestSurvivesRestart commissions two fabrics, changes their ACLs and labels, and reopens the
// table from its directory.
func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	tb, err := Open(DirStore{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	one := commission(t, tb, newCA(t, 1), 0x0A)
	two := commission(t, tb, newCA(t, 2), 0x0B)
	if one != 1 || two != 2 {
		t.Errorf("indexes %d and %d", one, two)
	}
	cluster, endpoint := uint32(6), uint16(2)
	acl := []ACE{
		{Privilege: im.PrivilegeAdminister, AuthMode: transport.AuthCASE, Subjects: []uint64{0x1122}},
		{Privilege: im.PrivilegeOperate, AuthMode: transport.AuthCASE, Targets: []Target{{Cluster: &cluster, Endpoint: &endpoint}}},
	}
	if err := tb.SetACL(two, acl); err != nil {
		t.Fatal(err)
	}
	if err := tb.SetLabel(one, "Home"); err != nil {
		t.Fatal(err)
	}
	if err := tb.SetNextEndpoint(7); err != nil {
		t.Fatal(err)
	}

	again, err := Open(DirStore{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Fabrics(), tb.Fabrics()) {
		t.Errorf("reloaded\n%+v\nwant\n%+v", again.Fabrics(), tb.Fabrics())
	}
	if again.NextEndpoint() != 7 {
		t.Errorf("next endpoint %d", again.NextEndpoint())
	}
	if f, _ := again.Fabric(two); !reflect.DeepEqual(f.ACL, acl) {
		t.Errorf("ACL %+v", f.ACL)
	}
	// A removed fabric's index isn't reused at once.
	if err := again.Remove(one); err != nil {
		t.Fatal(err)
	}
	if three := commission(t, again, newCA(t, 3), 0x0C); three != 3 {
		t.Errorf("index %d after removing 1", three)
	}
	if info, err := os.Stat(filepath.Join(dir, storeFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("stored file: %v, %v", info, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, storeFile+".*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// TestPendingIsNotSaved: what AddNOC adds isn't saved until Commit, and Revert drops it.
func TestPendingIsNotSaved(t *testing.T) {
	dir := t.TempDir()
	tb, _ := Open(DirStore{Dir: dir})
	c := newCA(t, 1)
	key, _ := tb.NewOperationalKey()
	if err := tb.AddTrustedRoot(c.der); err != nil {
		t.Fatal(err)
	}
	index, err := tb.AddNOC(c.noc(t, &key.PublicKey, 0x0A), nil, ipk, 0x1122, 0xFFF1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tb.Fabrics()) != 1 {
		t.Fatalf("pending fabric missing: %+v", tb.Fabrics())
	}
	if err := tb.SetACL(index, nil); err != nil {
		t.Fatal(err)
	}
	if again, _ := Open(DirStore{Dir: dir}); len(again.Fabrics()) != 0 {
		t.Errorf("pending fabric saved")
	}
	if dropped := tb.Revert(); dropped != index || len(tb.Fabrics()) != 0 {
		t.Errorf("Revert: %d, %+v", dropped, tb.Fabrics())
	}
}

func TestAddNOCRejects(t *testing.T) {
	c, other := newCA(t, 1), newCA(t, 1)
	for name, tc := range map[string]struct {
		setup func(tb *Table) []byte // returns the NOC
		want  error
	}{
		"no CSR": {func(tb *Table) []byte {
			_ = tb.AddTrustedRoot(c.der)
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			return c.noc(t, &key.PublicKey, 1)
		}, ErrMissingCSR},
		"no root": {func(tb *Table) []byte {
			key, _ := tb.NewOperationalKey()
			return c.noc(t, &key.PublicKey, 1)
		}, ErrNoRoot},
		"another key": {func(tb *Table) []byte {
			_, _ = tb.NewOperationalKey()
			_ = tb.AddTrustedRoot(c.der)
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			return c.noc(t, &key.PublicKey, 1)
		}, ErrInvalidPublicKey},
		"another root": {func(tb *Table) []byte {
			key, _ := tb.NewOperationalKey()
			_ = tb.AddTrustedRoot(c.der)
			return other.noc(t, &key.PublicKey, 1)
		}, ErrInvalidNOC},
		"same fabric again": {func(tb *Table) []byte {
			commission(t, tb, c, 1)
			key, _ := tb.NewOperationalKey()
			_ = tb.AddTrustedRoot(c.der)
			return c.noc(t, &key.PublicKey, 2)
		}, ErrFabricConflict},
	} {
		t.Run(name, func(t *testing.T) {
			tb, _ := Open(DirStore{Dir: t.TempDir()})
			noc := tc.setup(tb)
			if _, err := tb.AddNOC(noc, nil, ipk, 0x1122, 0xFFF1); !errors.Is(err, tc.want) {
				t.Errorf("AddNOC: %v, want %v", err, tc.want)
			}
		})
	}
}

// TestLoadRejectsCorruption: a table that doesn't decode fails to open rather than starting
// empty, which would lose the fabrics.
func TestLoadRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, storeFile), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(DirStore{Dir: dir}); err == nil {
		t.Error("opened a table of another version")
	}
}

func TestOperationalIPK(t *testing.T) {
	// The spec's example, as connectedhomeip's TestGroup_OperationalKeyDerivation.
	f := Fabric{
		IPK:                []byte{0x23, 0x5b, 0xf7, 0xe6, 0x28, 0x23, 0xd3, 0x58, 0xdc, 0xa4, 0xba, 0x50, 0xb1, 0x53, 0x5f, 0x4b},
		CompressedFabricID: 0x87e1b004e235a130,
	}
	got, err := f.OperationalIPK()
	want := []byte{0xa6, 0xf5, 0x30, 0x6b, 0xaf, 0x6d, 0x05, 0x0a, 0xf2, 0x3b, 0xa4, 0xbd, 0x6b, 0x9d, 0xd9, 0x60}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("OperationalIPK: %x, %v", got, err)
	}
}
