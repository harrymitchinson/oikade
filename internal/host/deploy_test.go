package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/store"
)

func manifest(rules ...wire.RuleInfo) wire.Manifest {
	return wire.Manifest{Protocol: wire.Version, Rules: rules}
}

func TestRenamedLatchedSafetyHoldIsFlagged(t *testing.T) {
	old := manifest(wire.RuleInfo{Name: "leak", Kind: "hold", Safety: true, Latch: true}, wire.RuleInfo{Name: "fan", Kind: "hold"})
	next := manifest(wire.RuleInfo{Name: "water-leak", Kind: "hold", Safety: true, Latch: true}, wire.RuleInfo{Name: "fan", Kind: "hold"})
	findings := safetyFindings(old, next, nil)
	if len(findings) != 1 || !strings.Contains(findings[0], `"leak"`) || !strings.Contains(findings[0], "latch") {
		t.Fatalf("got %q", findings)
	}
}

func TestOnlySafetyRulesAreFlagged(t *testing.T) {
	old := manifest(wire.RuleInfo{Name: "fan", Kind: "hold"}, wire.RuleInfo{Name: "leak", Kind: "hold", Safety: true})
	for name, tc := range map[string]struct {
		next wire.Manifest
		held map[string]control.Level
		want int
	}{
		"a normal rule goes":        {manifest(wire.RuleInfo{Name: "leak", Safety: true}), nil, 0},
		"nothing goes":              {old, nil, 0},
		"a Safety flow's step":      {old, map[string]control.Level{"gone": control.Safety}, 1},
		"a lower claim":             {old, map[string]control.Level{"gone": control.Override}, 0},
		"a claim of one that stays": {old, map[string]control.Level{"leak": control.Safety}, 0},
		"the rule and its claim":    {manifest(), map[string]control.Level{"leak": control.Safety}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := safetyFindings(old, tc.next, tc.held); len(got) != tc.want {
				t.Fatalf("got %q, want %d findings", got, tc.want)
			}
		})
	}
}

// script writes an executable that acts as a binary's check mode.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckTakesTheManifestFromCheckMode(t *testing.T) {
	raw, err := json.Marshal(manifest(wire.RuleInfo{Name: "fan", Kind: "hold"}))
	if err != nil {
		t.Fatal(err)
	}
	path := script(t, fmt.Sprintf(`[ "$1" = check ] || exit 9
echo something a home printed
echo '%s'`, raw))
	m, err := runCheck(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Rules) != 1 || m.Rules[0].Name != "fan" {
		t.Fatalf("got %+v", m)
	}
}

func TestCheckRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"a failure":      {"echo the reason >&2; exit 1", "the reason"},
		"no manifest":    {"echo hello", "no manifest"},
		"no output":      {"exit 0", "no manifest"},
		"an old binary":  {`echo '{"protocol":1,"rules":[]}'`, "protocol 1"},
		"a newer binary": {fmt.Sprintf(`echo '{"protocol":%d}'`, wire.Version+1), "build it with"},
		"no protocol":    {`echo '{"rules":[]}'`, "protocol 0"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runCheck(context.Background(), script(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func openTestVersions(t *testing.T, dir string) (*versions, func()) {
	t.Helper()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := openVersions(dir, data)
	if err != nil {
		t.Fatal(err)
	}
	return v, func() { _ = data.Close() }
}

func binary(t *testing.T, v *versions, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := v.add(path)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ids(v *versions) []string {
	got := make([]string, 0, len(v.state.Versions))
	for _, k := range v.state.Versions {
		got = append(got, k.ID)
	}
	return got
}

func TestAVersionsNameIsItsContents(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	if a, b := binary(t, v, "same"), binary(t, v, "same"); a != b {
		t.Fatalf("got %s and %s", a, b)
	}
	if a, b := binary(t, v, "one"), binary(t, v, "other"); a == b {
		t.Fatal("two builds have one name")
	}
}

func TestAnAbortedVersionLeavesEverythingAsItWas(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	first, second := binary(t, v, "first"), binary(t, v, "second")
	for _, id := range []string{first, second} {
		if err := v.begin(version{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := v.commit(); err != nil {
			t.Fatal(err)
		}
	}
	bad := binary(t, v, "bad")
	if err := v.begin(version{ID: bad}); err != nil {
		t.Fatal(err)
	}
	if err := v.abort(); err != nil {
		t.Fatal(err)
	}
	if got := ids(v); !slices.Equal(got, []string{second, first}) {
		t.Fatalf("got %v", got)
	}
	if _, err := os.Stat(v.path(bad)); !os.IsNotExist(err) {
		t.Fatalf("the failed binary stayed: %v", err)
	}
	// A failed rollback is the same, and keeps the version it tried.
	if err := v.begin(version{ID: first}); err != nil {
		t.Fatal(err)
	}
	if err := v.abort(); err != nil {
		t.Fatal(err)
	}
	if got := ids(v); !slices.Equal(got, []string{second, first}) {
		t.Fatalf("got %v", got)
	}
	if _, err := os.Stat(v.path(first)); err != nil {
		t.Fatal(err)
	}
}

func TestVersionsAreBounded(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	added := make([]string, 0, keptVersions+3)
	for i := range keptVersions + 3 {
		id := binary(t, v, fmt.Sprintf("build %d", i))
		added = append(added, id)
		if err := v.begin(version{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := v.commit(); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(v.state.Versions); got != keptVersions {
		t.Fatalf("kept %d", got)
	}
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != keptVersions {
		t.Fatalf("%d files are left", len(entries))
	}
	if _, err := os.Stat(v.path(added[0])); !os.IsNotExist(err) {
		t.Fatalf("the oldest binary stayed: %v", err)
	}
}

// A host that crashed before a version settled finds the record, and brings the last one back.
func TestAnUnfinishedDeployIsUndoneOnStartup(t *testing.T) {
	dir := t.TempDir()
	v, closeStore := openTestVersions(t, dir)
	good, bad := binary(t, v, "good"), binary(t, v, "bad")
	if err := v.begin(version{ID: good}); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	if err := v.begin(version{ID: bad}); err != nil {
		t.Fatal(err)
	}
	closeStore()

	v, closeStore = openTestVersions(t, dir)
	defer closeStore()
	d := &deployer{versions: v}
	if got := d.startup(); got != v.path(good) {
		t.Fatalf("got %s, want %s", got, v.path(good))
	}
	if v.state.Pending {
		t.Fatal("the deploy is still pending")
	}
}
