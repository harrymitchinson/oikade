package clusters

import (
	"slices"
	"testing"

	"oikade.dev/internal/matter/im"
)

// A controller locks and unlocks over a session, only in a timed interaction, and LockState follows
// the bolt's reports, not the commands.
func TestALockIsDrivenByTimedCommands(t *testing.T) {
	f := newFixture(t)
	lock := NewLock()
	var commands []bool
	lock.OnCommand = func(locked bool) { commands = append(commands, locked) }
	if _, err := f.bridge.AddDevice(3, DeviceDoorLock, "Back door", "lock-1", lock.Cluster()); err != nil {
		t.Fatal(err)
	}
	state := func() any {
		t.Helper()
		reports, err := f.client.Read(timeout(t), im.AttributePath{Endpoint: 3, Cluster: IDDoorLock, Attribute: 0x0000})
		if err != nil || len(reports) != 1 {
			t.Fatalf("reading LockState: %v, %+v", err, reports)
		}
		return value(t, reports[0])
	}
	invoke := func(command uint32, timed bool) im.Status {
		t.Helper()
		res, err := f.client.Invoke(timeout(t), im.CommandPath{Endpoint: 3, Cluster: IDDoorLock, Command: command}, nil, timed, 1000)
		if err != nil {
			t.Fatalf("invoking %#x: %v", command, err)
		}
		return res.Status
	}
	if state() != nil {
		t.Errorf("LockState before any report %v, want null", state())
	}

	if s := invoke(CommandLockDoor, false); s != im.StatusNeedsTimedInteraction {
		t.Errorf("an untimed LockDoor: %#x, want NeedsTimedInteraction", s)
	}
	if s := invoke(CommandLockDoor, true); s != im.StatusSuccess {
		t.Fatalf("LockDoor: %#x", s)
	}
	f.bridge.Node.View(func() {
		if !slices.Equal(commands, []bool{true}) {
			t.Errorf("commands %v, want one lock", commands)
		}
	})
	if state() != nil {
		t.Errorf("LockState before the bolt reports %v, want null", state())
	}
	f.bridge.Node.Update(func() { lock.Set(true) })
	if state() != uint64(LockStateLocked) {
		t.Errorf("LockState once the bolt reports %v, want Locked", state())
	}

	if s := invoke(CommandUnlockDoor, false); s != im.StatusNeedsTimedInteraction {
		t.Errorf("an untimed UnlockDoor: %#x, want NeedsTimedInteraction", s)
	}
	if s := invoke(CommandUnlockDoor, true); s != im.StatusSuccess {
		t.Fatalf("UnlockDoor: %#x", s)
	}
	if state() != uint64(LockStateLocked) {
		t.Errorf("LockState before the bolt reports %v, want still Locked", state())
	}
	f.bridge.Node.Update(func() { lock.Set(false) })
	if state() != uint64(LockStateUnlocked) {
		t.Errorf("LockState once the bolt reports %v, want Unlocked", state())
	}
	f.bridge.Node.View(func() {
		if !slices.Equal(commands, []bool{true, false}) {
			t.Errorf("commands %v, want a lock and an unlock", commands)
		}
	})
}
