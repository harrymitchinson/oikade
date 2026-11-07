// Package cli drives the oikade binary as a person would, from an empty directory.
package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repo is this checkout, which a new home builds against so that nothing needs fetching.
func repo(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("couldn't find the repo")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// oikade builds the binary, once a test.
func oikade(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oikade")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/oikade")
	cmd.Dir = repo(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building oikade: %v\n%s", err, out)
	}
	return path
}

// run runs oikade in dir, with GOPROXY off to prove a home needs only Go and the binary.
func run(t *testing.T, bin, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=mod")
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func newHome(t *testing.T, bin string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := run(t, bin, dir, "", "init", "--source", repo(t)); err != nil {
		t.Fatalf("oikade init: %v\n%s", err, out)
	}
	return dir
}

func TestInitThenTestAndCheckPass(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	for _, want := range []string{"go.mod", "main.go", "home_test.go", "devices.go", ".github/workflows/deploy.yml"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Error(err)
		}
	}
	workflow, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// CI deploys over the host's API with a token, and has no way in over SSH.
	for _, want := range []string{"secrets.OIKADE_TOKEN", `oikade login "$OIKADE_ADDRESS" --pin "$OIKADE_PIN"`} {
		if !strings.Contains(string(workflow), want) {
			t.Errorf("the workflow has no %s:\n%s", want, workflow)
		}
	}
	if strings.Contains(string(workflow), "ssh") || strings.Contains(string(workflow), "--host") {
		t.Errorf("the workflow still uses SSH:\n%s", workflow)
	}
	if out, err := run(t, bin, dir, "", "test"); err != nil {
		t.Fatalf("oikade test: %v\n%s", err, out)
	}
	out, err := run(t, bin, dir, "", "check")
	if err != nil {
		t.Fatalf("oikade check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 rules") {
		t.Errorf("oikade check said %q", out)
	}
	if out, err := run(t, bin, dir, "", "init", "--source", repo(t)); err == nil {
		t.Errorf("oikade init ran over a home: %s", out)
	}
}

func replaceIn(t *testing.T, path, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(from)) {
		t.Fatalf("%s has no %q", path, from)
	}
	if err := os.WriteFile(path, bytes.Replace(raw, []byte(from), []byte(to), 1), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInitLeavesTheDirectoryAsItFoundIt(t *testing.T) {
	bin := oikade(t)
	dir := t.TempDir()
	mine := filepath.Join(dir, ".github", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, bin, dir, "", "init", "--source", t.TempDir()); err == nil {
		t.Fatalf("init from no checkout passed:\n%s", out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".github" {
		t.Errorf("left behind %v", entries)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Error(err)
	}
	if out, err := run(t, bin, dir, "", "init", "--source", repo(t)); err != nil {
		t.Fatalf("a second try: %v\n%s", err, out)
	}
	if out, err := run(t, bin, dir, "", "init", "--source", repo(t)); err == nil {
		t.Fatalf("init over a home passed:\n%s", out)
	}
}

func TestCheckFailsOnAMisspeltDevice(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	replaceIn(t, filepath.Join(dir, "main.go"), "Device:     PorchLight", "Device:     PorchLigth")
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, "undefined: PorchLigth") {
		t.Errorf("oikade check said:\n%s", out)
	}
}

func TestCheckFailsOnAMistakeOnlyTheHomeCanSee(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	replaceIn(t, filepath.Join(dir, "main.go"), `Capability: "light.on"`, `Capability: "light.onn"`)
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, "light.onn") {
		t.Errorf("oikade check said:\n%s", out)
	}
}

func TestCheckFailsOnAFlowThatWouldntReplay(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	flow := `package main

import (
	"time"

	"github.com/harrymitchinson/oikade/framework"
)

func late(r *framework.FlowRun) error {
	_ = time.Now()
	return nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "flow.go"), []byte(flow), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, "time.Now") {
		t.Errorf("oikade check said:\n%s", out)
	}
}

func TestDevShowsAHoldStartingAndEnding(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	script := strings.Join([]string{
		"set porch-sensor sensor.occupancy-detected true",
		"status",
		"set porch-sensor sensor.occupancy-detected false",
		"advance 6m",
		"bogus",
		"quit",
	}, "\n") + "\n"
	out, err := run(t, bin, dir, script, "dev")
	if err != nil {
		t.Fatalf("oikade dev: %v\n%s", err, out)
	}
	started := strings.Index(out, "Hold porch-light started")
	ended := strings.Index(out, "Hold porch-light ended")
	if started < 0 || ended < started {
		t.Errorf("no hold starting then ending in:\n%s", out)
	}
	for _, want := range []string{
		"Wrote porch-light light.on = true", "Wrote porch-light light.on = false", "that isn't a command",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// Hosts are reached over their API: there's no SSH, and no installer, left to call.
func TestRemovedCommandsAreUnknown(t *testing.T) {
	bin := oikade(t)
	dir := t.TempDir()
	for _, args := range [][]string{{"install"}, {"upgrade"}, {"proxy"}, {"--host", "pi", "status"}} {
		out, err := run(t, bin, dir, "", args...)
		if err == nil || !strings.Contains(out, "unknown") {
			t.Errorf("oikade %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
}
