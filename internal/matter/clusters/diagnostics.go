package clusters

import (
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// Cluster IDs of the clusters every root node and bridged device has.
const (
	IDIdentify           uint32 = 0x0003
	IDGeneralDiagnostics uint32 = 0x0033
)

// GeneralDiagnostics is the root endpoint's General Diagnostics cluster, which the Root Node
// device type requires: no network interfaces listed, no reboots counted, the up time since
// started, and test event triggers disabled.
func GeneralDiagnostics(started time.Time) *im.Cluster {
	const (
		testEventTrigger     = 0x00
		timeSnapshot         = 0x01
		timeSnapshotResponse = 0x02
	)
	return &im.Cluster{
		ID: IDGeneralDiagnostics, Revision: 2,
		Attributes: []im.Attribute{
			readOnly(0x00, func(w *tlv.Writer, tag tlv.Tag) { w.StartArray(tag); w.End() }), // NetworkInterfaces
			uintAttr(0x01, 0), // RebootCount
			readOnly(0x02, func(w *tlv.Writer, tag tlv.Tag) { // UpTime, in seconds
				w.PutUint(tag, uint64(time.Since(started)/time.Second))
			}),
			readOnly(0x08, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, false) }), // TestEventTriggersEnabled
		},
		Commands: []im.Command{
			{ID: testEventTrigger, Privilege: im.PrivilegeManage, Invoke: func(*im.Request, *tlv.Reader) (*im.Response, error) {
				// No enable key is valid while TestEventTriggersEnabled is false.
				return nil, im.StatusConstraintError
			}},
			{ID: timeSnapshot, Invoke: func(*im.Request, *tlv.Reader) (*im.Response, error) {
				return &im.Response{Command: timeSnapshotResponse, Fields: func(w *tlv.Writer) {
					w.PutUint(tlv.Context(0), uint64(time.Since(started)/time.Millisecond)) // SystemTimeMs
					w.PutUint(tlv.Context(1), uint64(time.Now().UnixMilli()))               // PosixTimeMs
				}}, nil
			}},
		},
		Generated: []uint32{timeSnapshotResponse},
	}
}

// identify is an endpoint's Identify cluster, which every bridged device type requires. A bridge
// can't make the device itself identify, so it only counts IdentifyTime down.
type identify struct {
	cluster *im.Cluster
	seconds uint16
	timer   *time.Timer
}

func newIdentify() *im.Cluster {
	id := &identify{}
	id.cluster = &im.Cluster{
		ID: IDIdentify, Revision: 4,
		Attributes: []im.Attribute{
			{
				ID:   0x0000, // IdentifyTime
				Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { w.PutUint(tag, uint64(id.seconds)); return nil },
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					v, err := r.Uint()
					if err != nil || op != im.Replace {
						return im.StatusInvalidDataType
					}
					if v > 0xFFFF {
						return im.StatusConstraintError
					}
					id.start(uint16(v))
					return im.StatusSuccess
				},
				WritePrivilege: im.PrivilegeManage,
			},
			uintAttr(0x0001, 0), // IdentifyType: None
		},
		Commands: []im.Command{
			{ID: 0x00, Privilege: im.PrivilegeManage, Invoke: func(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
				var seconds uint64
				seen := false
				if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
					if tag == 0 {
						seconds, err = r.Uint()
						seen = true
					}
					return err
				}); err != nil || !seen {
					return nil, im.StatusInvalidCommand
				}
				if seconds > 0xFFFF {
					return nil, im.StatusConstraintError
				}
				id.start(uint16(seconds))
				return nil, nil
			}},
		},
	}
	return id.cluster
}

// start sets IdentifyTime, and clears it when it runs out. Call it with the Node's lock.
func (id *identify) start(seconds uint16) {
	if id.timer != nil {
		id.timer.Stop()
		id.timer = nil
	}
	id.set(seconds)
	if seconds == 0 {
		return
	}
	var t *time.Timer
	t = time.AfterFunc(time.Duration(seconds)*time.Second, func() {
		if n := id.cluster.Node(); n != nil {
			n.Update(func() {
				if id.timer == t {
					id.timer = nil
					id.set(0)
				}
			})
		}
	})
	id.timer = t
}

func (id *identify) set(seconds uint16) {
	if seconds != id.seconds {
		id.seconds = seconds
		id.cluster.Changed(0x0000)
	}
}
