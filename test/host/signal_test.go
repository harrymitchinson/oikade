package host

import (
	"encoding/json"
	"strings"
	"testing"

	"oikade.dev/internal/host/api"
	"oikade.dev/internal/testkit"
)

// changedSignal is your process's line as a signal's value changes.
const changedSignal = "Changed Signal"

// leaveHome has the presence sensor stop seeing anyone, so the signal anyone-home lingers.
func (r *rig) leaveHome() {
	r.t.Helper()
	r.sync()
	r.change("presence-sensor", "occupied", false)
	r.sync()
	if n := r.count(changedSignal, "signal", "signal/anyone-home"); n != 0 {
		r.t.Fatalf("a signal changed as its linger began\n%s", r.dump())
	}
}

// awayNever says the away mode, which reads the signal, was never written.
func (r *rig) awayNever() {
	r.t.Helper()
	if n := r.count(wrote, "device", "hardware/simulator/away-mode", "value", true); n != 0 {
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
	r.awaitCount(1, wrote, "device", "hardware/simulator/away-mode", "value", true)
	if n := r.count(changedSignal, "signal", "signal/someone-home"); n != 0 {
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

// oikade status signal/NAME says what a signal is, why, and when its Linger ends.
func TestStatusSaysWhatASignalIsAndWhy(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.leaveHome()
	var out string
	testkit.Eventually(t, "status to say why the signal lingers", func() bool {
		out = string(r.stdout(nil, "status", "signal/anyone-home"))
		return strings.Contains(out, "Its conditions are false: hardware/simulator/presence-sensor/occupied is false")
	})
	if !strings.Contains(out, "signal/anyone-home  true since ") || !strings.Contains(out, ", false at ") {
		t.Errorf("status doesn't say it's true until its linger ends:\n%s", out)
	}

	var st api.Status
	if err := json.Unmarshal(r.stdout(nil, "status", "--json", "signal/anyone-home"), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Signals) != 1 || st.Signals[0].Value == nil || !*st.Signals[0].Value || st.Signals[0].OffAt == nil {
		t.Errorf("status --json has %+v, want anyone-home true with its linger's end", st.Signals)
	}
}

// A signal's changes are in the host's event log, which oikade history reads, and outlive the host.
func TestHistoryListsASignalsChangesAcrossAHostRestart(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.leaveHome()
	r.change("presence-sensor", "occupied", true)
	r.sync()
	wantHistory := func() {
		t.Helper()
		var out string
		testkit.Eventually(t, "history to list both changes", func() bool {
			out = string(r.stdout(nil, "history", "signal/seen-now"))
			return strings.Contains(out, "signal/seen-now  false  was true") &&
				strings.Contains(out, "signal/seen-now  true  was false")
		})
		if strings.Contains(out, "anyone-home") {
			t.Errorf("history of one signal lists another's:\n%s", out)
		}
	}
	wantHistory()
	r.stop()
	r.launch()
	r.waitFor(func(l line) bool { return l.is(attached) })
	wantHistory()
}

// A signal the running version doesn't make fails status, naming the command that lists them, and
// a list of signals is an array.
func TestStatusOfNoSuchSignalSaysHowToListThem(t *testing.T) {
	t.Parallel()
	r := start(t)
	stderr, err := r.oikade(nil, "status", "signal/nope")
	if err == nil || !strings.Contains(stderr, "no signal/nope in the running version") ||
		!strings.Contains(stderr, "oikade list signals") {
		t.Errorf("status signal/nope: %v\n%s", err, stderr)
	}
	var listed []api.Signal
	if err := json.Unmarshal(r.stdout(nil, "list", "signal", "--json"), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Ref != "signal/anyone-home" || listed[1].Ref != "signal/seen-now" {
		t.Errorf("list signal --json is %+v, want anyone-home and seen-now, in the order made", listed)
	}

	r.mustDeploy("nosignals")
	if out := strings.TrimSpace(string(r.stdout(nil, "list", "signal", "--json"))); out != "[]" {
		t.Errorf("list signal --json with no signals is %s, want []", out)
	}
}
