package host

import (
	"bytes"
	"encoding/json"
	"errors"
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
	"github.com/harrymitchinson/oikade/internal/host/wire"
)

const (
	deployed = "Deployed automations version"
	restored = "Restored the last automations version"
	stopping = "Stopping automations process for a new version"
	ended    = "Ended the claims of a removed rule"
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
	args = append(args[:1:1], append([]string{"--data-dir", r.dir}, args[1:]...)...)
	cmd := exec.Command(hostBin, args...)
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
			r.change("bathroom-sensor", "sensor.relative-humidity", 80)
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
	devices, err := filepath.Abs(filepath.Join("testdata", "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, dir: dir, grew: make(chan struct{})}
	r.launchWithout(settle, "--simulate", devices)
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
	return st["switch.on"].Claims
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
