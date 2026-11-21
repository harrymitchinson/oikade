package host

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/logging"
)

func TestTheValuesAreSavedWithTheirVersionAndAnAbortBringsBackTheLast(t *testing.T) {
	dir := t.TempDir()
	v, closeStore := openTestVersions(t, dir)
	first, second := binary(t, v, "first"), binary(t, v, "second")
	before := map[string]string{"TOSHIBA_USER": "someone"}
	if err := v.begin(version{ID: first}, nil, before); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	digest := v.vars().digest
	after := map[string]string{"TOSHIBA_USER": "someone", "TOSHIBA_PASSWORD": "new"}
	if err := v.begin(version{ID: second}, nil, after); err != nil {
		t.Fatal(err)
	}
	if got := v.vars(); !maps.Equal(got.values, after) || got.digest == digest {
		t.Errorf("the processes starting get %q", got.values)
	}

	// A crash here leaves the new values beside the version they came with, and the old ones to go
	// back to.
	closeStore()
	v, closeStore = openTestVersions(t, dir)
	defer closeStore()
	if !v.state.Pending || !maps.Equal(v.state.Vars, after) || !maps.Equal(v.state.VarsBefore, before) {
		t.Fatalf("after a restart: %+v", v.state)
	}
	if got := v.vars(); !maps.Equal(got.values, after) {
		t.Errorf("after a restart, the processes starting get %q", got.values)
	}
	if err := v.abort(); err != nil {
		t.Fatal(err)
	}
	if got := ids(v); !slices.Equal(got, []string{first}) || !maps.Equal(v.state.Vars, before) || v.state.VarsBefore != nil {
		t.Fatalf("after the abort: %+v", v.state)
	}
	if got := v.vars(); !maps.Equal(got.values, before) || got.digest != digest {
		t.Errorf("after the abort, the processes starting get %q", got.values)
	}

	// Confirming keeps the values, and dealing with removals too.
	if err := v.begin(version{ID: second}, nil, after); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	if err := v.removalsDone(); err != nil {
		t.Fatal(err)
	}
	closeStore()
	v, closeStore = openTestVersions(t, dir)
	defer closeStore()
	if !maps.Equal(v.state.Vars, after) || v.state.VarsBefore != nil {
		t.Errorf("after confirming: %+v", v.state)
	}
}

// checks is a binary whose check mode says it uses vars.
func checks(t *testing.T, vars ...string) string {
	t.Helper()
	m := manifest()
	m.Vars = vars
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return script(t, fmt.Sprintf("echo '%s'", raw))
}

func TestAVersionThatUsesAValueTheDeployLacksIsRefused(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	d := &deployer{log: logging.Discard(), versions: v}
	resp := d.deploy(t.Context(), checks(t, "TOSHIBA_PASSWORD", "TOSHIBA_USER", "TAHOMA_PIN"),
		map[string]string{"TOSHIBA_USER": "someone"}, false)
	want := "the version uses TOSHIBA_PASSWORD and TAHOMA_PIN, which aren't among the deploy's values"
	if resp.Error != want || !slices.Equal(resp.MissingVars, []string{"TOSHIBA_PASSWORD", "TAHOMA_PIN"}) {
		t.Errorf("got %+v", resp)
	}
	if _, ok := v.current(); ok || len(v.state.Vars) != 0 {
		t.Errorf("the refused deploy changed the history: %+v", v.state)
	}
	if left, err := os.ReadDir(v.dir); err != nil || len(left) != 0 {
		t.Errorf("left %v, %v", left, err)
	}
}

func TestADeployThatSendsNoValuesRefusesAVersionWhoseNamesTheHostsLack(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	kept := map[string]string{"FIXTURE_TOKEN": "b"}
	if err := v.begin(version{ID: binary(t, v, "first")}, nil, kept); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	d := &deployer{log: logging.Discard(), versions: v}
	resp := d.deploy(t.Context(), checks(t, "FIXTURE_TOKEN", "FIXTURE_EXTRA"), nil, false)
	if resp.Error != "the version uses FIXTURE_EXTRA, which isn't among the host's values" ||
		!slices.Equal(resp.MissingVars, []string{"FIXTURE_EXTRA"}) {
		t.Errorf("got %+v", resp)
	}
	if got := ids(v); len(got) != 1 || !maps.Equal(v.state.Vars, kept) {
		t.Errorf("the refused deploy changed the history: %+v", v.state)
	}
}

func TestADeploysValuesNeedNamesAProcessCouldAskFor(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	d := &deployer{log: logging.Discard(), versions: v}
	resp := d.deploy(t.Context(), checks(t), map[string]string{"toshiba-user": "someone"}, false)
	if !strings.HasPrefix(resp.Error, `the deploy's values: "toshiba-user" isn't a variable name`) ||
		strings.Contains(resp.Error, "someone") {
		t.Errorf("got %+v", resp)
	}
}

func TestARollbackKeepsTheValuesAndRefusesAVersionWhoseNamesTheyLack(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	add := func(path string) string {
		id, err := v.add(path)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, second := add(checks(t, "FIXTURE_EXTRA")), add(checks(t))
	for _, id := range []string{first, second} {
		if err := v.begin(version{ID: id}, nil, map[string]string{"FIXTURE_TOKEN": "b"}); err != nil {
			t.Fatal(err)
		}
		if err := v.commit(); err != nil {
			t.Fatal(err)
		}
	}
	d := &deployer{log: logging.Discard(), versions: v}
	resp := d.rollback(t.Context(), nil, false)
	if resp.Error != "the version uses FIXTURE_EXTRA, which isn't among the host's values" ||
		!slices.Equal(resp.MissingVars, []string{"FIXTURE_EXTRA"}) {
		t.Errorf("got %+v", resp)
	}
	resp = d.rollback(t.Context(), map[string]string{"OTHER": "x"}, false)
	if resp.Error != "the version uses FIXTURE_EXTRA, which isn't among the rollback's values" {
		t.Errorf("with values of its own, got %+v", resp)
	}
	if got := ids(v); !slices.Equal(got, []string{second, first}) || !maps.Equal(v.state.Vars, map[string]string{"FIXTURE_TOKEN": "b"}) {
		t.Errorf("the refused rollback changed the history: %+v", v.state)
	}
	if _, err := os.Stat(v.path(first)); err != nil {
		t.Errorf("the refused rollback removed the version it went back to: %v", err)
	}
}

func TestTheValuesFileIsTheHostUsersAloneAndWhole(t *testing.T) {
	dir := t.TempDir()
	set := newVarSet(map[string]string{"TOSHIBA_PASSWORD": "pa\"ss"})
	f := &varsFile{path: filepath.Join(dir, varsFileName), set: func() *varSet { return set }}
	for _, values := range []map[string]string{{"TOSHIBA_PASSWORD": "pa\"ss"}, {}, nil} {
		set = newVarSet(values)
		if err := f.write(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("the file's mode is %o", mode)
		}
		raw, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.Unmarshal(raw, &got); err != nil || got == nil || !maps.Equal(got, values) {
			t.Errorf("the file has %s, %v", raw, err)
		}
	}
	if left, err := os.ReadDir(dir); err != nil || len(left) != 1 {
		t.Errorf("the directory has %v, %v", left, err)
	}
}

func TestTheValuesGoInTheRuntimeDirectoryIfThereIsOne(t *testing.T) {
	data, runtime := t.TempDir(), t.TempDir()
	stale := filepath.Join(data, varsFileName)
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := varsDir("", data); err != nil || got != data {
		t.Errorf("without one, got %q, %v", got, err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("without one, the file in the data directory went: %v", err)
	}
	if got, err := varsDir(runtime, data); err != nil || got != runtime {
		t.Errorf("with one, got %q, %v", got, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("with one, the copy in the data directory stayed: %v", err)
	}
}

// A host killed as it wrote a file of values, or as a check ran with one, leaves it: the next one
// removes it, from either directory.
func TestTheValuesAHostLeftAreRemoved(t *testing.T) {
	data, runtime := t.TempDir(), t.TempDir()
	left := make([]string, 0, 4)
	for _, dir := range []string{data, runtime} {
		for _, name := range []string{".vars-123", ".check-vars-456"} {
			left = append(left, filepath.Join(dir, name))
		}
	}
	kept := filepath.Join(runtime, varsFileName)
	for _, path := range append(left, kept) {
		if err := os.WriteFile(path, []byte(`{"A":"b"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := varsDir(runtime, data); err != nil {
		t.Fatal(err)
	}
	for _, path := range left {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s stayed: %v", path, err)
		}
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the processes' file went: %v", err)
	}
}

func TestChangedVarsNamesWhatWasAddedChangedAndRemoved(t *testing.T) {
	before := map[string]string{"KEPT": "1", "CHANGED": "a", "REMOVED": "x", "EMPTIED": "y"}
	after := map[string]string{"KEPT": "1", "CHANGED": "b", "ADDED": "z", "EMPTIED": ""}
	if got := changedVars(before, after); !slices.Equal(got, []string{"ADDED", "CHANGED", "EMPTIED", "REMOVED"}) {
		t.Errorf("got %q", got)
	}
	if got := changedVars(nil, map[string]string{}); len(got) != 0 {
		t.Errorf("an empty set changed from none: %q", got)
	}
	// Two sets that differ only in where a value starts have different digests.
	if newVarSet(map[string]string{"A": "b", "C": ""}).digest == newVarSet(map[string]string{"A": "", "C": "b"}).digest {
		t.Error("two sets share a digest")
	}
}

func TestAManifestNamesItsVars(t *testing.T) {
	m, err := runCheck(t.Context(), checks(t, "TOSHIBA_USER"), nil, nil)
	if err != nil || !slices.Equal(m.Vars, []string{"TOSHIBA_USER"}) || m.Protocol != wire.Version {
		t.Errorf("got %+v, %v", m, err)
	}
}

// A version's check runs with the values it would run with, so code that reads one as the home is
// built is checked as it will run: the deploy's, in a file of their own that's gone once it's done.
func TestAVersionsCheckHasTheDeploysValues(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	dir, seen := t.TempDir(), filepath.Join(t.TempDir(), "seen")
	m := manifest()
	m.Vars = []string{"TOSHIBA_PASSWORD"}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	binary := script(t, fmt.Sprintf(`cat "$%s" > %s
echo '%s'`, wire.VarsEnv, seen, raw))
	d := &deployer{log: logging.Discard(), versions: v,
		vars: &varsFile{path: filepath.Join(dir, varsFileName), set: v.vars}}
	resp := d.deploy(t.Context(), binary, map[string]string{"TOSHIBA_USER": "someone"}, false)
	if !slices.Equal(resp.MissingVars, []string{"TOSHIBA_PASSWORD"}) {
		t.Fatalf("got %+v", resp)
	}
	got, err := os.ReadFile(seen)
	if err != nil || string(got) != `{"TOSHIBA_USER":"someone"}` {
		t.Errorf("the check had %s, %v", got, err)
	}
	if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
		t.Errorf("left %v, %v", left, err)
	}
}
