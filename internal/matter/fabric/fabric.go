// Package fabric is a Matter node's fabric table (Matter Core Specification, 11.18): the
// fabrics that commissioned it, with their root certificates, NOCs, operational keys, IPKs and
// access control entries, and the next endpoint the node will allocate.
//
// The table is state whose loss means re-commissioning the node, so it persists through a Store
// after every change. Changes made while a fail-safe is armed (Core spec, 11.10.7.2) stay pending
// until Commit, and Revert drops them.
package fabric

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/cert"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// MaxFabrics is how many fabrics the table holds, the spec's minimum (SupportedFabrics).
const MaxFabrics = 5

// IPKSize is the size of an epoch key, such as the IPK.
const IPKSize = 16

// Errors from adding and updating NOCs, each matching a NodeOperationalCertStatusEnum value.
var (
	ErrInvalidPublicKey = errors.New("matter: NOC's public key isn't the operational key")
	ErrInvalidNodeID    = errors.New("matter: NOC has an invalid node ID")
	ErrInvalidNOC       = errors.New("matter: invalid NOC chain")
	ErrMissingCSR       = errors.New("matter: no operational key: CSRRequest wasn't invoked")
	ErrTableFull        = errors.New("matter: fabric table is full")
	ErrFabricConflict   = errors.New("matter: a fabric with the same root and fabric ID exists")
	ErrLabelConflict    = errors.New("matter: another fabric has that label")
	ErrInvalidIndex     = errors.New("matter: no such fabric")
	ErrNoRoot           = errors.New("matter: no trusted root: AddTrustedRootCertificate wasn't invoked")
	ErrPending          = errors.New("matter: the fail-safe already has a change of that kind pending")
)

// Fabric is one fabric the node is on.
type Fabric struct {
	Index uint8
	// RCAC, ICAC and NOC are Matter TLV certificates. ICAC is nil without one.
	RCAC, ICAC, NOC []byte
	// Key is the node's operational key on the fabric.
	Key *ecdsa.PrivateKey
	// IPK is the fabric's Identity Protection Key epoch key (Core spec, 4.17.2).
	IPK      []byte
	VendorID uint16 // the admin's vendor ID
	Label    string
	ACL      []ACE

	// From the certificates.
	FabricID, NodeID   uint64
	RootPublicKey      []byte // uncompressed P-256
	CompressedFabricID uint64
}

// ACE is an access control entry (Access Control cluster, AccessControlEntryStruct).
type ACE struct {
	Privilege im.Privilege
	AuthMode  transport.AuthMode // CASE, or 3 for groups
	Subjects  []uint64           // nil grants every subject of the auth mode
	Targets   []Target           // nil grants the whole node
}

// AuthGroup is the Access Control cluster's group auth mode, which this node doesn't serve yet.
const AuthGroup transport.AuthMode = 3

// Target is part of a node an ACE grants: a cluster, endpoint or device type, or a cluster on
// an endpoint or of a device type. nil fields match anything.
type Target struct {
	Cluster    *uint32
	Endpoint   *uint16
	DeviceType *uint32
}

// OperationalIPK is the fabric's operational group key derived from its IPK, which CASE uses
// (Core spec, 4.17.2).
func (f *Fabric) OperationalIPK() ([]byte, error) {
	salt := binary.BigEndian.AppendUint64(nil, f.CompressedFabricID)
	return hkdf.Key(sha256.New, f.IPK, salt, "GroupKey v1.0", IPKSize)
}

func (f *Fabric) clone() Fabric {
	c := *f
	c.ACL = cloneACL(f.ACL)
	return c
}

func cloneACL(acl []ACE) []ACE {
	out := slices.Clone(acl)
	for i := range out {
		out[i].Subjects = slices.Clone(out[i].Subjects)
		out[i].Targets = slices.Clone(out[i].Targets)
	}
	return out
}

// derive fills the fields that come from the certificates, checking the chain at now (zero skips
// the validity periods, for a node without a trusted clock).
func (f *Fabric) derive(now time.Time) error {
	rcac, err := cert.Decode(f.RCAC)
	if err != nil {
		return fmt.Errorf("%w: root: %w", ErrInvalidNOC, err)
	}
	var icac *cert.Certificate
	if f.ICAC != nil {
		if icac, err = cert.Decode(f.ICAC); err != nil {
			return fmt.Errorf("%w: ICAC: %w", ErrInvalidNOC, err)
		}
	}
	noc, err := cert.Decode(f.NOC)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidNOC, err)
	}
	if err := cert.Verify(rcac, icac, noc, now); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidNOC, err)
	}
	nodeID, err := noc.NodeID()
	if err != nil || !cert.IsOperationalNodeID(nodeID) {
		return ErrInvalidNodeID
	}
	fabricID, ok := noc.FabricID()
	if !ok || fabricID == 0 {
		return fmt.Errorf("%w: no fabric ID", ErrInvalidNOC)
	}
	pub, err := noc.ECDSAPublicKey()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidNOC, err)
	}
	if f.Key == nil || !pub.Equal(&f.Key.PublicKey) {
		return ErrInvalidPublicKey
	}
	cfid, err := cert.CompressedFabricID(rcac.PublicKey, fabricID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidNOC, err)
	}
	f.NodeID, f.FabricID, f.RootPublicKey, f.CompressedFabricID = nodeID, fabricID, rcac.PublicKey, cfid
	return nil
}

// Store keeps the table's encoded state.
type Store interface {
	// Load returns what Save last saved, or nil if nothing was.
	Load() ([]byte, error)
	// Save replaces the saved state atomically: after a crash, Load returns either the old
	// state or the new one.
	Save(b []byte) error
}

// Table is a node's fabric table. It's safe for concurrent use.
type Table struct {
	store Store
	// Now is the time NOC chains are checked at. nil, or a zero time, skips the validity periods.
	Now func() time.Time

	mu           sync.Mutex
	fabrics      []Fabric // committed, in index order
	nextIndex    uint8
	nextEndpoint uint16

	// Pending changes, while a fail-safe is armed.
	key     *ecdsa.PrivateKey // from NewOperationalKey
	root    []byte            // from AddTrustedRoot
	added   *Fabric           // from AddNOC
	updated *Fabric           // from UpdateNOC: the fabric as it will be

	caseCache map[uint8]*securechannel.Fabric // what CASE() last returned
}

// Open loads a table from store.
func Open(store Store) (*Table, error) {
	t := &Table{store: store, nextIndex: 1}
	b, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("matter: loading the fabric table: %w", err)
	}
	if b != nil {
		if err := t.decode(b); err != nil {
			return nil, fmt.Errorf("matter: loading the fabric table: %w", err)
		}
	}
	return t, nil
}

func (t *Table) now() time.Time {
	if t.Now == nil {
		return time.Time{}
	}
	return t.Now()
}

// save persists the committed state. Call it with the lock held.
func (t *Table) save() error {
	b, err := t.encode()
	if err != nil {
		return err
	}
	if err := t.store.Save(b); err != nil {
		return fmt.Errorf("matter: saving the fabric table: %w", err)
	}
	return nil
}

// Fabrics returns the fabrics, a pending one included and with a pending update applied, in index
// order.
func (t *Table) Fabrics() []Fabric {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur := t.current()
	out := make([]Fabric, 0, len(cur))
	for _, f := range cur {
		out = append(out, f.clone())
	}
	return out
}

// Fabric returns the fabric with index, as Fabrics would.
func (t *Table) Fabric(index uint8) (Fabric, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f := t.find(index); f != nil {
		return f.clone(), true
	}
	return Fabric{}, false
}

// Count returns how many fabrics Fabrics would return.
func (t *Table) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.current())
}

// current returns the fabrics as they stand, pending changes applied, without copying them. Call
// it with the lock held.
func (t *Table) current() []*Fabric {
	out := make([]*Fabric, 0, len(t.fabrics)+1)
	for i := range t.fabrics {
		out = append(out, t.find(t.fabrics[i].Index))
	}
	if t.added != nil {
		out = append(out, t.added)
	}
	return out
}

// find returns the fabric with index as it stands, pending changes applied. Call it with the lock
// held.
func (t *Table) find(index uint8) *Fabric {
	if t.added != nil && t.added.Index == index {
		return t.added
	}
	if t.updated != nil && t.updated.Index == index {
		return t.updated
	}
	for i := range t.fabrics {
		if t.fabrics[i].Index == index {
			return &t.fabrics[i]
		}
	}
	return nil
}

// NewOperationalKey makes the operational key that the next AddNOC or UpdateNOC certifies, for
// CSRRequest. A second call replaces the first's key.
func (t *Table) NewOperationalKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.key = key
	return key, nil
}

// AddTrustedRoot records the root certificate, Matter TLV, that the next AddNOC chains to.
func (t *Table) AddTrustedRoot(rcac []byte) error {
	c, err := cert.Decode(rcac)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidNOC, err)
	}
	if k, err := c.Kind(); err != nil || k != cert.KindRoot {
		return fmt.Errorf("%w: not a root certificate", ErrInvalidNOC)
	}
	if err := c.CheckSignatureFrom(c); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidNOC, err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.root != nil || t.added != nil {
		return ErrPending
	}
	t.root = slices.Clone(rcac)
	return nil
}

// AddNOC adds a fabric, pending until Commit, from a NOC and optional ICAC chained to the root
// from AddTrustedRoot and certifying the key from NewOperationalKey. Its ACL grants
// adminSubject Administer over CASE. It returns the new fabric's index.
func (t *Table) AddNOC(noc, icac, ipk []byte, adminSubject uint64, adminVendor uint16) (uint8, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.added != nil || t.updated != nil:
		return 0, ErrPending
	case t.key == nil:
		return 0, ErrMissingCSR
	case t.root == nil:
		return 0, ErrNoRoot
	case len(ipk) != IPKSize:
		return 0, fmt.Errorf("%w: IPK of %d bytes", ErrInvalidNOC, len(ipk))
	case len(t.fabrics) >= MaxFabrics:
		return 0, ErrTableFull
	}
	f := Fabric{
		RCAC: t.root, ICAC: slices.Clone(icac), NOC: slices.Clone(noc), Key: t.key,
		IPK: slices.Clone(ipk), VendorID: adminVendor,
		ACL: []ACE{{Privilege: im.PrivilegeAdminister, AuthMode: transport.AuthCASE, Subjects: []uint64{adminSubject}}},
	}
	if err := f.derive(t.now()); err != nil {
		return 0, err
	}
	for _, o := range t.fabrics {
		if o.FabricID == f.FabricID && bytes.Equal(o.RootPublicKey, f.RootPublicKey) {
			return 0, ErrFabricConflict
		}
	}
	f.Index = t.allocateIndex()
	t.added, t.key = &f, nil
	return f.Index, nil
}

// allocateIndex returns the next free fabric index. Call it with the lock held, and fewer than
// 254 fabrics.
func (t *Table) allocateIndex() uint8 {
	for {
		i := t.nextIndex
		t.nextIndex++
		if t.nextIndex == 0 || t.nextIndex == 255 {
			t.nextIndex = 1
		}
		if t.find(i) == nil {
			return i
		}
	}
}

// UpdateNOC replaces fabric index's NOC and ICAC, pending until Commit, with ones that chain to
// its root and certify the key from NewOperationalKey.
func (t *Table) UpdateNOC(index uint8, noc, icac []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.added != nil || t.updated != nil {
		return ErrPending
	}
	if t.key == nil {
		return ErrMissingCSR
	}
	old := t.find(index)
	if old == nil {
		return ErrInvalidIndex
	}
	f := old.clone()
	f.NOC, f.ICAC, f.Key = slices.Clone(noc), slices.Clone(icac), t.key
	if err := f.derive(t.now()); err != nil {
		return err
	}
	if f.FabricID != old.FabricID {
		return fmt.Errorf("%w: fabric ID changed", ErrInvalidNOC)
	}
	t.updated, t.key = &f, nil
	return nil
}

// SetLabel sets a fabric's label, which no other fabric may have.
func (t *Table) SetLabel(index uint8, label string) error {
	return t.change(index, func(f *Fabric) error {
		for _, o := range t.current() {
			if o.Index != index && label != "" && o.Label == label {
				return ErrLabelConflict
			}
		}
		f.Label = label
		return nil
	})
}

// SetACL replaces a fabric's access control entries.
func (t *Table) SetACL(index uint8, acl []ACE) error {
	return t.change(index, func(f *Fabric) error {
		f.ACL = cloneACL(acl)
		return nil
	})
}

// change applies f to fabric index, saving it unless the fabric is pending. A committed fabric
// with a pending update gets the change both now and in the update.
func (t *Table) change(index uint8, apply func(*Fabric) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.added != nil && t.added.Index == index {
		return apply(t.added)
	}
	i := slices.IndexFunc(t.fabrics, func(f Fabric) bool { return f.Index == index })
	if i < 0 {
		return ErrInvalidIndex
	}
	f := t.fabrics[i].clone()
	if err := apply(&f); err != nil {
		return err
	}
	old := t.fabrics[i]
	t.fabrics[i] = f
	if err := t.save(); err != nil {
		t.fabrics[i] = old
		return err
	}
	if t.updated != nil && t.updated.Index == index {
		_ = apply(t.updated) // it succeeded on the committed fabric
	}
	return nil
}

// Remove removes a fabric, and saves the table. A pending fabric is dropped.
func (t *Table) Remove(index uint8) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.added != nil && t.added.Index == index {
		t.added = nil
		return nil
	}
	i := slices.IndexFunc(t.fabrics, func(f Fabric) bool { return f.Index == index })
	if i < 0 {
		return ErrInvalidIndex
	}
	old := t.fabrics
	t.fabrics = slices.Delete(slices.Clone(t.fabrics), i, i+1)
	if err := t.save(); err != nil {
		t.fabrics = old
		return err
	}
	if t.updated != nil && t.updated.Index == index {
		t.updated = nil
	}
	return nil
}

// Pending reports whether AddNOC or UpdateNOC has a change waiting for Commit, and the fabric it
// changes.
func (t *Table) Pending() (index uint8, added, updated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.added != nil:
		return t.added.Index, true, false
	case t.updated != nil:
		return t.updated.Index, false, true
	}
	return 0, false, false
}

// HasRoot reports whether AddTrustedRoot has a root pending.
func (t *Table) HasRoot() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.root != nil
}

// Commit saves the pending fabric or NOC update, and clears every pending change.
func (t *Table) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	old := t.fabrics
	next := slices.Clone(t.fabrics)
	if t.added != nil {
		next = append(next, *t.added)
	}
	if t.updated != nil {
		for i := range next {
			if next[i].Index == t.updated.Index {
				next[i] = *t.updated
			}
		}
	}
	slices.SortFunc(next, func(a, b Fabric) int { return int(a.Index) - int(b.Index) })
	t.fabrics = next
	if err := t.save(); err != nil {
		t.fabrics = old
		return err
	}
	t.key, t.root, t.added, t.updated = nil, nil, nil, nil
	return nil
}

// Revert drops every pending change, as an expired fail-safe does. It returns the index of a
// pending fabric it dropped, or 0.
func (t *Table) Revert() uint8 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var dropped uint8
	if t.added != nil {
		dropped = t.added.Index
	}
	t.key, t.root, t.added, t.updated = nil, nil, nil, nil
	return dropped
}

// NextEndpoint returns the endpoint ID the node allocates next.
func (t *Table) NextEndpoint() uint16 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nextEndpoint
}

// SetNextEndpoint records the endpoint ID the node allocates next, and saves it.
func (t *Table) SetNextEndpoint(id uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	old := t.nextEndpoint
	t.nextEndpoint = id
	if err := t.save(); err != nil {
		t.nextEndpoint = old
		return err
	}
	return nil
}

// ACL returns a fabric's access control entries, a pending change applied, without copying
// them: the caller must not modify them.
func (t *Table) ACL(index uint8) ([]ACE, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.find(index)
	if f == nil {
		return nil, false
	}
	return f.ACL, true
}
