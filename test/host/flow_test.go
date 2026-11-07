package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/fault"
	"github.com/harrymitchinson/oikade/internal/store"
)

const (
	opened   = "open"
	finished = "Finished flow run"
	loaded   = "Loaded rules"
	waiting  = "Waiting in flow run"

	// routineSteps is how many journal writes a run of the fixture's routine makes.
	routineSteps = 9
	// Which of them repeat something if they aren't durable. The others are safe to lose.
	stepLamp0, stepLamp1, stepPlan, stepDone = 3, 4, 5, 8
)

// flowRig is a rig with the fixture's files, and a planned crash if faults is set.
func flowRig(t *testing.T, faults string) *rig {
	t.Helper()
	r := prepare(t)
	file := filepath.Join(r.dir, "faults")
	if faults != "" {
		if err := os.WriteFile(file, []byte(faults), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.env = []string{
		"OIKADE_FAULTS=" + file,
		"OIKADE_TEST_CALLS=" + filepath.Join(r.dir, "calls"),
		"OIKADE_TEST_EDITED=" + filepath.Join(r.dir, "edited"),
		"OIKADE_TEST_CRASH=" + filepath.Join(r.dir, "crash"),
	}
	return r
}

// contact opens or closes a contact sensor, without waiting for the host to have it.
func (r *rig) contact(device string, open bool) {
	r.t.Helper()
	raw, err := json.Marshal(map[string]any{"device": device, "capability": opened, "value": open})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.stdin.Write(append(raw, '\n')); err != nil {
		r.t.Fatal(err)
	}
}

// awaitLoaded waits for a process to be ready, and returns how many runs it resumed.
func (r *rig) awaitLoaded() int {
	r.t.Helper()
	resumed := 0
	r.waitFor(func(l line) bool {
		if !l.is(loaded) {
			return false
		}
		n, _ := l["resumedRuns"].(float64)
		resumed = int(n)
		return true
	})
	return resumed
}

func (r *rig) calls() int {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, "calls"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		r.t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}

// stop stops the host, and returns once its store is free.
func (r *rig) stop() {
	r.t.Helper()
	if err := r.cmd.Process.Signal(os.Interrupt); err != nil {
		r.t.Fatal(err)
	}
	select {
	case err := <-r.exit:
		r.exit <- err
	case <-time.After(waitLimit):
		r.t.Fatalf("the host didn't stop. The log:\n%s", r.dump())
	}
}

// died waits for a host killed by a fault point.
func (r *rig) died() {
	r.t.Helper()
	select {
	case err := <-r.exit:
		r.exit <- err
	case <-time.After(waitLimit):
		r.t.Fatalf("the host didn't die. The log:\n%s", r.dump())
	}
}

func (r *rig) openStore() *store.Store {
	r.t.Helper()
	s, err := store.Open(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = s.Close() })
	return s
}

// wantRoutine is what a kill at a step should leave: each thing once, but for the step whose entry
// never reached disk, which runs again.
type wantRoutine struct {
	lamp0, lamp1, done, calls int
}

func routineWant(step int, durable bool) wantRoutine {
	w := wantRoutine{lamp0: 1, lamp1: 1, done: 1, calls: 1}
	if durable {
		return w
	}
	switch step {
	case stepLamp0:
		w.lamp0 = 2
	case stepLamp1:
		w.lamp1 = 2
	case stepPlan:
		w.calls = 2
	case stepDone:
		w.done = 2
	}
	return w
}

// survey says what the routine did, and whether its journal is gone.
func (r *rig) survey() (wantRoutine, []string, error) {
	r.t.Helper()
	got := wantRoutine{
		lamp0: r.count(wrote, "device", "lamp-0"),
		lamp1: r.count(wrote, "device", "lamp-1"),
		done:  r.count(wrote, "device", "routine-done"),
		calls: r.calls(),
	}
	if n := r.count(wrote, "device", "routine-wrong"); n != 0 {
		return got, nil, fmt.Errorf("took the wrong branch %d times", n)
	}
	s := r.openStore()
	defer func() { _ = s.Close() }()
	runs, err := s.Journal().Runs()
	return got, runs, err
}

// killAt starts the routine, lets the planned crash land, and sees the run home.
func killAt(t *testing.T, r *rig, point string, step int) {
	t.Helper()
	inHost := strings.HasPrefix(point, "host.")
	notStarted := step == 1 && strings.HasSuffix(point, ".before")

	r.launch()
	r.awaitLoaded()
	r.contact("flow-go", true)
	if inHost {
		r.died()
		r.launch()
	}
	resumed := r.awaitLoaded()
	switch {
	case resumed > 0:
	case notStarted:
		// It died before the run's first entry, so no run exists, and the trigger is gone with
		// it. This is the one place a crash can lose work, and it's what a moment does too.
		// A restarted host's device is back to false, and your process's stays true.
		r.contact("flow-go", inHost)
	default:
		if step != routineSteps {
			t.Fatalf("no run to resume after step %d. The log:\n%s", step, r.dump())
		}
		r.stop()
		return
	}
	r.waitFor(func(l line) bool { return l.is(finished, "rule", "routine") })
	r.stop()
}

// TestKilledAtEveryJournalBoundary kills each side at every journal write of a flow with a loop, a
// branch, a wait and an outside call, just before and just after the entry is durable.
func TestKilledAtEveryJournalBoundary(t *testing.T) {
	for _, tc := range []struct {
		point   string
		durable bool
	}{
		{fault.ClientJournalBefore, false},
		{fault.ClientJournalAfter, true},
		{fault.HostJournalBefore, false},
		{fault.HostJournalAfter, true},
	} {
		for step := 1; step <= routineSteps; step++ {
			t.Run(fmt.Sprintf("%s/%d", tc.point, step), func(t *testing.T) {
				t.Parallel()
				r := flowRig(t, fmt.Sprintf("%s %d kill\n", tc.point, step))
				killAt(t, r, tc.point, step)
				got, runs, err := r.survey()
				if err != nil {
					t.Fatal(err)
				}
				if want := routineWant(step, tc.durable); got != want {
					t.Fatalf("got %+v, want %+v. The log:\n%s", got, want, r.dump())
				}
				if len(runs) != 0 {
					t.Fatalf("runs left in the journal: %v", runs)
				}
			})
		}
	}
}

// TestBrokenWriteAheadFailsTheCheck proves the test above can fail. The host answers the plan's
// entry without writing it, as a journal that isn't write-ahead would, and a kill lands once the
// flow has been told it's durable. The call then runs twice, which the boundary test forbids.
func TestBrokenWriteAheadFailsTheCheck(t *testing.T) {
	r := flowRig(t, fmt.Sprintf("%s %d drop\n%s %d kill\n",
		fault.HostJournalAppend, stepPlan, fault.ClientJournalAfter, stepPlan))
	killAt(t, r, fault.ClientJournalAfter, stepPlan)
	got, _, err := r.survey()
	if err != nil {
		t.Fatal(err)
	}
	if want := routineWant(stepPlan, true); got == want {
		t.Fatalf("the check passed with the journal broken: %+v", got)
	}
	if got.calls != 2 {
		t.Fatalf("got %+v, want the outside call run twice", got)
	}
}

func TestChangedFlowIsDroppedAndTheNextTriggerStartsAgain(t *testing.T) {
	r := flowRig(t, "")
	r.launch()
	r.awaitLoaded()
	r.contact("edit-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "editable") })
	r.sync()

	// The edit lands while it waits, and takes effect when the process is replaced.
	if err := os.WriteFile(filepath.Join(r.dir, "edited"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.kill()
	r.waitFor(func(l line) bool {
		return l.is("Dropped flow run; its code changed while it waited", "rule", "editable")
	})
	r.sync()
	if n := r.count(wrote, "device", "edit-out"); n != 0 {
		t.Fatalf("a changed flow went on to act: %d writes", n)
	}

	r.contact("edit-go", false)
	r.wrote("edit-out", true)
}

func TestClaimInAFlowSurvivesAKillAndIsReleased(t *testing.T) {
	r := flowRig(t, "")
	r.launch()
	r.awaitLoaded()
	r.contact("claim-flow-go", true)
	r.wrote("claimed-flow", false)
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "claiming") })

	r.kill()
	if resumed := r.awaitLoaded(); resumed != 1 {
		t.Fatalf("resumed %d runs", resumed)
	}
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "claiming") })
	r.change("claim-end", opened, true)
	r.wrote("claimed-flow", true)
	r.waitFor(func(l line) bool { return l.is(finished, "rule", "claiming") })
}

// TestClaimSurvivesTheHostDyingRightAfterItsAnswer is the gap a save behind the answer leaves open:
// a moment doesn't run twice, so a lost claim wouldn't come back.
// claimsOf reads the claims of the rule "failing" saved for a device's switch, and frees the store
// for the next host.
func (r *rig) claimsOf(device string) []control.Claim {
	r.t.Helper()
	s, err := store.Open(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	raw, ok, err := s.Namespace(engine.ControlStatesNamespace).Get("home/" + device)
	if err != nil || !ok {
		r.t.Fatalf("no saved state for %s (%v). The log:\n%s", device, err, r.dump())
	}
	var st engine.ControlState
	if err := json.Unmarshal(raw, &st); err != nil {
		r.t.Fatal(err)
	}
	return slices.DeleteFunc(st["on"].Claims, func(c control.Claim) bool { return c.Source != "Rule/failing" })
}

func TestFailedRunReleasesItsNormalClaimAndKeepsItsSafetyOne(t *testing.T) {
	// Each ends with a restart, so what the release decided has to come back from disk.
	check := func(t *testing.T, r *rig) {
		t.Helper()
		if got := r.claimsOf("claimed-normal"); len(got) != 0 {
			t.Fatalf("the normal claim stayed: %+v", got)
		}
		if got := r.claimsOf("claimed-safety"); len(got) != 1 || got[0].Level != control.Safety {
			t.Fatalf("the Safety claim went: %+v", got)
		}
	}
	survivesRestart := func(t *testing.T, r *rig) {
		t.Helper()
		r.sync()
		r.stop()
		check(t, r)
		r.launch()
		if resumed := r.awaitLoaded(); resumed != 0 {
			t.Fatalf("a failed run came back: resumed %d runs", resumed)
		}
		r.sync()
		r.stop()
		check(t, r)
	}
	start := func(t *testing.T) *rig {
		t.Helper()
		r := flowRig(t, "")
		r.launch()
		r.awaitLoaded()
		r.contact("fail-go", true)
		r.wrote("claimed-normal", false)
		r.wrote("claimed-safety", false)
		r.waitFor(func(l line) bool { return l.is(waiting, "rule", "failing") })
		return r
	}
	failed := func(l line) bool { return l.is("Failed flow run", "rule", "failing") }

	t.Run("the claim's device goes back, and the Safety one's doesn't", func(t *testing.T) {
		r := start(t)
		r.change("fail-now", opened, true)
		r.waitFor(failed)
		r.wrote("claimed-normal", true)
		survivesRestart(t, r)
		if n := r.count(wrote, "device", "claimed-safety", "value", true); n != 0 {
			t.Fatalf("the Safety claim's device was written back %d times", n)
		}
	})
	// A new host's simulator reports its first value, which outranks a normal claim as an outside
	// change, so here the saved state shows the release and no write does.
	t.Run("the release reaches claims a new host restored", func(t *testing.T) {
		r := start(t)
		r.stop()
		if got := r.claimsOf("claimed-normal"); len(got) != 1 {
			t.Fatalf("the normal claim wasn't saved: %+v", got)
		}
		r.launch()
		if resumed := r.awaitLoaded(); resumed != 1 {
			t.Fatalf("resumed %d runs", resumed)
		}
		r.waitFor(func(l line) bool { return l.is(waiting, "rule", "failing") })
		r.change("fail-now", opened, true)
		r.waitFor(failed)
		survivesRestart(t, r)
	})
}

func TestClaimSurvivesTheHostDyingRightAfterItsAnswer(t *testing.T) {
	// No hold has started, so this moment's is the first claim the host sees.
	r := flowRig(t, fault.HostClaimReplied+" 1 kill\n")
	r.launch()
	r.awaitLoaded()
	r.contact("claim-go", true)
	r.died()

	s := r.openStore()
	raw, ok, err := s.Namespace(engine.ControlStatesNamespace).Get("home/claimed")
	if err != nil || !ok {
		t.Fatalf("no saved state for the device (%v). The log:\n%s", err, r.dump())
	}
	var st engine.ControlState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	claimed := func(c control.Claim) bool { return c.Source == "Rule/claim-moment" }
	if !slices.ContainsFunc(st["on"].Claims, claimed) {
		t.Fatalf("the answered claim wasn't saved: %s", raw)
	}
}

func seedFire(t *testing.T, r *rig, rule string, at time.Time) {
	t.Helper()
	s := r.openStore()
	defer func() { _ = s.Close() }()
	raw, err := json.Marshal(at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Namespace(engine.FireStatesNamespace).Put("home/"+rule, raw); err != nil {
		t.Fatal(err)
	}
}

func TestTimeOfDayMissedWhileNoProcessRan(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	t.Run("a little late fires", func(t *testing.T) {
		r := flowRig(t, "")
		r.env = append(r.env, "OIKADE_TEST_AT="+now.Add(-time.Minute).Format("15:04"))
		seedFire(t, r, "timed", now.Add(-3*time.Minute))
		r.launch()
		r.wrote("timed-out", true)
	})
	t.Run("too late is skipped and logged", func(t *testing.T) {
		r := flowRig(t, "")
		r.env = append(r.env, "OIKADE_TEST_AT="+now.Add(-20*time.Minute).Format("15:04"))
		seedFire(t, r, "timed", now.Add(-30*time.Minute))
		r.launch()
		r.waitFor(func(l line) bool {
			return l.is("Skipped missed trigger; it is older than the startingDeadline")
		})
		r.sync()
		if n := r.count(wrote, "device", "timed-out"); n != 0 {
			t.Fatalf("a time of day fired 20 minutes late: %d writes", n)
		}
	})
}
