package host

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/fault"
)

// Your values, as the fixture's variants "vars" and "vars-more" use them (OIK-365). Each is told
// apart in the log only by a digest, as the fixture never logs a value.
const (
	tokenVar = "FIXTURE_TOKEN"
	extraVar = "FIXTURE_EXTRA"
	readVar  = "Read var"
)

// digest is how the fixture reports a value it was given.
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

// envFile writes values as a .env, in a directory of its own.
func envFile(t *testing.T, values map[string]string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(values)) {
		fmt.Fprintf(&b, "%s=%q\n", name, values[name])
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cli runs oikade against the rig's host, with env added to its environment, and returns all it
// printed. It runs in a directory with no .env, so that only what a test gives it is sent.
func (r *rig) cli(env []string, args ...string) (string, error) {
	r.t.Helper()
	args = append(args[:1:1], append([]string{"--data-dir", r.dir}, args[1:]...)...)
	cmd := exec.Command(ctlBin, args...)
	cmd.Dir = r.t.TempDir()
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// deployValues deploys a variant with values, from an --env-file.
func (r *rig) deployValues(name string, values map[string]string, env ...string) (string, error) {
	r.t.Helper()
	return r.cli(env, "deploy", "--binary", variant(r.t, name), "--env-file", envFile(r.t, values))
}

func (r *rig) mustDeployValues(name string, values map[string]string, env ...string) string {
	r.t.Helper()
	out, err := r.deployValues(name, values, env...)
	if err != nil {
		r.t.Fatalf("%v: %s\nThe log:\n%s", err, out, r.dump())
	}
	return out
}

// readsVar counts the processes of a variant with role that were given value as tokenVar.
func (r *rig) readsVar(role, name, value string) int {
	return r.count(readVar, "role", role, "variant", name, "sha", digest(value))
}

// awaitVar waits until the processes of both roles of a variant have each been given value n times.
func (r *rig) awaitVar(n int, name, value string) {
	r.t.Helper()
	for _, role := range []string{"automations", "plugin"} {
		r.awaitCount(n, readVar, "role", role, "variant", name, "sha", digest(value))
	}
}

// noValueIn fails if any of values is in what.
func noValueIn(t *testing.T, where, what string, values ...string) {
	t.Helper()
	for _, v := range values {
		if strings.Contains(what, v) {
			t.Errorf("%s has a value:\n%s", where, what)
		}
	}
}

func TestAPluginAndYourAutomationsAreGivenTheDeployedValues(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.sync()
	first, second := "first-correct-horse", "second-battery-staple"
	out := r.mustDeployValues("vars", map[string]string{tokenVar: first})
	if !strings.Contains(out, "Deployed version ") || !strings.Contains(out, ", with 1 value from ") {
		t.Errorf("the deploy said %q", out)
	}
	r.awaitVar(1, "vars", first)

	// Only the value changes: the processes start again, with it.
	pid := r.pid()
	stopped := r.count(stopping)
	out = r.mustDeployValues("vars", map[string]string{tokenVar: second})
	if !strings.Contains(out, "again, with "+tokenVar+" changed") {
		t.Errorf("the deploy said %q", out)
	}
	r.awaitVar(1, "vars", second)
	if r.pid() == pid || r.count(stopping) != stopped+1 {
		t.Errorf("the process didn't start again. The log:\n%s", r.dump())
	}

	// The same again changes nothing.
	pid = r.pid()
	out = r.mustDeployValues("vars", map[string]string{tokenVar: second})
	if !strings.Contains(out, "Already running version") {
		t.Errorf("the deploy said %q", out)
	}
	if r.pid() != pid || r.readsVar("plugin", "vars", second) != 1 {
		t.Errorf("an unchanged deploy started something again. The log:\n%s", r.dump())
	}

	// No value is in the log, an answer, or --json.
	status, err := r.cli(nil, "status", "--json")
	if err != nil {
		t.Fatalf("%v: %s", err, status)
	}
	noValueIn(t, "status --json", status, first, second)
	noValueIn(t, "the host's log", r.dump(), first, second)
	if info, err := os.Stat(filepath.Join(r.dir, "vars.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the values file: %v, %v", info, err)
	}
}

func TestAMissingValueStopsADeployBeforeItsUpload(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.sync()
	out, err := r.cli(nil, "deploy", "--binary", variant(t, "vars"))
	if err == nil || !strings.Contains(out, "Error: "+tokenVar+" isn't set") ||
		!strings.Contains(out, "Hint: add it to .env, or set it in the environment") {
		t.Fatalf("got %v: %s", err, out)
	}
	if n := r.count("Received API request", "type", "deploy"); n != 0 {
		t.Errorf("the deploy reached the host. The log:\n%s", r.dump())
	}

	// The environment gives what the file lacks, and wins over what it has.
	fromEnv := "from-the-environment"
	r.mustDeployValues("vars", map[string]string{tokenVar: "from-the-file"}, tokenVar+"="+fromEnv)
	r.awaitVar(1, "vars", fromEnv)
	if r.readsVar("automations", "vars", "from-the-file") != 0 {
		t.Errorf("the file's value won. The log:\n%s", r.dump())
	}
}

func TestARollbackKeepsTheValuesAndRefusesAVersionWhoseNamesTheyLack(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.sync()
	r.mustDeployValues("vars", map[string]string{tokenVar: "old"})
	r.awaitVar(1, "vars", "old")
	r.mustDeployValues("vars-more", map[string]string{tokenVar: "new", extraVar: "x"})
	r.awaitVar(1, "vars-more", "new")
	// The version before comes back with today's values, so an old password never does.
	if out, err := r.cli(nil, "rollback"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	r.awaitVar(1, "vars", "new")

	// Now the values lack FIXTURE_EXTRA, which the version before uses.
	r.mustDeployValues("vars-more", map[string]string{tokenVar: "new", extraVar: "x"})
	r.mustDeployValues("vars", map[string]string{tokenVar: "new"})
	r.awaitVar(2, "vars", "new")
	pid := r.pid()
	out, err := r.cli(nil, "rollback")
	if err == nil || !strings.Contains(out, "Error: the version uses "+extraVar+", which isn't among the host's values") ||
		!strings.Contains(out, "Hint: oikade rollback --env-file FILE") {
		t.Fatalf("got %v: %s", err, out)
	}
	if r.pid() != pid {
		t.Errorf("the refused rollback stopped the process. The log:\n%s", r.dump())
	}
	// --env-file sends values of its own with it.
	out, err = r.cli(nil, "rollback", "--env-file", envFile(t, map[string]string{tokenVar: "given", extraVar: "y"}))
	if err != nil || !strings.Contains(out, "Rolled back to version ") || !strings.Contains(out, ", with 2 values from ") {
		t.Fatalf("got %v: %s", err, out)
	}
	r.awaitVar(1, "vars-more", "given")
}

// A deploy's values reach disk in the same write as its version. A host killed anywhere from there
// to its answer comes back with a binary and values that go together: the old version with the old
// values until the new one is confirmed, and the new with the new from then on.
func TestKilledAsADeploysValuesSaveKeepsThemWithTheirVersion(t *testing.T) {
	t.Parallel()
	for point, confirmed := range map[string]bool{
		fault.HostDeployBegun: false, fault.HostDeploySettled: false, fault.HostDeployConfirmed: true,
	} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			r := faultRig(t)
			r.launchAttached()
			r.mustDeployValues("vars", map[string]string{tokenVar: "old"})
			r.awaitVar(1, "vars", "old")
			r.plan(point)
			if out, err := r.deployValues("vars-more", map[string]string{tokenVar: "new", extraVar: "x"}); err == nil {
				t.Fatalf("the deploy was answered: %s. The log:\n%s", out, r.dump())
			}
			r.died()
			want, value := "vars", "old"
			if confirmed {
				want, value = "vars-more", "new"
			}
			before := map[string]int{}
			for _, role := range []string{"automations", "plugin"} {
				before[role] = r.readsVar(role, want, value)
			}
			r.launchAttached()
			for role, n := range before {
				r.awaitCount(n+1, readVar, "role", role, "variant", want, "sha", digest(value))
			}
			r.sync()
			// Nothing ran the other binary with these values, or this one with the others.
			for _, mixed := range [][2]string{{"vars", "new"}, {"vars-more", "old"}} {
				if n := r.readsVar("automations", mixed[0], mixed[1]); n != 0 {
					t.Errorf("%s ran with %q. The log:\n%s", mixed[0], mixed[1], r.dump())
				}
			}
		})
	}
}
