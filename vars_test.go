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
