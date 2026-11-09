// Package cli drives the oikade and oikade-host binaries as a person would, from an empty directory.
package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/sun"
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

// oikade builds the CLI, once a test.
func oikade(t *testing.T) string {
	t.Helper()
	return binary(t, "oikade")
}

// binary builds one of cmd's binaries.
func binary(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	args := []string{"build", "-o", path}
	// make cover-binaries measures what these binaries run.
	if os.Getenv("OIKADE_COVER") != "" {
		args = append(args, "-cover", "-coverpkg=github.com/harrymitchinson/oikade/...")
	}
	cmd := exec.Command("go", append(args, "./cmd/"+name)...)
	cmd.Dir = repo(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", name, err, out)
	}
	return path
}

// run runs a binary in dir, with GOPROXY off to prove a home needs only Go and the binary.
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
	replaceIn(t, filepath.Join(dir, "main.go"), "PorchLight.On", "PorchLigth.On")
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, "undefined: PorchLigth") {
		t.Errorf("oikade check said:\n%s", out)
	}
}

// A capability the device doesn't have, or a value of the wrong type, doesn't compile.
func TestCheckFailsOnACapabilityTheDeviceHasnt(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	replaceIn(t, filepath.Join(dir, "main.go"), "PorchLight.On", "PorchLight.Occupied")
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, "PorchLight.Occupied undefined") {
		t.Errorf("oikade check said:\n%s", out)
	}
}

func TestCheckFailsOnAMistakeOnlyTheHomeCanSee(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	replaceIn(t, filepath.Join(dir, "main.go"), "When: func", "OnDelay: -time.Minute,\n\t\t\tWhen: func")
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, "onDelay must not be negative") {
		t.Errorf("oikade check said:\n%s", out)
	}
}

func TestCheckFailsOnATimeThatIsntOne(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	replaceIn(t, filepath.Join(dir, "main.go"), "OffDelay: 5 * time.Minute,\n\t})", `OffDelay: 5 * time.Minute,
	}, framework.Moment{
		Name: "lights-out", At: []framework.Time{framework.Clock("25:00")},
		Do: []framework.Action{PorchLight.On.To(false)},
	})`)
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, `rule lights-out: timers[0]: time "25:00" is not HH:MM`) {
		t.Errorf("oikade check said:\n%s", out)
	}
}

// duskMoment is a moment for oikade init's home, after its hold.
const duskMoment = `OffDelay: 5 * time.Minute,
	}, framework.Moment{
		Name: "porch-at-dusk", At: []framework.Time{framework.Sunset(-20 * time.Minute)},
		Do: []framework.Action{PorchLight.On.To(true)},
	})`

func TestCheckFailsOnASunsetWithNoLocation(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	replaceIn(t, filepath.Join(dir, "main.go"), "OffDelay: 5 * time.Minute,\n\t})", duskMoment)
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, "rule porch-at-dusk: a sunrise or sunset needs the home's location") {
		t.Errorf("oikade check said:\n%s", out)
	}
}

func TestCheckFailsOnANilPart(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	replaceIn(t, filepath.Join(dir, "main.go"), "framework.Options{},", "framework.Options{}, nil,")
	out, err := run(t, bin, dir, "", "check")
	if err == nil {
		t.Fatalf("oikade check passed:\n%s", out)
	}
	if !strings.Contains(out, "a nil part, at parts[0]") {
		t.Errorf("oikade check said:\n%s", out)
	}
}

func TestDevFiresAtSunsetForTheHomesLocation(t *testing.T) {
	bin := oikade(t)
	dir := newHome(t, bin)
	main := filepath.Join(dir, "main.go")
	replaceIn(t, main, "OffDelay: 5 * time.Minute,\n\t})", duskMoment)
	replaceIn(t, main, "framework.Options{}", "framework.Options{Latitude: 51.48, Longitude: -0.01}")
	// oikade dev starts now, in this machine's zone, so the next sunset is within a day.
	before := time.Now()
	out, err := run(t, bin, dir, "advance 25h\nquit\n", "dev")
	if err != nil {
		t.Fatalf("oikade dev: %v\n%s", err, out)
	}
	start := devStart(t, out, before)
	site := sun.Site{Latitude: 51.48, Longitude: -0.01}
	var want time.Time
	for day := start; want.IsZero(); day = day.AddDate(0, 0, 1) {
		at, ok := site.Time(sun.Sunset, day.Year(), day.Month(), day.Day(), time.Local)
		// The banner gives dev's start to the minute, so a time within that minute may be before or
		// after it: the next day's is the first one certain to come.
		if ok && at.Add(-20*time.Minute).Sub(start) >= time.Minute {
			want = at.Add(-20 * time.Minute)
		}
	}
	line := want.Format("Mon 15:04") + " Wrote porch-light on = true"
	if !strings.Contains(out, line) {
		t.Errorf("no %q in:\n%s", line, out)
	}
}

// devStart reads when oikade dev's clock started from its banner, which names the day and the
// minute: the first such minute from before, the moment before it ran.
func devStart(t *testing.T, out string, before time.Time) time.Time {
	t.Helper()
	m := regexp.MustCompile(`from (\w{3} \d{2}:\d{2})\.`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no start time in:\n%s", out)
	}
	for at := before.Truncate(time.Minute); at.Before(before.Add(48 * time.Hour)); at = at.Add(time.Minute) {
		if at.Format("Mon 15:04") == m[1] {
			return at
		}
	}
	t.Fatalf("start %q isn't after %v", m[1], before)
	return time.Time{}
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
		"set porch-sensor occupied true",
		"status",
		"set porch-sensor occupied false",
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
		"Wrote porch-light on = true", "Wrote porch-light on = false", "that isn't a command",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// Hosts are reached over their API: there's no SSH, and no installer, left to call. Running a host,
// and trusting a client on its machine, are oikade-host's.
func TestRemovedCommandsAreUnknown(t *testing.T) {
	bin := oikade(t)
	dir := t.TempDir()
	for _, args := range [][]string{{"install"}, {"upgrade"}, {"proxy"}, {"--host", "pi", "status"}, {"host"}} {
		out, err := run(t, bin, dir, "", args...)
		if err == nil || !strings.Contains(out, "unknown") {
			t.Errorf("oikade %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	for _, help := range []string{"help", "-h", "--help"} {
		if out, err := run(t, bin, dir, "", help); err != nil || !strings.HasPrefix(out, "Usage:") {
			t.Errorf("oikade %s: %v: %s", help, err, out)
		}
	}
}

// oikade runs on a laptop, so it carries nothing of the host: not the host, not Matter.
func TestOikadeLinksNothingOfTheHost(t *testing.T) {
	deps := func(pkg string) []string {
		cmd := exec.Command("go", "list", "-deps", pkg)
		cmd.Dir = repo(t)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		return strings.Fields(string(out))
	}
	const module = "github.com/harrymitchinson/oikade/"
	for _, dep := range deps("./cmd/oikade") {
		if dep == module+"internal/host" || strings.HasPrefix(dep, module+"internal/matter") {
			t.Errorf("oikade links %s", dep)
		}
	}
	for _, dep := range deps("./cmd/oikade-host") {
		if dep == module+"internal/flowcheck" || dep == module+"internal/simhome" {
			t.Errorf("oikade-host links %s, which only oikade's commands need", dep)
		}
	}
}

func TestOikadeHostHasOnlyItsOwnCommands(t *testing.T) {
	bin := binary(t, "oikade-host")
	dir := t.TempDir()
	if out, err := run(t, bin, dir, "", "version"); err != nil || strings.TrimSpace(out) == "" {
		t.Errorf("oikade-host version: %v: %q", err, out)
	}
	for _, help := range []string{"help", "-h", "--help"} {
		if out, err := run(t, bin, dir, "", help); err != nil || !strings.HasPrefix(out, "Usage:") {
			t.Errorf("oikade-host %s: %v: %s", help, err, out)
		}
	}
	if out, err := run(t, bin, dir, "", "--version"); err != nil || strings.TrimSpace(out) == "" {
		t.Errorf("oikade-host --version: %v: %q", err, out)
	}
	// A command after a flag is refused, rather than ignored while a host starts on the directory.
	out, err := run(t, bin, dir, "", "--data-dir", dir, "client", "add", "laptop")
	if err == nil || !strings.Contains(out, "comes first") {
		t.Errorf("oikade-host with a command after a flag: %v: %s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("it started a host, which left %v", entries)
	}
	for _, args := range [][]string{{"deploy"}, {"status"}, {"init"}} {
		out, err := run(t, bin, dir, "", args...)
		if err == nil || !strings.Contains(out, "unknown command") {
			t.Errorf("oikade-host %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	// Adding a client needs the host's socket, so with no host there it says so rather than pass.
	out, err = run(t, bin, dir, "", "client", "add", "laptop", "--data-dir", dir)
	if err == nil {
		t.Errorf("oikade-host client add with no host: %s", out)
	}
	cli := oikade(t)
	out, err = run(t, cli, dir, "", "client", "add", "laptop")
	if err == nil || !strings.Contains(out, "oikade-host client add") {
		t.Errorf("oikade client add should point at oikade-host: %v: %s", err, out)
	}
}
