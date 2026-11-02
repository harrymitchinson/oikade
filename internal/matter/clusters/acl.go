package clusters

import (
	"slices"

	"github.com/harrymitchinson/oikade/internal/matter/fabric"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// The Access Control cluster's limits, the spec's minimums.
const (
	subjectsPerEntry = 4
	targetsPerEntry  = 3
	entriesPerFabric = 4
)

func isCAT(s uint64) bool { return s>>32 == 0xFFFF_FFFD }

func (c *Commissioning) accessControl() *im.Cluster {
	admin := im.PrivilegeAdminister
	return &im.Cluster{
		ID: IDAccessControl, Revision: 1,
		Attributes: []im.Attribute{
			{
				ID: 0x0000, Read: c.readACL, Write: c.writeACL,
				ReadPrivilege: admin, WritePrivilege: admin, FabricScoped: true,
			},
			uintAttr(0x0002, subjectsPerEntry),
			uintAttr(0x0003, targetsPerEntry),
			uintAttr(0x0004, entriesPerFabric),
		},
	}
}

func (c *Commissioning) readACL(req *im.Request, w *tlv.Writer, tag tlv.Tag) error {
	w.StartArray(tag)
	for _, f := range c.fabrics.Fabrics() {
		if req.FabricFiltered && f.Index != req.FabricIndex {
			continue
		}
		for _, e := range f.ACL {
			w.StartStructure(tlv.Anonymous)
			if f.Index == req.FabricIndex { // every field but the fabric index is fabric-sensitive
				putACE(w, e)
			}
			w.PutUint(tagFabricIndex, uint64(f.Index))
			w.End()
		}
	}
	w.End()
	return nil
}

func putACE(w *tlv.Writer, e fabric.ACE) {
	w.PutUint(tlv.Context(1), uint64(e.Privilege))
	w.PutUint(tlv.Context(2), uint64(e.AuthMode))
	if e.Subjects == nil {
		w.PutNull(tlv.Context(3))
	} else {
		w.StartArray(tlv.Context(3))
		for _, s := range e.Subjects {
			w.PutUint(tlv.Anonymous, s)
		}
		w.End()
	}
	if e.Targets == nil {
		w.PutNull(tlv.Context(4))
		return
	}
	w.StartArray(tlv.Context(4))
	for _, t := range e.Targets {
		w.StartStructure(tlv.Anonymous)
		putNullable(w, tlv.Context(0), t.Cluster)
		putNullable(w, tlv.Context(1), t.Endpoint)
		putNullable(w, tlv.Context(2), t.DeviceType)
		w.End()
	}
	w.End()
}

func putNullable[T uint16 | uint32](w *tlv.Writer, tag tlv.Tag, v *T) {
	if v == nil {
		w.PutNull(tag)
	} else {
		w.PutUint(tag, uint64(*v))
	}
}

// writeACL replaces the accessing fabric's entries, or appends one. Entries of other fabrics are
// untouched, and the fabric index a client writes is ignored.
func (c *Commissioning) writeACL(req *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
	var acl []fabric.ACE
	if op == im.Append {
		e, st := readACE(r)
		if st != im.StatusSuccess {
			return st
		}
		f, ok := c.fabrics.Fabric(req.FabricIndex)
		if !ok {
			return im.StatusUnsupportedAccess
		}
		acl = append(f.ACL, e)
	} else {
		acl = []fabric.ACE{}
		err := im.Items(r, func(r *tlv.Reader) error {
			e, st := readACE(r)
			if st != im.StatusSuccess {
				return st
			}
			acl = append(acl, e)
			return nil
		})
		if st, ok := err.(im.Status); ok {
			return st
		} else if err != nil {
			return im.StatusInvalidDataType
		}
	}
	if len(acl) > entriesPerFabric {
		return im.StatusResourceExhausted
	}
	if err := c.fabrics.SetACL(req.FabricIndex, acl); err != nil {
		if err == fabric.ErrInvalidIndex {
			return im.StatusUnsupportedAccess
		}
		c.log.Error(err, "Could not save the ACL")
		return im.StatusFailure
	}
	c.acl.Changed(0x0000)
	return im.StatusSuccess
}

// readACE reads and checks an AccessControlEntryStruct.
func readACE(r *tlv.Reader) (fabric.ACE, im.Status) {
	var e fabric.ACE
	var seen [5]bool
	var bad im.Status
	err := im.Fields(r, func(r *tlv.Reader, tag uint8) error {
		switch tag {
		case 1:
			v, err := r.Uint()
			if err != nil {
				return err
			}
			if v < uint64(im.PrivilegeView) || v > uint64(im.PrivilegeAdminister) {
				bad = im.StatusConstraintError
			}
			e.Privilege = im.Privilege(v)
		case 2:
			v, err := r.Uint()
			if err != nil {
				return err
			}
			if v != uint64(transport.AuthCASE) && v != uint64(fabric.AuthGroup) {
				bad = im.StatusConstraintError
			}
			e.AuthMode = transport.AuthMode(v)
		case 3:
			if r.IsNull() {
				break
			}
			e.Subjects = []uint64{}
			if err := im.Items(r, func(r *tlv.Reader) error {
				v, err := r.Uint()
				e.Subjects = append(e.Subjects, v)
				return err
			}); err != nil {
				return err
			}
		case 4:
			if r.IsNull() {
				break
			}
			e.Targets = []fabric.Target{}
			if err := im.Items(r, func(r *tlv.Reader) error {
				t, err := readTarget(r)
				e.Targets = append(e.Targets, t)
				return err
			}); err != nil {
				return err
			}
		default:
			return nil
		}
		seen[tag] = true
		return nil
	})
	// An empty list of subjects or targets grants all, as null does.
	if len(e.Subjects) == 0 {
		e.Subjects = nil
	}
	if len(e.Targets) == 0 {
		e.Targets = nil
	}
	switch {
	case err != nil || !seen[1] || !seen[2] || !seen[3] || !seen[4]:
		return e, im.StatusInvalidDataType
	case bad != im.StatusSuccess:
		return e, bad
	case !validACE(e):
		return e, im.StatusConstraintError
	}
	return e, im.StatusSuccess
}

func readTarget(r *tlv.Reader) (fabric.Target, error) {
	var t fabric.Target
	err := im.Fields(r, func(r *tlv.Reader, tag uint8) error {
		if r.IsNull() {
			return nil
		}
		v, err := r.Uint()
		if err != nil {
			return err
		}
		switch tag {
		case 0:
			x := uint32(v)
			t.Cluster = &x
		case 1:
			x := uint16(v)
			t.Endpoint = &x
		case 2:
			x := uint32(v)
			t.DeviceType = &x
		}
		return nil
	})
	return t, err
}

// validACE checks an entry's constraints (Access Control cluster, 9.10.5.6).
func validACE(e fabric.ACE) bool {
	if e.AuthMode == fabric.AuthGroup && e.Privilege == im.PrivilegeAdminister {
		return false
	}
	if len(e.Subjects) > subjectsPerEntry || len(e.Targets) > targetsPerEntry {
		return false
	}
	for _, s := range e.Subjects {
		ok := validAdminSubject(s)
		if e.AuthMode == fabric.AuthGroup {
			ok = s != 0 && s <= 0xFFFF
		}
		if !ok {
			return false
		}
	}
	for _, t := range e.Targets {
		if (t.Cluster == nil && t.Endpoint == nil && t.DeviceType == nil) || (t.Endpoint != nil && t.DeviceType != nil) {
			return false
		}
	}
	return true
}

// Allowed reports whether s holds priv on cluster of endpoint e, from the entries of its fabric
// (Core spec, 6.6.6). It implements im.AccessControl.
func (c *Commissioning) Allowed(s *im.Subject, e *im.Endpoint, cluster uint32, priv im.Privilege) bool {
	if s.FabricIndex == 0 {
		return false
	}
	acl, _ := c.fabrics.ACL(s.FabricIndex)
	return aclAllows(acl, s, e, cluster, priv)
}

// aclAllows reports whether an entry of acl grants s priv on cluster of endpoint e.
func aclAllows(acl []fabric.ACE, s *im.Subject, e *im.Endpoint, cluster uint32, priv im.Privilege) bool {
	for _, ace := range acl {
		if ace.AuthMode != s.AuthMode || !ace.Privilege.Grants(priv) {
			continue
		}
		if ace.Subjects != nil && !slices.ContainsFunc(ace.Subjects, func(x uint64) bool { return subjectMatches(x, s) }) {
			continue
		}
		if ace.Targets != nil && !slices.ContainsFunc(ace.Targets, func(t fabric.Target) bool { return targetMatches(t, e, cluster) }) {
			continue
		}
		return true
	}
	return false
}

// subjectMatches reports whether an entry's subject is s's node ID, or a CAT that s holds at the
// entry's version or later.
func subjectMatches(x uint64, s *im.Subject) bool {
	if !isCAT(x) {
		return x == s.NodeID
	}
	for _, cat := range s.CATs {
		if uint64(cat>>16) == (x>>16)&0xFFFF && cat&0xFFFF >= uint32(x&0xFFFF) {
			return true
		}
	}
	return false
}

func targetMatches(t fabric.Target, e *im.Endpoint, cluster uint32) bool {
	if t.Cluster != nil && *t.Cluster != cluster {
		return false
	}
	if t.Endpoint != nil && *t.Endpoint != e.ID {
		return false
	}
	if t.DeviceType != nil && !slices.ContainsFunc(e.DeviceTypes, func(d im.DeviceType) bool { return d.ID == *t.DeviceType }) {
		return false
	}
	return true
}
