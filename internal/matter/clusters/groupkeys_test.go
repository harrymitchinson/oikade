package clusters

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/matter/fabric"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// commissioned returns a node commissioned to one fabric, with its index, and its admin over CASE.
func commissioned(t *testing.T) (*node, uint8, *im.Client) {
	t.Helper()
	n := newNode(t, t.TempDir())
	cm := newCommissioner(t)
	c := n.pase(t)
	index := cm.commission(t, c)
	if st := writeACL(t, c); st != im.StatusSuccess {
		t.Fatalf("ACL write: %#x", st)
	}
	admin := n.caseSession(t, cm, commissionerNode)
	if code := response(t, invoke(t, admin, IDGeneralCommissioning, 0x04, nil, false), 0x05)[0].(uint64); code != commissioningOK {
		t.Fatalf("CommissioningComplete: %d", code)
	}
	return n, index, admin
}

// withKeySetID writes a command's fields with a GroupKeySetID.
func withKeySetID(id uint64) func(w *tlv.Writer) {
	return func(w *tlv.Writer) { w.PutUint(tlv.Context(0), id) }
}

// commandStatus is a command's status, with no response.
func commandStatus(t *testing.T, res *im.InvokeResult) im.Status {
	t.Helper()
	if res.Response != nil {
		t.Fatalf("got response %+v, want a status", res.Response)
	}
	return res.Status
}

// A controller pairing reads the fabric's IPK keyset, 0, and may not change or remove it. The node
// serves no groups, so it takes no other keyset.
func TestGroupKeySets(t *testing.T) {
	_, _, admin := commissioned(t)
	invokeKeys := func(command uint32, fields func(w *tlv.Writer)) *im.InvokeResult {
		return invoke(t, admin, IDGroupKeyManagement, command, fields, false)
	}

	set, ok := response(t, invokeKeys(0x01, withKeySetID(0)), 0x02)[0].(map[uint8]any)
	if !ok || set[0] != uint64(0) || set[1] != uint64(0) || set[3] != uint64(0) {
		t.Errorf("KeySetRead(0): %+v, want keyset 0, TrustFirst and epoch start 0", set)
	}
	// The keys are null, never sent, but present: the struct has them all.
	for _, tag := range []uint8{2, 4, 5, 6, 7} {
		if v, present := set[tag]; !present || v != nil {
			t.Errorf("KeySetRead(0): field %d is %v (present %v), want null", tag, v, present)
		}
	}
	if st := commandStatus(t, invokeKeys(0x01, withKeySetID(1))); st != im.StatusNotFound {
		t.Errorf("KeySetRead(1): %#x, want NOT_FOUND", st)
	}
	if st := commandStatus(t, invokeKeys(0x01, nil)); st != im.StatusInvalidCommand {
		t.Errorf("KeySetRead with no ID: %#x, want INVALID_COMMAND", st)
	}

	if ids := response(t, invokeKeys(0x04, nil), 0x05)[0].([]any); len(ids) != 1 || ids[0] != uint64(0) {
		t.Errorf("KeySetReadAllIndices: %v, want [0]", ids)
	}

	keySet := func(id uint64) func(w *tlv.Writer) {
		return func(w *tlv.Writer) {
			w.StartStructure(tlv.Context(0))
			w.PutUint(tlv.Context(0), id)
			w.End()
		}
	}
	for name, tt := range map[string]struct {
		command uint32
		fields  func(w *tlv.Writer)
		want    im.Status
	}{
		"KeySetWrite of the IPK":  {0x00, keySet(0), im.StatusInvalidCommand},
		"KeySetWrite of another":  {0x00, keySet(2), im.StatusResourceExhausted},
		"KeySetWrite with no set": {0x00, nil, im.StatusInvalidCommand},
		"KeySetRemove of the IPK": {0x03, withKeySetID(0), im.StatusInvalidCommand},
		"KeySetRemove of another": {0x03, withKeySetID(2), im.StatusNotFound},
		"KeySetRemove with no ID": {0x03, nil, im.StatusInvalidCommand},
	} {
		if st := commandStatus(t, invokeKeys(tt.command, tt.fields)); st != tt.want {
			t.Errorf("%s: %#x, want %#x", name, st, tt.want)
		}
	}
}

// The admin reads its own NOC, which pairing checks, and labels its fabric.
func TestNOCsAndFabricLabels(t *testing.T) {
	n, index, admin := commissioned(t)

	nocs, err := im.DecodeValue(readAttr(t, admin, IDOperationalCredentials, 0x0000).Data)
	if err != nil {
		t.Fatal(err)
	}
	list := nocs.([]any)
	if len(list) != 1 {
		t.Fatalf("NOCs %+v", list)
	}
	f, ok := n.table.Fabric(index)
	if !ok {
		t.Fatalf("no fabric %d", index)
	}
	got, _ := list[0].(map[uint8]any)
	if noc, _ := got[1].([]byte); !bytes.Equal(noc, f.NOC) || got[2] != nil || got[0xFE] != uint64(index) {
		t.Errorf("NOCStruct %+v, want the fabric's NOC, no ICAC and index %d", got, index)
	}

	label := func(l string) *im.InvokeResult {
		return invoke(t, admin, IDOperationalCredentials, 0x09, func(w *tlv.Writer) { w.PutUTF8(tlv.Context(0), l) }, false)
	}
	if r := response(t, label("Kitchen"), 0x08); r[0] != uint64(nocOK) || r[1] != uint64(index) {
		t.Fatalf("UpdateFabricLabel: %+v", r)
	}
	if f, _ := n.table.Fabric(index); f.Label != "Kitchen" {
		t.Errorf("label %q, want Kitchen", f.Label)
	}
	fabrics, err := im.DecodeValue(readAttr(t, admin, IDOperationalCredentials, 0x0001).Data)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := fabrics.([]any); len(list) != 1 {
		t.Errorf("Fabrics %+v, want one", fabrics)
	} else if fd, _ := list[0].(map[uint8]any); fd[5] != "Kitchen" {
		t.Errorf("Fabrics says label %v, want Kitchen", fd[5])
	}
	if st := commandStatus(t, label(strings.Repeat("x", maxLabelLength+1))); st != im.StatusConstraintError {
		t.Errorf("a label too long: %#x, want CONSTRAINT_ERROR", st)
	}
	if st := commandStatus(t, invoke(t, admin, IDOperationalCredentials, 0x09, nil, false)); st != im.StatusInvalidCommand {
		t.Errorf("no label: %#x, want INVALID_COMMAND", st)
	}
}

// Each of the fabric table's refusals has its own NOCResponse status, so a controller can say why.
func TestNOCStatuses(t *testing.T) {
	for err, want := range map[error]uint8{
		fabric.ErrInvalidPublicKey:  nocInvalidPublicKey,
		fabric.ErrInvalidNodeID:     nocInvalidNodeOpID,
		fabric.ErrMissingCSR:        nocMissingCSR,
		fabric.ErrTableFull:         nocTableFull,
		fabric.ErrFabricConflict:    nocFabricConflict,
		fabric.ErrLabelConflict:     nocLabelConflict,
		fabric.ErrInvalidIndex:      nocInvalidFabricIndex,
		errors.New("anything else"): nocInvalidNOC,
	} {
		if got := nocStatus(fmt.Errorf("wrapped: %w", err)); got != want {
			t.Errorf("%v: %d, want %d", err, got, want)
		}
	}
}
