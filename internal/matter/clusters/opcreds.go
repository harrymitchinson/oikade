package clusters

import (
	"errors"

	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/cert"
	"github.com/harrymitchinson/oikade/internal/matter/fabric"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// NodeOperationalCertStatusEnum.
const (
	nocOK                  = 0
	nocInvalidPublicKey    = 1
	nocInvalidNodeOpID     = 2
	nocInvalidNOC          = 3
	nocMissingCSR          = 4
	nocTableFull           = 5
	nocInvalidAdminSubject = 6
	nocFabricConflict      = 9
	nocLabelConflict       = 10
	nocInvalidFabricIndex  = 11
)

// tagFabricIndex is the field a fabric-scoped struct carries its fabric index in.
var tagFabricIndex = tlv.Context(0xFE)

const maxLabelLength = 32

func nocStatus(err error) uint8 {
	switch {
	case errors.Is(err, fabric.ErrInvalidPublicKey):
		return nocInvalidPublicKey
	case errors.Is(err, fabric.ErrInvalidNodeID):
		return nocInvalidNodeOpID
	case errors.Is(err, fabric.ErrMissingCSR):
		return nocMissingCSR
	case errors.Is(err, fabric.ErrTableFull):
		return nocTableFull
	case errors.Is(err, fabric.ErrFabricConflict):
		return nocFabricConflict
	case errors.Is(err, fabric.ErrLabelConflict):
		return nocLabelConflict
	case errors.Is(err, fabric.ErrInvalidIndex):
		return nocInvalidFabricIndex
	}
	return nocInvalidNOC
}

// nocResponse is NOCResponse, with the fabric index when there is one.
func nocResponse(status uint8, index uint8) *im.Response {
	return &im.Response{Command: 0x08, Fields: func(w *tlv.Writer) {
		w.PutUint(tlv.Context(0), uint64(status))
		if index != 0 {
			w.PutUint(tlv.Context(1), uint64(index))
		}
	}}
}

func (c *Commissioning) operationalCredentials() *im.Cluster {
	admin := im.PrivilegeAdminister
	return &im.Cluster{
		ID: IDOperationalCredentials, Revision: 1,
		Attributes: []im.Attribute{
			{ID: 0x0000, Read: c.readNOCs, ReadPrivilege: admin, FabricScoped: true},
			{ID: 0x0001, Read: c.readFabrics, FabricScoped: true},
			uintAttr(0x0002, fabric.MaxFabrics), // SupportedFabrics
			{ID: 0x0003, Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { // CommissionedFabrics
				w.PutUint(tag, uint64(c.fabrics.Count()))
				return nil
			}},
			{ID: 0x0004, Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { // TrustedRootCertificates
				w.StartArray(tag)
				for _, f := range c.fabrics.Fabrics() {
					w.PutBytes(tlv.Anonymous, f.RCAC)
				}
				w.End()
				return nil
			}},
			{ID: 0x0005, Read: func(req *im.Request, w *tlv.Writer, tag tlv.Tag) error { // CurrentFabricIndex
				w.PutUint(tag, uint64(req.FabricIndex))
				return nil
			}},
		},
		Commands: []im.Command{
			{ID: 0x00, Invoke: c.attestationRequest, Privilege: admin},
			{ID: 0x02, Invoke: c.certificateChainRequest, Privilege: admin},
			{ID: 0x04, Invoke: c.csrRequest, Privilege: admin},
			{ID: 0x06, Invoke: c.addNOC, Privilege: admin},
			{ID: 0x07, Invoke: c.updateNOC, Privilege: admin},
			{ID: 0x09, Invoke: c.updateFabricLabel, Privilege: admin},
			{ID: 0x0A, Invoke: c.removeFabric, Privilege: admin},
			{ID: 0x0B, Invoke: c.addTrustedRoot, Privilege: admin},
		},
		Generated: []uint32{0x01, 0x03, 0x05, 0x08},
	}
}

// fabricsChanged tells subscribers that the fabric attributes changed. Call it with the Node's
// lock.
func (c *Commissioning) fabricsChanged() {
	for _, id := range []uint32{0, 1, 3, 4} {
		c.opCreds.Changed(id)
	}
	c.acl.Changed(0)
	c.groupKeys.Changed(0)
}

func (c *Commissioning) readNOCs(req *im.Request, w *tlv.Writer, tag tlv.Tag) error {
	w.StartArray(tag)
	for _, f := range c.fabrics.Fabrics() {
		if req.FabricFiltered && f.Index != req.FabricIndex {
			continue
		}
		w.StartStructure(tlv.Anonymous)
		if f.Index == req.FabricIndex { // NOCStruct's certificates are fabric-sensitive
			w.PutBytes(tlv.Context(1), f.NOC)
			if f.ICAC != nil {
				w.PutBytes(tlv.Context(2), f.ICAC)
			} else {
				w.PutNull(tlv.Context(2))
			}
		}
		w.PutUint(tagFabricIndex, uint64(f.Index))
		w.End()
	}
	w.End()
	return nil
}

func (c *Commissioning) readFabrics(req *im.Request, w *tlv.Writer, tag tlv.Tag) error {
	w.StartArray(tag)
	for _, f := range c.fabrics.Fabrics() {
		if req.FabricFiltered && f.Index != req.FabricIndex {
			continue
		}
		w.StartStructure(tlv.Anonymous)
		w.PutBytes(tlv.Context(1), f.RootPublicKey)
		w.PutUint(tlv.Context(2), uint64(f.VendorID))
		w.PutUint(tlv.Context(3), f.FabricID)
		w.PutUint(tlv.Context(4), f.NodeID)
		w.PutUTF8(tlv.Context(5), f.Label)
		w.PutUint(tagFabricIndex, uint64(f.Index))
		w.End()
	}
	w.End()
	return nil
}

// nonceField reads a command whose only required field, 0, is a 32-byte nonce.
func nonceField(fields *tlv.Reader, extra func(r *tlv.Reader, tag uint8) error) ([]byte, error) {
	var nonce []byte
	err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 0 {
			nonce, err = r.Bytes()
			nonce = append([]byte(nil), nonce...)
			return err
		}
		if extra != nil {
			return extra(r, tag)
		}
		return nil
	})
	if err != nil || nonce == nil {
		return nil, im.StatusInvalidCommand
	}
	if len(nonce) != attestation.NonceSize {
		return nil, im.StatusInvalidCommand
	}
	return nonce, nil
}

// signed signs elements with the session's attestation challenge, and returns a response with
// both.
func (c *Commissioning) signed(req *im.Request, command uint32, elements []byte) (*im.Response, error) {
	challenge := req.Session.AttestationChallenge()
	sig, err := c.creds.Sign(append(append([]byte(nil), elements...), challenge...))
	if err != nil {
		c.log.Error(err, "Could not sign with the DAC key")
		return nil, im.StatusFailure
	}
	return &im.Response{Command: command, Fields: func(w *tlv.Writer) {
		w.PutBytes(tlv.Context(0), elements)
		w.PutBytes(tlv.Context(1), sig)
	}}, nil
}

func (c *Commissioning) attestationRequest(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	nonce, err := nonceField(fields, nil)
	if err != nil {
		return nil, err
	}
	return c.signed(req, 0x01, attestation.AttestationElements(c.creds.CD, nonce, 0))
}

func (c *Commissioning) certificateChainRequest(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var kind uint64
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 0 {
			kind, err = r.Uint()
		}
		return err
	}); err != nil {
		return nil, im.StatusInvalidCommand
	}
	var der []byte
	switch kind {
	case 1:
		der = c.creds.DAC
	case 2:
		der = c.creds.PAI
	default:
		return nil, im.StatusInvalidCommand
	}
	return &im.Response{Command: 0x03, Fields: func(w *tlv.Writer) { w.PutBytes(tlv.Context(0), der) }}, nil
}

func (c *Commissioning) csrRequest(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	forUpdate := false
	nonce, err := nonceField(fields, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 1 {
			forUpdate, err = r.Bool()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	fs := &c.failSafe
	switch {
	case !fs.armed:
		return nil, im.StatusFailsafeRequired
	case fs.nocAdded || fs.nocUpdated:
		return nil, im.StatusConstraintError
	case forUpdate && req.AuthMode != transport.AuthCASE:
		return nil, im.StatusInvalidCommand
	}
	key, err := c.fabrics.NewOperationalKey()
	if err != nil {
		return nil, im.StatusFailure
	}
	csr, err := cert.NewCSR(key)
	if err != nil {
		return nil, im.StatusFailure
	}
	fs.csr, fs.csrForUpdate = true, forUpdate
	return c.signed(req, 0x05, attestation.NOCSRElements(csr, nonce))
}

func (c *Commissioning) addTrustedRoot(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var rcac []byte
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 0 {
			rcac, err = r.Bytes()
		}
		return err
	}); err != nil || rcac == nil {
		return nil, im.StatusInvalidCommand
	}
	fs := &c.failSafe
	switch {
	case !fs.armed:
		return nil, im.StatusFailsafeRequired
	case fs.rootAdded || fs.nocUpdated:
		return nil, im.StatusConstraintError
	}
	if err := c.fabrics.AddTrustedRoot(rcac); err != nil {
		if errors.Is(err, fabric.ErrPending) {
			return nil, im.StatusConstraintError
		}
		return nil, im.StatusInvalidCommand
	}
	fs.rootAdded = true
	return nil, nil
}

// validAdminSubject reports whether s may be an ACL subject: an operational node ID or a CAT.
func validAdminSubject(s uint64) bool {
	if isCAT(s) {
		return s&0xFFFF != 0
	}
	return cert.IsOperationalNodeID(s)
}

func (c *Commissioning) addNOC(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var noc, icac, ipk []byte
	var subject, vendor uint64
	var seen [5]bool
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			noc, err = r.Bytes()
		case 1:
			icac, err = r.Bytes()
		case 2:
			ipk, err = r.Bytes()
		case 3:
			subject, err = r.Uint()
		case 4:
			vendor, err = r.Uint()
		default:
			return nil
		}
		seen[tag] = true
		return err
	}); err != nil || !seen[0] || !seen[2] || !seen[3] || !seen[4] || vendor > 0xFFFE {
		return nil, im.StatusInvalidCommand
	}
	fs := &c.failSafe
	switch {
	case !fs.armed:
		return nil, im.StatusFailsafeRequired
	case fs.nocAdded || fs.nocUpdated || fs.csrForUpdate:
		return nil, im.StatusConstraintError
	case !fs.csr:
		return nocResponse(nocMissingCSR, 0), nil
	case !validAdminSubject(subject):
		return nocResponse(nocInvalidAdminSubject, 0), nil
	}
	index, err := c.fabrics.AddNOC(noc, icac, ipk, subject, uint16(vendor))
	if err != nil {
		c.log.Info("Refused AddNOC", "error", err.Error())
		return nocResponse(nocStatus(err), 0), nil
	}
	fs.nocAdded, fs.fabricIndex = true, index
	if req.AuthMode == transport.AuthPASE {
		req.Session.SetFabricIndex(index)
	}
	c.fabricsChanged()
	return nocResponse(nocOK, index), nil
}

func (c *Commissioning) updateNOC(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var noc, icac []byte
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			noc, err = r.Bytes()
		case 1:
			icac, err = r.Bytes()
		}
		return err
	}); err != nil || noc == nil {
		return nil, im.StatusInvalidCommand
	}
	fs := &c.failSafe
	switch {
	case req.AuthMode != transport.AuthCASE || req.FabricIndex == 0:
		return nil, im.StatusUnsupportedAccess
	case !fs.armed:
		return nil, im.StatusFailsafeRequired
	case fs.nocAdded || fs.nocUpdated || fs.rootAdded:
		return nil, im.StatusConstraintError
	case !fs.csr || !fs.csrForUpdate:
		return nocResponse(nocMissingCSR, 0), nil
	}
	if err := c.fabrics.UpdateNOC(req.FabricIndex, noc, icac); err != nil {
		return nocResponse(nocStatus(err), 0), nil
	}
	fs.nocUpdated = true
	c.fabricsChanged()
	return nocResponse(nocOK, req.FabricIndex), nil
}

func (c *Commissioning) updateFabricLabel(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var label string
	seen := false
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 0 {
			label, err = r.UTF8()
			seen = true
		}
		return err
	}); err != nil || !seen {
		return nil, im.StatusInvalidCommand
	}
	if req.FabricIndex == 0 {
		return nil, im.StatusUnsupportedAccess
	}
	if len(label) > maxLabelLength {
		return nil, im.StatusConstraintError
	}
	if err := c.fabrics.SetLabel(req.FabricIndex, label); err != nil {
		return nocResponse(nocStatus(err), 0), nil
	}
	c.opCreds.Changed(0x0001)
	return nocResponse(nocOK, req.FabricIndex), nil
}

func (c *Commissioning) removeFabric(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var index uint64
	seen := false
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 0 {
			index, err = r.Uint()
			seen = true
		}
		return err
	}); err != nil || !seen || index == 0 || index > 254 {
		return nil, im.StatusInvalidCommand
	}
	i := uint8(index)
	if pending, added, _ := c.fabrics.Pending(); added && pending == i {
		// Removing the fabric being commissioned rolls the commissioning back.
		c.expireFailSafe()
		return nocResponse(nocOK, i), nil
	}
	if c.failSafe.armed && c.failSafe.fabricIndex == i {
		// The fabric's admin is gone: so is what its fail-safe guards, such as an UpdateNOC.
		c.expireFailSafe()
	}
	if err := c.fabrics.Remove(i); err != nil {
		if !errors.Is(err, fabric.ErrInvalidIndex) {
			c.log.Error(err, "Could not remove fabric", "fabric", i)
			return nil, im.StatusFailure
		}
		return nocResponse(nocInvalidFabricIndex, 0), nil
	}
	c.log.Info("Removed fabric", "fabric", i)
	c.fabricsChanged()
	if c.hooks.FabricRemoved != nil {
		c.hook(func() { c.hooks.FabricRemoved(i) })
	}
	return nocResponse(nocOK, i), nil
}
