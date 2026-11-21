package clusters

import (
	"oikade.dev/internal/matter/im"
	"oikade.dev/internal/matter/tlv"
)

const (
	IDDoorLock uint32 = 0x0101
)

// DeviceDoorLock is the Door Lock device type, at Matter 1.3's revision. It needs Identify, which
// every bridged endpoint has, and Door Lock.
var DeviceDoorLock = im.DeviceType{ID: 0x000A, Revision: 3}

// The LockState values (LockStateEnum).
const (
	LockStateNotFullyLocked = 0
	LockStateLocked         = 1
	LockStateUnlocked       = 2
)

const (
	CommandLockDoor   = 0x00
	CommandUnlockDoor = 0x01
)

// Lock is a Door Lock cluster with no optional features: lock and unlock, with no PINs, users or
// schedules.
//
// It has no events, though the spec requires DoorLockAlarm and LockOperation, because the stack
// has none yet (OIK-23).
type Lock struct {
	cluster *im.Cluster
	// locked is the bolt as the device last reported it, nil until it has.
	locked *bool
	// OnCommand is called, under the Node's lock, when a client locks or unlocks. It must not
	// block.
	OnCommand func(locked bool)
}

// NewLock returns a Door Lock cluster. LockState comes only from the device's reports, never from
// a command, so a controller never shows a bolt that hasn't moved.
func NewLock() *Lock {
	l := &Lock{}
	command := func(locked bool) func(*im.Request, *tlv.Reader) (*im.Response, error) {
		return func(*im.Request, *tlv.Reader) (*im.Response, error) {
			if l.OnCommand != nil {
				l.OnCommand(locked)
			}
			return nil, nil
		}
	}
	l.cluster = &im.Cluster{
		ID: IDDoorLock, Revision: 7,
		Attributes: []im.Attribute{
			readOnly(0x0000, func(w *tlv.Writer, tag tlv.Tag) { // LockState
				switch {
				case l.locked == nil:
					w.PutNull(tag)
				case *l.locked:
					w.PutUint(tag, LockStateLocked)
				default:
					w.PutUint(tag, LockStateUnlocked)
				}
			}),
			uintAttr(0x0001, 0), // LockType: DeadBolt
			readOnly(0x0002, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, true) }), // ActuatorEnabled
			uintAttr(0x0025, 0), // OperatingMode: Normal
			// SupportedOperatingModes: a bit clear is a mode supported, so this is Normal and
			// NoRemoteLockUnlock, the two every lock has.
			uintAttr(0x0026, 0xFFF6),
		},
		Commands: []im.Command{
			{ID: CommandLockDoor, Invoke: command(true), Timed: true},
			{ID: CommandUnlockDoor, Invoke: command(false), Timed: true},
		},
	}
	return l
}

// Cluster returns the cluster to add to an endpoint.
func (l *Lock) Cluster() *im.Cluster { return l.cluster }

// Set records whether the bolt is fully thrown, as the device reports it. Call it inside
// Node.Update.
func (l *Lock) Set(locked bool) {
	if l.locked == nil || *l.locked != locked {
		l.locked = &locked
		l.cluster.Changed(0x0000)
	}
}
