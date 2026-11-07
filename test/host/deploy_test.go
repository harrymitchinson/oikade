package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/fault"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
)

const (
	deployed = "Deployed automations version"
	restored = "Restored the last automations version"
	stopping = "Stopping automations process for a new version"
	ended    = "Ended the claims of a removed rule"
	checked  = "Checked automations version"
	// settle is long enough for a process to crash in, and short enough to wait for.
	settle = "--settle=2s"
)

var (
	variantsMu sync.Mutex
	variants   = map[string]string{}
)

// variant builds the fixture as another version of the home.
func variant(t *testing.T, name string) string {
	t.Helper()
	build(t)
	variantsMu.Lock()
	defer variantsMu.Unlock()
	if path, ok := variants[name]; ok {
		return path
	}
	path := filepath.Join(buildDir, "home-"+name)
	args := []string{"build", "-tags", "faultpoints", "-ldflags", "-X main.variant=" + name}
	if raceBuild {
		args = append(args, "-race")
	}
	module := filepath.Join(buildDir, "module")
	if err := run(module, []string{"GOFLAGS=-mod=mod", "GOPROXY=off"}, append(args, "-o", path, ".")...); err != nil {
		t.Fatal(err)
	}
	variants[name] = path
	return path
}

// oikade runs the command against the rig's host.
func (r *rig) oikade(env []string, args ...string) (stderr string, err error) {
	r.t.Helper()
	bin := binFor(args)
	args = append(args[:1:1], append([]string{"--data-dir", r.dir}, args[1:]...)...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	err = cmd.Run()
	return errOut.String(), err
}

func (r *rig) deploy(name string, args ...string) (string, error) {
	r.t.Helper()
	stderr, err := r.oikade(nil, append([]string{"deploy", "--binary", variant(r.t, name)}, args...)...)
	return stderr, err
}

func (r *rig) mustDeploy(name string, args ...string) {
	r.t.Helper()
	if stderr, err := r.deploy(name, args...); err != nil {
		r.t.Fatalf("%v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
}

func (r *rig) awaitDeployed() {
	r.t.Helper()
	r.waitFor(func(l line) bool { return l.is(deployed) })
}

func (r *rig) mustRollback(args ...string) {
	r.t.Helper()
	if stderr, err := r.oikade(nil, append([]string{"rollback"}, args...)...); err != nil {
		r.t.Fatalf("%v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
}

func TestVersionThatFailsItsCheckNeverStopsTheOldOne(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.sync()
	pid := r.pid()
	stderr, err := r.deploy("badcheck")
	if err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	if !strings.Contains(stderr, "declared twice") {
		t.Fatalf("got %q", stderr)
	}
	r.sync()
	if got := r.pid(); got != pid || r.count(started) != 1 || r.count(stopping) != 0 {
		t.Fatalf("the process changed. The log:\n%s", r.dump())
	}
}

func TestVersionThatCrashesOnStartIsRolledBack(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"crash", "crash-late"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := start(t, settle)
			r.sync()
			stderr, err := r.deploy(name)
			if err == nil {
				t.Fatalf("the deploy succeeded. The log:\n%s", r.dump())
			}
			if !strings.Contains(stderr, "the new version failed") || !strings.Contains(stderr, "Still running version") {
				t.Fatalf("got %q", stderr)
			}
			r.waitFor(func(l line) bool { return l.is(restored) })
			r.sync()
			r.change("bathroom-sensor", "humidity", 80)
			r.wrote("fan", true)
		})
	}
}

func timeOf(t *testing.T, l line) time.Time {
	t.Helper()
	raw, _ := l["time"].(string)
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestDeployNeverOverlapsAndPausesBriefly(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.sync()
	r.mustDeploy("same")
	r.sync()
	var stop, connect time.Time
	connects, sessions := 0, 0
	r.mu.Lock()
	lines := append([]line(nil), r.lines...)
	r.mu.Unlock()
	for _, l := range lines {
		switch {
		case l.is(stopping):
			stop = timeOf(t, l)
			sessions = 0
		case l.is(attached):
			connects++
			sessions++
			if !stop.IsZero() && connect.IsZero() {
				connect = timeOf(t, l)
			}
		case strings.HasPrefix(l.msg(), "Refused"):
			t.Fatalf("a process was refused. The log:\n%s", r.dump())
		}
		if sessions > 1 {
			t.Fatalf("two sessions overlapped. The log:\n%s", r.dump())
		}
	}
	if connects != 2 {
		t.Fatalf("%d connections. The log:\n%s", connects, r.dump())
	}
	if gap := connect.Sub(stop); gap <= 0 || gap >= 2*time.Second {
		t.Fatalf("automations were down for %s", gap)
	}
}

func TestWaitingFlowAcrossADeploy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		variant string
		resumed int
		out     string
	}{
		"unchanged code":               {"same", 1, "park-out"},
		"a change past the wait":       {"tail", 1, "park-tail"},
		"a change to the step it's at": {"edited", 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := start(t, settle)
			r.contact("park-go", true)
			r.waitFor(func(l line) bool { return l.is(waiting) })
			r.mustDeploy(tc.variant)
			if got := r.count("Dropped flow run; its code changed while it waited"); tc.out == "" && got != 1 {
				t.Fatalf("the run wasn't dropped. The log:\n%s", r.dump())
			}
			r.sync()
			r.contact("park-end", true)
			if tc.out != "" {
				r.wrote(tc.out, true)
				return
			}
			r.sync()
			if r.count(wrote, "device", "park-out") != 0 || r.count(wrote, "device", "park-tail") != 0 {
				t.Fatalf("a dropped run carried on. The log:\n%s", r.dump())
			}
		})
	}
}

func TestRollbackRestoresThePreviousVersionAndReplaysFlows(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.mustDeploy("same")
	r.contact("park-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting) })
	r.mustDeploy("tail")
	r.mustRollback()
	r.sync()
	r.contact("park-end", true)
	r.wrote("park-out", true) // The tail variant would write park-tail.
	if got := r.count(wrote, "device", "park-tail"); got != 0 {
		t.Fatalf("the rolled-back code still ran. The log:\n%s", r.dump())
	}
}

func TestRollbackWithNothingToGoBackTo(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	if stderr, err := r.oikade(nil, "rollback"); err == nil || !strings.Contains(stderr, "no earlier version") {
		t.Fatalf("got %v: %s", err, stderr)
	}
}

func TestDeployFinishesAfterTheCommandThatAskedIsKilled(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	cmd := exec.Command(ctlBin, "deploy", "--data-dir", r.dir, "--binary", variant(t, "second"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Once the binary is all there: before that, a deploy that's cut off is no deploy.
	r.waitFor(func(l line) bool { return l.is("Received deploy upload") })
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	r.waitFor(func(l line) bool { return l.is(deployed) })
}

func TestAnUploadCutShortDeploysNothingAndLeavesNothing(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	conn, err := net.Dial("unix", filepath.Join(r.dir, api.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	head := "POST /v1/deploy HTTP/1.1\r\nHost: oikade\r\nContent-Type: %s\r\nContent-Length: 1000000\r\n\r\n%s"
	_, _ = fmt.Fprintf(conn, head, api.BinaryContentType, strings.Repeat("x", 1000))
	_ = conn.Close()
	r.waitFor(func(l line) bool { return l.is("Dropped deploy upload") })
	if r.count(stopping) != 0 {
		t.Errorf("a cut-off upload stopped the process. The log:\n%s", r.dump())
	}
	left, err := os.ReadDir(filepath.Join(r.dir, "uploads"))
	if err != nil || len(left) != 0 {
		t.Errorf("left %v behind: %v", left, err)
	}
	r.mustDeploy("second")
}

func TestDeployingTheRunningVersionChangesNothing(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.mustDeploy("same")
	pid := r.pid()
	if stderr, err := r.deploy("same"); err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	if r.pid() != pid {
		t.Fatalf("the process changed. The log:\n%s", r.dump())
	}
}

func TestDeployedVersionOutlivesAHostRestart(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.contact("park-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting) })
	r.mustDeploy("tail")
	r.stop()
	r.launch(settle) // Still names the first binary.
	r.awaitLoaded()
	r.sync()
	r.contact("park-end", true)
	r.wrote("park-tail", true)
}

func TestHostStartsEmptyAndTakesAFirstDeploy(t *testing.T) {
	t.Parallel()
	build(t)
	dir, err := os.MkdirTemp("", "oik")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	r := &rig{t: t, dir: dir, grew: make(chan struct{})}
	r.launchWithout(settle, "--simulate", devicesFile(t))
	r.waitFor(func(l line) bool { return l.is("Started host") })
	if r.waitSocket(); r.count(started) != 0 {
		t.Fatalf("a process started. The log:\n%s", r.dump())
	}
	r.mustDeploy("same")
	r.sync()
}

// waitSocket waits for the API, which opens after the log line a test can see.
func (r *rig) waitSocket() {
	r.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		if stderr, err := r.oikade(nil, "rollback"); err != nil && strings.Contains(stderr, "no earlier version") {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the API never opened. The log:\n%s", r.dump())
		}
	}
}

func TestRenamedSafetyHoldIsRefusedUntilAllowed(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.change("leak-sensor", opened, true)
	r.wrote("valve", false)
	r.sync()
	pid := r.pid()
	stderr, err := r.deploy("renamed")
	if err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	if !strings.Contains(stderr, `"leak-shutoff"`) || !strings.Contains(stderr, "--allow-safety-removal") {
		t.Fatalf("got %q", stderr)
	}
	if r.pid() != pid || r.count(stopping) != 0 {
		t.Fatalf("the process changed. The log:\n%s", r.dump())
	}
	r.mustDeploy("renamed", "--allow-safety-removal")
	if r.count(ended, "rule", "leak-shutoff", "highest", "Safety") != 1 {
		t.Fatalf("the claim stayed. The log:\n%s", r.dump())
	}
	r.stop()
	s := r.openStore()
	defer func() { _ = s.Close() }()
	names, err := (&engine.Stored[engine.HoldState]{Values: s.Namespace(engine.HoldStatesNamespace)}).Names(wire.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "leak-alarm") || slices.Contains(names, "leak-shutoff") {
		t.Fatalf("saved holds: %v", names)
	}
}

func TestRemovedRulesLeaveNothingBehind(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.change("claim-go", opened, true)
	r.sync()
	r.mustDeploy("removed")
	if r.count(ended, "rule", "claim-moment") != 1 {
		t.Fatalf("the claim stayed. The log:\n%s", r.dump())
	}
	r.stop()
	if got := r.ruleClaims("claimed"); len(got) != 0 {
		t.Fatalf("the claim outlived its rule: %+v", got)
	}
}

// A claim ends only once the version that dropped its rule has proved itself, because the last one
// may come back.
func TestAFailedVersionDoesNotEndTheClaimsOfRulesItDropped(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.change("claim-go", opened, true)
	r.sync()
	if _, err := r.deploy("crash-removed"); err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	r.waitFor(func(l line) bool { return l.is(restored) })
	r.sync()
	r.stop()
	if got := r.ruleClaims("claimed"); len(got) != 1 {
		t.Fatalf("got %+v. The log:\n%s", got, r.dump())
	}
}

// A failed version's own rules go with it, because no code is left to end what they claimed.
func TestAFailedVersionLeavesNoClaimsOfItsOwn(t *testing.T) {
	r := flowRig(t, "")
	r.launch("--settle=30s")
	r.awaitLoaded()
	bin := variant(t, "crash-told")
	done := make(chan error, 1)
	go func() {
		_, err := r.oikade(nil, "deploy", "--binary", bin)
		done <- err
	}()
	r.awaitLoaded()
	r.change("claim-go", opened, true)
	r.wrote("claimed", false)
	if err := os.WriteFile(filepath.Join(r.dir, "crash"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	r.waitFor(func(l line) bool { return l.is(ended, "rule", "claim-moment-2") })
	r.stop()
	if got := r.ruleClaims("claimed"); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// crashAfterLoading deploys a variant that crashes when told, and tells it once it has loaded.
func (r *rig) crashAfterLoading(name string) {
	r.t.Helper()
	bin := variant(r.t, name)
	done := make(chan error, 1)
	go func() {
		_, err := r.oikade(nil, "deploy", "--binary", bin)
		done <- err
	}()
	r.awaitLoaded()
	r.sync()
	if err := os.WriteFile(filepath.Join(r.dir, "crash"), nil, 0o600); err != nil {
		r.t.Fatal(err)
	}
	if err := <-done; err == nil {
		r.t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
}

// A version that failed hasn't earned ending a flow it doesn't have, whose run comes back with the
// last version.
func TestAFailedVersionLeavesAFlowItDroppedAlone(t *testing.T) {
	r := flowRig(t, "")
	r.launch("--settle=30s")
	r.awaitLoaded()
	r.contact("claim-flow-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "claiming") })
	r.sync()
	r.crashAfterLoading("crash-noflow")
	if got := r.awaitLoaded(); got != 1 {
		t.Fatalf("resumed %d runs. The log:\n%s", got, r.dump())
	}
	r.waitFor(func(l line) bool { return l.is(restored) })
	r.sync()
	r.contact("claim-end", true)
	r.wrote("claimed-flow", true)
}

// Only a deploy ends a flow, so a binary changed behind the host's back leaves its run and claims,
// and the run carries on once the flow is back.
func TestABinaryChangedWithoutADeployLeavesAFlowAlone(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.contact("claim-flow-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "claiming") })
	r.sync()
	r.stop()
	r.launchWithout("--home", variant(t, "noflow"), "--simulate", devicesFile(t), settle)
	r.awaitLoaded()
	r.sync()
	r.stop()
	if got := r.ruleClaims("claimed-flow"); !holdsClaim(got, "Rule/claiming", -1) {
		t.Fatalf("got %+v. The log:\n%s", got, r.dump())
	}
	r.launch(settle)
	if got := r.awaitLoaded(); got != 1 {
		t.Fatalf("resumed %d runs. The log:\n%s", got, r.dump())
	}
}

// A confirmed version without a flow ends what its runs left, and forgets them.
func TestADeployedVersionEndsAFlowItDropped(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.contact("claim-flow-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "claiming") })
	r.sync()
	r.mustDeploy("noflow")
	if r.count(ended, "rule", "claiming") != 1 {
		t.Fatalf("the claim stayed. The log:\n%s", r.dump())
	}
	r.stop()
	if got := r.ruleClaims("claimed-flow"); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	s := r.openStore()
	defer func() { _ = s.Close() }()
	if runs, err := s.Journal().Runs(); err != nil || len(runs) != 0 {
		t.Fatalf("got %v, %v", runs, err)
	}
}

// orphanedSafety leaves a Safety claim and latch of a rule the running binary doesn't have, as a
// binary changed in place does. A deploy that has the rule again raises no finding.
func (r *rig) orphanedSafety() {
	r.t.Helper()
	r.launch("--settle=30s")
	r.awaitLoaded()
	r.change("leak-sensor", opened, true)
	r.wrote("valve", false)
	r.sync()
	r.stop()
	r.launchWithout("--home", variant(r.t, "noleak"), "--simulate", devicesFile(r.t), "--settle=30s")
	r.awaitLoaded()
	r.sync()
}

func (r *rig) expectSafetyKept() {
	r.t.Helper()
	got := r.ruleClaims("valve")
	if !holdsClaim(got, "Rule/leak-shutoff", control.Safety) {
		r.t.Fatalf("got %+v. The log:\n%s", got, r.dump())
	}
	s := r.openStore()
	defer func() { _ = s.Close() }()
	if _, ok, err := s.Namespace(engine.HoldStatesNamespace).Get("home/leak-shutoff"); err != nil || !ok {
		r.t.Fatalf("the hold's state went: %v, %v", ok, err)
	}
}

// A Safety claim the failed version had nothing to do with isn't its to end.
func TestAFailedVersionCleansUpOnlyWhatItMade(t *testing.T) {
	r := flowRig(t, "")
	r.orphanedSafety()
	r.crashAfterLoading("crash-told")
	r.waitFor(func(l line) bool { return l.is(restored) })
	r.stop()
	r.expectSafetyKept()
}

// A deploy that was waiting when the host stopped says so, and confirms nothing that didn't settle.
func TestAHostStoppingConfirmsNoDeployWaitingOnIt(t *testing.T) {
	r := flowRig(t, "")
	r.launch("--settle=60s")
	r.awaitLoaded()
	r.change("claim-go", opened, true)
	r.wrote("claimed", false)
	r.sync()
	bin := variant(t, "removed")
	type result struct {
		stderr string
		err    error
	}
	done := make(chan result, 2)
	deploy := func() {
		stderr, err := r.oikade(nil, "deploy", "--binary", bin)
		done <- result{stderr, err}
	}
	received := func(l line) bool { return l.is("Received API request", "type", "deploy") }
	go deploy()
	r.waitFor(received)
	r.awaitLoaded()
	go deploy()
	r.waitFor(received)
	r.stop()
	for range 2 {
		if got := <-done; got.err == nil {
			t.Fatalf("a deploy says it worked: %s. The log:\n%s", got.stderr, r.dump())
		}
	}
	if r.count(deployed) != 0 || r.count(ended) != 0 {
		t.Fatalf("a version that never settled counted. The log:\n%s", r.dump())
	}
	if got := r.ruleClaims("claimed"); !holdsClaim(got, "Rule/claim-moment", -1) {
		t.Fatalf("got %+v", got)
	}
}

// A flow that became a moment of the same name has left its run behind.
func TestAFlowThatChangedKindLosesItsRuns(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.contact("park-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "parked") })
	r.sync()
	r.mustDeploy("parkmoment")
	r.stop()
	s := r.openStore()
	defer func() { _ = s.Close() }()
	if runs, err := s.Journal().Runs(); err != nil || len(runs) != 0 {
		t.Fatalf("got %v, %v. The log:\n%s", runs, err, r.dump())
	}
}

// The override covers a gone flow's Safety claim and its run, as well as a hold's.
func TestAllowedDeployEndsAGoneFlowsSafetyClaim(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.contact("fail-go", true)
	r.wrote("claimed-safety", false)
	r.sync()
	if stderr, err := r.deploy("nofail"); err == nil || !strings.Contains(stderr, `"failing"`) {
		t.Fatalf("got %v, %s", err, stderr)
	}
	r.mustDeploy("nofail", "--allow-safety-removal")
	if r.count(ended, "rule", "failing", "highest", "Safety") != 1 {
		t.Fatalf("the claim stayed. The log:\n%s", r.dump())
	}
	r.stop()
	if got := r.ruleClaims("claimed-safety"); holdsClaim(got, "Rule/failing", -1) {
		t.Fatalf("got %+v", got)
	}
	s := r.openStore()
	defer func() { _ = s.Close() }()
	if runs, err := s.Journal().Runs(); err != nil || len(runs) != 0 {
		t.Fatalf("got %v, %v", runs, err)
	}
}

// A run's journal is the record of what it claimed, so it outlasts a claim that wouldn't end.
func TestAFailedEndKeepsARunsJournal(t *testing.T) {
	r := flowRig(t, fmt.Sprintf("%s 1 drop\n", fault.HostEndRule))
	r.launch(settle)
	r.awaitLoaded()
	r.contact("claim-flow-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "claiming") })
	r.sync()
	stderr, err := r.deploy("noflow")
	if err == nil || !strings.Contains(stderr, "retry") {
		t.Fatalf("got %v, %s", err, stderr)
	}
	const finished = "Dropped flow run; its flow has gone"
	if r.count(finished) != 0 {
		t.Fatalf("the journal went first. The log:\n%s", r.dump())
	}
	if _, err := r.deploy("noflow"); err != nil {
		t.Fatal(err)
	}
	if r.count(finished) != 1 {
		t.Fatalf("the retry left the journal. The log:\n%s", r.dump())
	}
}

// A claim with nothing else on record is spared just the same.
func TestAFailedVersionSparesAClaimOfARuleTheLastOneLacks(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.change("claim-go", opened, true)
	r.wrote("claimed", false)
	r.sync()
	r.stop()
	r.launchWithout("--home", variant(t, "removed"), "--simulate", devicesFile(t), settle)
	r.awaitLoaded()
	r.sync()
	if _, err := r.deploy("crash-late"); err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	r.waitFor(func(l line) bool { return l.is(restored) })
	r.stop()
	got := r.ruleClaims("claimed")
	if !slices.ContainsFunc(got, func(c control.Claim) bool { return c.Source == "Rule/claim-moment" }) {
		t.Fatalf("got %+v. The log:\n%s", got, r.dump())
	}
}

// So is saved state with no claim.
func TestAFailedVersionSparesTheStateOfARuleTheLastOneLacks(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.sync()
	r.stop()
	r.launchWithout("--home", variant(t, "nofan"), "--simulate", devicesFile(t), settle)
	r.awaitLoaded()
	r.sync()
	if _, err := r.deploy("crash-late"); err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	r.waitFor(func(l line) bool { return l.is(restored) })
	r.stop()
	s := r.openStore()
	defer func() { _ = s.Close() }()
	if _, ok, err := s.Namespace(engine.HoldStatesNamespace).Get("home/fan"); err != nil || !ok {
		t.Fatalf("the hold's state went: %v, %v. The log:\n%s", ok, err, r.dump())
	}
}

// So is a waiting run with no claims at all.
func TestAFailedVersionSparesARunOfAFlowTheLastOneLacks(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.contact("park-go", true)
	r.waitFor(func(l line) bool { return l.is(waiting, "rule", "parked") })
	r.sync()
	r.stop()
	r.launchWithout("--home", variant(t, "nopark"), "--simulate", devicesFile(t), settle)
	r.awaitLoaded()
	r.sync()
	if _, err := r.deploy("crash-late"); err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	r.waitFor(func(l line) bool { return l.is(restored) })
	r.stop()
	s := r.openStore()
	defer func() { _ = s.Close() }()
	if runs, err := s.Journal().Runs(); err != nil || len(runs) != 1 {
		t.Fatalf("got %v, %v. The log:\n%s", runs, err, r.dump())
	}
}

// The same goes for a deploy a host died during.
func TestAnUnfinishedDeployCleansUpOnlyWhatItMade(t *testing.T) {
	r := flowRig(t, fmt.Sprintf("%s 1 kill\n", fault.HostDeployBegun))
	r.orphanedSafety()
	if _, err := r.deploy("same"); err == nil {
		t.Fatalf("the host survived its fault. The log:\n%s", r.dump())
	}
	r.died()
	r.launchWithout("--home", variant(t, "noleak"), "--simulate", devicesFile(t), settle)
	r.waitFor(func(l line) bool { return l.is("Restored the last automations version; a deploy didn't finish") })
	r.awaitLoaded()
	r.stop()
	r.expectSafetyKept()
}

// A Safety claim made once the check has read them still counts: the old version stops, the check
// runs again, and it comes back.
func TestASafetyClaimMadeAfterTheCheckStopsTheDeploy(t *testing.T) {
	r := flowRig(t, fmt.Sprintf("%s 1 wait\n", fault.HostDeployChecked))
	r.launch(settle)
	r.awaitLoaded()
	bin := variant(t, "nofail")
	type result struct {
		stderr string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		stderr, err := r.oikade(nil, "deploy", "--binary", bin)
		done <- result{stderr, err}
	}()
	r.waitFor(func(l line) bool { return l.is(checked) })
	r.contact("fail-go", true)
	r.wrote("claimed-safety", false)
	if err := os.WriteFile(filepath.Join(r.dir, "faults.go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err == nil || !strings.Contains(got.stderr, `"failing"`) {
		t.Fatalf("got %v, %s. The log:\n%s", got.err, got.stderr, r.dump())
	}
	r.waitFor(func(l line) bool { return l.is(restored) })
	r.stop()
	if claims := r.ruleClaims("claimed-safety"); len(claims) == 0 {
		t.Fatalf("the claim went. The log:\n%s", r.dump())
	}
}

// Redeploying the running version picks up a removal that failed.
func TestRedeployingRetriesAFailedRemoval(t *testing.T) {
	r := flowRig(t, fmt.Sprintf("%s 1 drop\n", fault.HostRemovals))
	r.launch(settle)
	r.awaitLoaded()
	r.change("claim-go", opened, true)
	r.sync()
	stderr, err := r.deploy("removed")
	if err == nil || !strings.Contains(stderr, "retry") {
		t.Fatalf("got %v, %s", err, stderr)
	}
	if r.count(ended) != 0 {
		t.Fatalf("the removal went ahead. The log:\n%s", r.dump())
	}
	if _, err := r.deploy("removed"); err != nil {
		t.Fatal(err)
	}
	r.waitFor(func(l line) bool { return l.is(ended, "rule", "claim-moment") })
}

// Only a deploy ends a rule, so a binary changed behind the host's back can't get past the check.
func TestABinaryChangedWithoutADeployEndsNothing(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.change("leak-sensor", opened, true)
	r.wrote("valve", false)
	r.sync()
	r.stop()
	r.launchWithout("--home", variant(t, "renamed"), "--simulate", devicesFile(t), settle)
	r.awaitLoaded()
	r.sync()
	if r.count(ended) != 0 {
		t.Fatalf("a claim ended with no deploy. The log:\n%s", r.dump())
	}
	r.stop()
	got := r.ruleClaims("valve")
	if !slices.ContainsFunc(got, func(c control.Claim) bool { return c.Source == "Rule/leak-shutoff" }) {
		t.Fatalf("got %+v", got)
	}
}

// A confirmed version's removals outlive a crash just after confirming it.
func TestRemovalsFinishAfterACrash(t *testing.T) {
	r := flowRig(t, fmt.Sprintf("%s 1 kill\n", fault.HostDeployConfirmed))
	r.launch(settle)
	r.awaitLoaded()
	r.change("claim-go", opened, true)
	r.sync()
	if _, err := r.deploy("removed"); err == nil {
		t.Fatalf("the host survived its fault. The log:\n%s", r.dump())
	}
	r.died()
	if r.count(ended) != 0 {
		t.Fatalf("the removal came first. The log:\n%s", r.dump())
	}
	r.launch(settle)
	r.waitFor(func(l line) bool { return l.is(ended, "rule", "claim-moment") })
	r.awaitLoaded()
	r.stop()
	if got := r.ruleClaims("claimed"); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// A running binary that has gone costs the way back to it and a sight of its Safety rules. You have
// to agree to that, but it needn't stop the deploy that may be there to fix it.
func TestDeployWorksWhenTheRunningBinaryHasGone(t *testing.T) {
	r := flowRig(t, "")
	home := filepath.Join(r.dir, "home")
	raw, err := os.ReadFile(variant(t, "same"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	r.launchWithout("--home", home, "--simulate", devicesFile(t), settle)
	r.awaitLoaded()
	r.sync()
	if err := os.Remove(home); err != nil {
		t.Fatal(err)
	}
	stderr, err := r.deploy("tail")
	if err == nil || !strings.Contains(stderr, "nothing to go back to") {
		t.Fatalf("got %v, %s", err, stderr)
	}
	r.mustDeploy("tail", "--allow-safety-removal")
	stderr, err = r.oikade(nil, "rollback")
	if err == nil || !strings.Contains(stderr, "no earlier version") {
		t.Fatalf("got %v, %s", err, stderr)
	}
}

func TestRollbackGoesBackAVersionAtATime(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.mustDeploy("same")
	r.mustDeploy("tail")
	r.mustDeploy("edited")
	r.mustRollback() // To tail.
	r.mustRollback() // To same.
	r.contact("park-go", true)
	r.sync()
	r.contact("park-end", true)
	r.wrote("park-out", true)
}

// holdsClaim says whether a source holds a claim, at level if that isn't negative.
func holdsClaim(claims []control.Claim, source string, level control.Level) bool {
	return slices.ContainsFunc(claims, func(c control.Claim) bool {
		return c.Source == source && (level < 0 || c.Level == level)
	})
}

// ruleClaims reads a stopped host's saved claims on a device's switch.
func (r *rig) ruleClaims(device string) []control.Claim {
	r.t.Helper()
	s := r.openStore()
	defer func() { _ = s.Close() }()
	raw, ok, err := s.Namespace(engine.ControlStatesNamespace).Get("home/" + device)
	if err != nil || !ok {
		r.t.Fatalf("no saved state for %s (%v). The log:\n%s", device, err, r.dump())
	}
	var st engine.ControlState
	if err := json.Unmarshal(raw, &st); err != nil {
		r.t.Fatal(err)
	}
	return st["on"].Claims
}

func TestDeployBuildsAModuleAndChecksItsFlows(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.sync()
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	module, err := os.MkdirTemp("", "oikmod")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(module) })
	if err := writeModule(repo, module); err != nil {
		t.Fatal(err)
	}
	env := []string{"GOFLAGS=-mod=mod", "GOPROXY=off"}
	bad := filepath.Join(module, "bad.go")
	source := `package main

import (
	"time"

	"github.com/harrymitchinson/oikade/framework"
)

var _ = func(r *framework.FlowRun) error {
	_ = time.Now()
	return nil
}
`
	if err := os.WriteFile(bad, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, err := r.oikade(env, "deploy", "--module", module)
	if err == nil || !strings.Contains(stderr, "time.Now") {
		t.Fatalf("got %v: %s", err, stderr)
	}
	if r.count(stopping) != 0 {
		t.Fatalf("the process stopped. The log:\n%s", r.dump())
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if stderr, err := r.oikade(env, "deploy", "--module", module); err != nil {
		t.Fatalf("%v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
	r.sync()
}

// emulator returns how to run a linux/arm64 binary here.
func emulator() ([]string, bool) {
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		return nil, true
	}
	for _, name := range []string{"qemu-aarch64-static", "qemu-aarch64"} {
		if path, err := exec.LookPath(name); err == nil {
			return []string{path}, true
		}
	}
	return nil, false
}

func TestCrossCompiledBuildRunsItsCheck(t *testing.T) {
	t.Parallel()
	prefix, ok := emulator()
	if !ok {
		if os.Getenv("OIKADE_REQUIRE_EMULATOR") != "" {
			t.Fatal("no way to run a linux/arm64 binary here")
		}
		t.Skip("needs an arm64 machine or qemu-user")
	}
	build(t)
	path := filepath.Join(buildDir, "home-arm64")
	env := []string{"GOFLAGS=-mod=mod", "GOPROXY=off", "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0"}
	if err := run(filepath.Join(buildDir, "module"), env, "build", "-o", path, "."); err != nil {
		t.Fatal(err)
	}
	argv := append(prefix, path, wire.RoleCheck)
	raw, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("%v: %s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if m.Protocol != wire.Version || len(m.Rules) == 0 {
		t.Fatalf("got %+v", m)
	}
	t.Log("Ran the arm64 check through", argv[0])
}
