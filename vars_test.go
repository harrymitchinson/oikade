package oikade

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func valuesIn(path string) *values { return &values{path: func() string { return path }} }

func TestVarReadsTheFileTheHostNamed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vars.json")
	if err := os.WriteFile(path, []byte(`{"TOSHIBA_USER":"someone","TOSHIBA_PASSWORD":"pa ss"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	v := valuesIn(path)
	if got := v.get("TOSHIBA_USER"); got != "someone" {
		t.Errorf("TOSHIBA_USER is %q", got)
	}
	if got := v.get("TOSHIBA_PASSWORD"); got != "pa ss" {
		t.Errorf("TOSHIBA_PASSWORD is %q", got)
	}
	// One the host has no value for is empty, and still declared.
	if got := v.get("TAHOMA_PIN"); got != "" {
		t.Errorf("TAHOMA_PIN is %q", got)
	}
	if got := v.names(); !slices.Equal(got, []string{"TAHOMA_PIN", "TOSHIBA_PASSWORD", "TOSHIBA_USER"}) {
		t.Errorf("names are %q", got)
	}
	if err := v.check(); err != nil {
		t.Error(err)
	}
}

func TestVarIsEmptyWithoutTheHost(t *testing.T) {
	v := valuesIn("")
	if got := v.get("TOSHIBA_USER"); got != "" {
		t.Errorf("got %q", got)
	}
	if err := v.check(); err != nil {
		t.Error(err)
	}
}

func TestVarRefusesANameThatBreaksTheRule(t *testing.T) {
	v := valuesIn("")
	v.get("toshiba-user")
	err := v.check()
	if err == nil || !strings.Contains(err.Error(), `oikade.Var: "toshiba-user" isn't a variable name`) ||
		!strings.Contains(err.Error(), `such as "TOSHIBA_USER"`) {
		t.Errorf("got %v", err)
	}
}

func TestAProcessWhoseValuesCantBeReadFails(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"A": "the secret value`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing.json"), bad} {
		v := valuesIn(path)
		v.get("A")
		err := v.check()
		if err == nil || !strings.Contains(err.Error(), "reading the values the host gave this process") {
			t.Errorf("%s: got %v", path, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret value") {
			t.Errorf("the error quotes a value: %v", err)
		}
	}
}

func TestVarDeclaresItsNameInTheManifest(t *testing.T) {
	_ = Var("MANIFEST_TEST_VAR")
	if m := NewHome(Options{}).manifest(); !slices.Contains(m.Vars, "MANIFEST_TEST_VAR") {
		t.Errorf("the manifest's vars are %q", m.Vars)
	}
}

// Run by oikade on your machine, the environment wins over the file for any name, as it does for a
// deploy's values. Run by the host, it never does.
func TestOnYourMachineTheEnvironmentWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vars.json")
	if err := os.WriteFile(path, []byte(`{"TOSHIBA_USER":"from-the-file","TAHOMA_PIN":"1234"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOSHIBA_USER", "from-the-environment")
	t.Setenv("COZYTOUCH_USER", "only-in-the-environment")
	for wins, want := range map[bool][2]string{
		true:  {"from-the-environment", "only-in-the-environment"},
		false: {"from-the-file", ""},
	} {
		v := valuesIn(path)
		v.environment = func() bool { return wins }
		if got := [2]string{v.get("TOSHIBA_USER"), v.get("COZYTOUCH_USER")}; got != want || v.get("TAHOMA_PIN") != "1234" {
			t.Errorf("with the environment winning %v, got %q", wins, got)
		}
	}
}
