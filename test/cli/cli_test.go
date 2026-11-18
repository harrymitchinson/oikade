// Package cli drives the oikade and oikade-host binaries as a person would, from an empty directory.
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/sun"
	"github.com/harrymitchinson/oikade/test/internal/childrace"
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

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

// TestMain removes the binaries the tests built, and fails the tests if one of them reported a data
// race.
func TestMain(m *testing.M) {
	code := childrace.Main(m)
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// oikade is the CLI.
func oikade(t *testing.T) string {
	t.Helper()
	return binary(t, "oikade")
}

// binary is one of cmd's binaries. The first test to ask builds oikade and oikade-host, with one go
// build, for every test.
func binary(t *testing.T, name string) string {
	t.Helper()
	dir := repo(t)
	buildOnce.Do(func() {
		if binDir, buildErr = os.MkdirTemp("", "oikcli"); buildErr != nil { //nolint:usetesting // every test shares it
			return
		}
		args := []string{"build", "-o", binDir + string(filepath.Separator)}
		// Under the race detector, so are the binaries.
		if childrace.Enabled {
			args = append(args, "-race")
		}
		// make cover-binaries measures what these binaries run.
		if os.Getenv("OIKADE_COVER") != "" {
			args = append(args, "-cover", "-coverpkg=github.com/harrymitchinson/oikade/...")
		}
		cmd := exec.Command("go", append(args, "./cmd/oikade", "./cmd/oikade-host")...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building oikade and oikade-host: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(binDir, name)
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
	t.Parallel()
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
	t.Parallel()
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

// A home with a mistake in it fails oikade check, which says what the mistake is.
func TestCheckFailsOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// mistake puts it in a new home.
		mistake func(t *testing.T, dir string)
		want    []string
	}{
		{"a misspelt device", replacing("PorchLight.On", "PorchLigth.On"), []string{"undefined: PorchLigth"}},
		// A capability the device doesn't have, or a value of the wrong type, doesn't compile.
		{"a capability the device hasn't", replacing("PorchLight.On", "PorchLight.Occupied"),
			[]string{"PorchLight.Occupied undefined"}},
		{"a mistake only the home can see", replacing(lingers, lingers+".Delay(-time.Minute)"),
			[]string{"Delay -1m0s must not be negative"}},
		{"a time that isn't one", replacing(returns,
			withFlow(`home.Flow("lights-out", func(r *oikade.FlowRun) { r.Set(PorchLight.On.To(false)) }).`+
				`At(oikade.Clock("25:00"))`)),
			[]string{`rule lights-out: timers[0]: time "25:00" is not HH:MM`}},
		{"a sunset with no location", replacing(returns, duskFlow),
			[]string{"rule porch-at-dusk: a sunrise or sunset needs the home's location"}},
		{"a flow that wouldn't replay", writing("flow.go", lateFlow), []string{"time.Now"}},
		{"a result that isn't used", writing("discard.go", discarded),
			[]string{"the result of For isn't used", "the result of To isn't used"}},
		{"an option given twice", replacing(lingers, lingers+".Linger(time.Minute)"),
			[]string{`decision porch-light on: Case "motion": Linger is given twice`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := oikade(t)
			dir := newHome(t, bin)
			tc.mistake(t, dir)
			out, err := run(t, bin, dir, "", "check")
			if err == nil {
				t.Fatalf("oikade check passed:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("oikade check didn't say %q:\n%s", want, out)
				}
			}
		})
	}
}

// replacing makes the first from in a home's main.go to.
func replacing(from, to string) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		replaceIn(t, filepath.Join(dir, "main.go"), from, to)
	}
}

// writing adds a file of code to a home.
func writing(name, code string) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(code), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// lateFlow reads the clock in a flow, which a replay would read again and get another answer.
const lateFlow = `package main

import (
	"time"

	"github.com/harrymitchinson/oikade"
)

func late(r *oikade.FlowRun) {
	_ = time.Now()
}
`

// discarded makes a Set and a Bool's change, and does nothing with either.
const discarded = `package main

import (
	"time"

	"github.com/harrymitchinson/oikade"
)

func discard(s oikade.Set) {
	s.For(time.Minute)
}

func thrownAway(g oikade.Bool) {
	g.To(true)
}
`

// duskFlow is a flow for oikade init's home.
var duskFlow = withFlow(`home.Flow("porch-at-dusk", func(r *oikade.FlowRun) {` +
	`r.Set(PorchLight.On.To(true).For(time.Hour)) }).At(oikade.Sunset(-20*time.Minute))`)

// returns is where oikade init's home is returned, before which a test adds a flow, and lingers its
// case's Linger, where a test adds an option.
const (
	returns = "\treturn home\n"
	lingers = "Linger(5 * time.Minute)"
)

// withFlow is code, which adds a flow to oikade init's home, before it's returned.
func withFlow(code string) string { return "\t" + code + "\n" + returns }

func TestDevFiresAtSunsetForTheHomesLocation(t *testing.T) {
	t.Parallel()
	bin := oikade(t)
	dir := newHome(t, bin)
	main := filepath.Join(dir, "main.go")
	replaceIn(t, main, returns, duskFlow)
	// The flow's Set ranks at the decision's Flows line.
	replaceIn(t, main, lingers, lingers+".Flows()")
	replaceIn(t, main, "oikade.Options{}", "oikade.Options{Location: oikade.NewLocation(51.48, -0.01)}")
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

func TestDevShowsAHoldStartingAndEnding(t *testing.T) {
	t.Parallel()
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
	started := strings.Index(out, "case/porch-light/on/motion applies")
	ended := strings.Index(out, "case/porch-light/on/motion no longer applies")
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
func TestHelpPrintsTheUsage(t *testing.T) {
	t.Parallel()
	bin := oikade(t)
	dir := t.TempDir()
	for _, help := range []string{"help", "-h", "--help"} {
		if out, err := run(t, bin, dir, "", help); err != nil || !strings.HasPrefix(out, "Usage:") {
			t.Errorf("oikade %s: %v: %s", help, err, out)
		}
	}
	// A command's own help, and a slip, exit as a person and a script expect: 0, and 2 with that
	// command's usage alone.
	if out, err := run(t, bin, dir, "", "deploy", "--help"); err != nil ||
		!strings.Contains(out, "--module DIR") || strings.Contains(out, "Usage of") {
		t.Errorf("oikade deploy --help: %v: %s", err, out)
	}
	for args, want := range map[string]string{
		"deploy --bogus": "Error: oikade deploy has no flag --bogus\nUsage:\n  oikade deploy ",
		"statu":          "Error: unknown command statu\nHint: did you mean status?",
	} {
		out, err := run(t, bin, dir, "", strings.Fields(args)...)
		if code := exitCode(err); code != 2 || !strings.HasPrefix(out, want) || strings.Contains(out, "oikade rollback") {
			t.Errorf("oikade %s exits %d:\n%s", args, code, out)
		}
	}
}

// exitCode is the code a command that ran exited with.
func exitCode(err error) int {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// oikade runs on a laptop, so it carries nothing of the host: not the host, not Matter.
func TestOikadeLinksNothingOfTheHost(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	bin := binary(t, "oikade-host")
	dir := t.TempDir()
	if out, err := run(t, bin, dir, "", "version"); err != nil || strings.TrimSpace(out) == "" {
		t.Errorf("oikade-host version: %v: %q", err, out)
	}
	for _, help := range []string{"help", "-h", "--help"} {
		if out, err := run(t, bin, dir, "", help); err != nil || !strings.HasPrefix(out, "oikade-host: Run the host") ||
			!strings.Contains(out, "--data-dir DIR") || !strings.Contains(out, "client add") {
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
	// A flag about the home rather than the machine has moved into the home's code, and says where.
	for args, want := range map[string]string{
		"--matter-name=Cottage":       "oikade.Options",
		"--broker mqtt://broker:1883": "zigbee2mqtt.Config",
	} {
		out, err := run(t, bin, dir, "", append([]string{"--data-dir", dir}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(out, want) {
			t.Errorf("oikade-host %s: %v: %s", args, err, out)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a removed flag started a host, which left %v", entries)
	}
	for _, args := range [][]string{{"deploy"}, {"status"}, {"init"}} {
		out, err := run(t, bin, dir, "", args...)
		if err == nil || !strings.Contains(out, "unknown command") {
			t.Errorf("oikade-host %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	// Installing writes system files, so it asks for sudo rather than half do it.
	if os.Geteuid() != 0 {
		out, err := run(t, bin, dir, "", "install")
		if err == nil || !strings.Contains(out, "run it with sudo") {
			t.Errorf("oikade-host install as a user: %v: %s", err, out)
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
