package clusters

import (
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// groupKeyManagement is Group Key Management with only each fabric's IPK keyset, 0, which AddNOC
// installs. The node serves no groups, so it holds no other keysets.
func (c *Commissioning) groupKeyManagement() *im.Cluster {
	admin := im.PrivilegeAdminister
	emptyList := func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error {
		w.StartArray(tag)
		w.End()
		return nil
	}
	return &im.Cluster{
		ID: IDGroupKeyManagement, Revision: 1,
		Attributes: []im.Attribute{
			{
				ID: 0x0000, Read: emptyList, FabricScoped: true, WritePrivilege: im.PrivilegeManage, // GroupKeyMap
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					if op == im.Append {
						return im.StatusResourceExhausted
					}
					n := 0
					if err := im.Items(r, func(*tlv.Reader) error { n++; return nil }); err != nil {
						return im.StatusInvalidDataType
					}
					if n > 0 {
						return im.StatusResourceExhausted // no groups
					}
					return im.StatusSuccess
				},
			},
			{ID: 0x0001, Read: emptyList, FabricScoped: true}, // GroupTable
			uintAttr(0x0002, 0), // MaxGroupsPerFabric
			uintAttr(0x0003, 1), // MaxGroupKeysPerFabric: the IPK
		},
		Commands: []im.Command{
			{ID: 0x00, Invoke: c.keySetWrite, Privilege: admin},
			{ID: 0x01, Invoke: c.keySetRead, Privilege: admin},
			{ID: 0x03, Invoke: c.keySetRemove, Privilege: admin},
			{ID: 0x04, Invoke: c.keySetReadAllIndices, Privilege: admin},
		},
		Generated: []uint32{0x02, 0x05},
	}
}

func keySetID(fields *tlv.Reader) (uint64, error) {
	var id uint64
	seen := false
	err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 0 {
			id, err = r.Uint()
			seen = true
		}
		return err
	})
	if err != nil || !seen {
		return 0, im.StatusInvalidCommand
	}
	return id, nil
}

func (c *Commissioning) keySetWrite(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var id uint64
	seen := false
	err := readFields(fields, func(r *tlv.Reader, tag uint8) error {
		if tag != 0 {
			return nil
		}
		seen = true
		return im.Fields(r, func(r *tlv.Reader, tag uint8) (err error) {
			if tag == 0 {
				id, err = r.Uint()
			}
			return err
		})
	})
	if err != nil || !seen {
		return nil, im.StatusInvalidCommand
	}
	if id == 0 {
		return nil, im.StatusInvalidCommand // the IPK is AddNOC's
	}
	return nil, im.StatusResourceExhausted
}

func (c *Commissioning) keySetRead(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	id, err := keySetID(fields)
	if err != nil {
		return nil, err
	}
	if _, ok := c.fabrics.Fabric(req.FabricIndex); !ok || id != 0 {
		return nil, im.StatusNotFound
	}
	return &im.Response{Command: 0x02, Fields: func(w *tlv.Writer) {
		w.StartStructure(tlv.Context(0)) // GroupKeySetStruct, without its keys
		w.PutUint(tlv.Context(0), 0)
		w.PutUint(tlv.Context(1), 0) // TrustFirst
		w.PutNull(tlv.Context(2))
		w.PutUint(tlv.Context(3), 0)
		for tag := uint8(4); tag <= 7; tag++ {
			w.PutNull(tlv.Context(tag))
		}
		w.End()
	}}, nil
}

func (c *Commissioning) keySetRemove(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	id, err := keySetID(fields)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, im.StatusInvalidCommand
	}
	return nil, im.StatusNotFound
}

func (c *Commissioning) keySetReadAllIndices(req *im.Request, _ *tlv.Reader) (*im.Response, error) {
	_, ok := c.fabrics.Fabric(req.FabricIndex)
	return &im.Response{Command: 0x05, Fields: func(w *tlv.Writer) {
		w.StartArray(tlv.Context(0))
		if ok {
			w.PutUint(tlv.Anonymous, 0)
		}
		w.End()
	}}, nil
}
