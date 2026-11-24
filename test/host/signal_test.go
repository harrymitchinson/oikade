package host

import "testing"

// changedSignal is your process's line as a signal's value changes.
const changedSignal = "Changed Signal"

// leaveHome has the presence sensor stop seeing anyone, so the signal anyone-home lingers.
func (r *rig) leaveHome() {
	r.t.Helper()
	r.sync()
	r.change("presence-sensor", "occupied", false)
	r.sync()
	if n := r.count(changedSignal); n != 0 {
		r.t.Fatalf("a signal changed as its linger began\n%s", r.dump())
	}
}

// awayNever says the away mode, which reads the signal, was never written.
func (r *rig) awayNever() {
	r.t.Helper()
	if n := r.count(wrote, "device", "away-mode", "value", true); n != 0 {
		r.t.Fatalf("away-mode was written %d times, want none\n%s", n, r.dump())
	}
}

// The host keeps a signal's state, so a process killed mid-linger is followed by one that still
// reads the signal as true, rather than working it out afresh as false.
func TestASignalKeepsItsLingerAcrossAKill(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.leaveHome()
	r.kill()
	r.sync()
	r.awayNever()

	// The next process works the signal out from the sensor as before.
	r.change("presence-sensor", "occupied", true)
	r.sync()
	r.awayNever()
}

// A deploy that keeps a signal's name keeps its state, and one that renames it starts it afresh,
// from what its conditions say now.
func TestADeployKeepsASignalByItsName(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.leaveHome()
	r.mustDeploy("second")
	r.sync()
	r.awayNever()

	r.mustDeploy("renamed-signal")
	r.awaitCount(1, wrote, "device", "away-mode", "value", true)
	if n := r.count(changedSignal); n != 0 {
		t.Fatalf("the renamed signal's first value counted as a change\n%s", r.dump())
	}
}

// leftOut is your process's line as a signal starts leaving a dead device out.
const leftOut = "Left dead Devices out of Signal"

// Your process hears from the host that a device went offline, as it goes and in the welcome after,
// and leaves it out of a signal.
func TestASignalLeavesOutADeviceTheHostSaysIsOffline(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.online("presence-sensor", false)
	r.waitFor(func(l line) bool { return l.is(leftOut, "signal", "signal/anyone-home") })
	r.kill()
	r.waitFor(func(l line) bool { return l.is(leftOut, "signal", "signal/anyone-home") })
}
