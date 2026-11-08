package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/store"
)

func manifest(rules ...wire.RuleInfo) wire.Manifest {
	return wire.Manifest{Protocol: wire.Version, Rules: rules}
}

func TestRenamedLatchedSafetyHoldIsFlagged(t *testing.T) {
	old := manifest(wire.RuleInfo{Name: "leak", Kind: "hold", Safety: true, Latch: true}, wire.RuleInfo{Name: "fan", Kind: "hold"})
	next := manifest(wire.RuleInfo{Name: "water-leak", Kind: "hold", Safety: true, Latch: true}, wire.RuleInfo{Name: "fan", Kind: "hold"})
	findings := safetyFindings(old, next, nil, noneHeld)
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
			if got := safetyFindings(old, tc.next, tc.held, noneHeld); len(got) != tc.want {
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
		if err := v.begin(version{ID: id}, nil); err != nil {
			t.Fatal(err)
		}
		if err := v.commit(); err != nil {
			t.Fatal(err)
		}
	}
	bad := binary(t, v, "bad")
	if err := v.begin(version{ID: bad}, nil); err != nil {
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
	if err := v.begin(version{ID: first}, nil); err != nil {
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
		if err := v.begin(version{ID: id}, nil); err != nil {
			t.Fatal(err)
		}
		if err := v.commit(); err != nil {
			t.Fatal(err)
		}
		if err := v.prune(); err != nil {
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

func TestRollbackGoesFurtherBackEachTime(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	first, second, third := binary(t, v, "first"), binary(t, v, "second"), binary(t, v, "third")
	for _, id := range []string{first, second, third} {
		if err := v.begin(version{ID: id}, nil); err != nil {
			t.Fatal(err)
		}
		if err := v.commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{second, first} {
		prev, _ := v.previous()
		if err := v.beginRollback(prev, nil); err != nil {
			t.Fatal(err)
		}
		if err := v.commit(); err != nil {
			t.Fatal(err)
		}
		if err := v.prune(); err != nil {
			t.Fatal(err)
		}
		if cur, _ := v.current(); cur.ID != want {
			t.Fatalf("got %s, want %s", cur.ID, want)
		}
	}
	if _, ok := v.previous(); ok {
		t.Fatal("there should be nothing left")
	}
	if _, err := os.Stat(v.path(third)); !os.IsNotExist(err) {
		t.Fatalf("a rolled-back binary stayed: %v", err)
	}
}

// A failed save changes nothing, so the old version is still what's running.
func TestAFailedSaveLeavesTheHistoryAlone(t *testing.T) {
	dir := t.TempDir()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := openVersions(dir, data)
	if err != nil {
		t.Fatal(err)
	}
	good, bad := binary(t, v, "good"), binary(t, v, "bad")
	if err := v.begin(version{ID: good}, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	if err := v.begin(version{ID: bad}, nil); err == nil {
		t.Fatal("a save to a closed store worked")
	}
	if cur, _ := v.current(); cur.ID != good || v.state.Pending || v.known(bad) {
		t.Fatalf("got %+v", v.state)
	}
}

// A host that crashed before a version settled finds the record, and brings the last one back.
func TestAnUnfinishedDeployIsUndoneOnStartup(t *testing.T) {
	dir := t.TempDir()
	v, closeStore := openTestVersions(t, dir)
	good, bad := binary(t, v, "good"), binary(t, v, "bad")
	if err := v.begin(version{ID: good}, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	if err := v.begin(version{ID: bad}, nil); err != nil {
		t.Fatal(err)
	}
	closeStore()

	v, closeStore = openTestVersions(t, dir)
	defer closeStore()
	d := &deployer{log: logging.Discard(), versions: v}
	if got := d.startup(); got != v.path(good) {
		t.Fatalf("got %s, want %s", got, v.path(good))
	}
	if v.state.Pending {
		t.Fatal("the deploy is still pending")
	}
}

// A version that failed isn't what runs, whether or not that could be saved.
func TestAFailedVersionIsGoneEvenIfThatCantBeSaved(t *testing.T) {
	dir := t.TempDir()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := openVersions(dir, data)
	if err != nil {
		t.Fatal(err)
	}
	good, bad := binary(t, v, "good"), binary(t, v, "bad")
	if err := v.begin(version{ID: good}, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	if err := v.begin(version{ID: bad}, nil); err != nil {
		t.Fatal(err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	if err := v.abort(); err == nil {
		t.Fatal("a save to a closed store worked")
	}
	if cur, _ := v.current(); cur.ID != good || v.state.Pending {
		t.Fatalf("got %+v", v.state)
	}
}

// A version whose rules aren't known looks like one with none, which would end everything.
func TestRemovalsWaitForKnownRules(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	if err := v.begin(version{ID: binary(t, v, "first")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	if err := (&deployer{log: logging.Discard(), versions: v}).removals(); err != nil || !v.state.Removals {
		t.Fatalf("got %v, %+v", err, v.state)
	}
}

// Confirming can fail, which leaves the version running but unconfirmed. Deploying it again retries
// that, and doesn't call it done until it is.
func TestRedeployingAVersionThatCouldntBeConfirmedRetries(t *testing.T) {
	dir := t.TempDir()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := openVersions(dir, data)
	if err != nil {
		t.Fatal(err)
	}
	id := binary(t, v, "home")
	if err := v.begin(version{ID: id}, nil); err != nil {
		t.Fatal(err)
	}
	d := &deployer{log: logging.Discard(), versions: v, srv: &server{log: logging.Discard(), feed: newFeed(), events: &eventLog{log: logging.Discard()}}}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	if resp := d.confirm(id); resp.Error == "" {
		t.Fatalf("a save to a closed store worked: %+v", resp)
	}
	resp := d.deploy(context.Background(), v.path(id), false)
	if resp.Error == "" || resp.Unchanged || !v.state.Pending {
		t.Fatalf("got %+v, %+v", resp, v.state)
	}
	data, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	v.values = data.Namespace(versionsNamespace)
	resp = d.deploy(context.Background(), v.path(id), false)
	if resp.Error != "" || resp.Unchanged || v.state.Pending {
		t.Fatalf("got %+v, %+v", resp, v.state)
	}
	if resp = d.deploy(context.Background(), v.path(id), false); !resp.Unchanged {
		t.Fatalf("it was confirmed twice: %+v", resp)
	}
}

// Unconfirmed alone isn't enough: nothing has said this one settled.
func TestRedeployingAVersionThatHasntSettledConfirmsNothing(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	id := binary(t, v, "home")
	if err := v.begin(version{ID: id}, nil); err != nil {
		t.Fatal(err)
	}
	resp := (&deployer{log: logging.Discard(), versions: v}).deploy(context.Background(), v.path(id), false)
	if resp.Error == "" || resp.Unchanged || !v.state.Pending {
		t.Fatalf("got %+v, %+v", resp, v.state)
	}
}

// An unconfirmed version mustn't end anything from its manifest, which nothing has confirmed.
func TestRemovalsWaitForAConfirmedVersion(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	if err := v.begin(version{ID: binary(t, v, "first"), Manifest: manifest()}, nil); err != nil {
		t.Fatal(err)
	}
	v.state.Removals = true
	if err := (&deployer{log: logging.Discard(), versions: v}).removals(); err != nil || !v.state.Removals {
		t.Fatalf("got %v, %+v", err, v.state)
	}
}

// What a failed deploy spared survives a restart, and a version adopted after it.
func TestAFailedDeployKeepsWhatItOwes(t *testing.T) {
	dir := t.TempDir()
	v, closeStore := openTestVersions(t, dir)
	if err := v.begin(version{ID: binary(t, v, "bad")}, []string{"leak-shutoff"}); err != nil {
		t.Fatal(err)
	}
	if err := v.abort(); err != nil {
		t.Fatal(err)
	}
	closeStore()
	v, closeStore = openTestVersions(t, dir)
	defer closeStore()
	if err := v.adopt(version{ID: binary(t, v, "home")}); err != nil {
		t.Fatal(err)
	}
	if !v.state.Removals || !slices.Equal(v.state.Spared, []string{"leak-shutoff"}) {
		t.Fatalf("got %+v", v.state)
	}
	if err := v.begin(version{ID: binary(t, v, "next")}, []string{"leak-shutoff"}); err != nil {
		t.Fatal(err)
	}
	if err := v.commit(); err != nil {
		t.Fatal(err)
	}
	if !v.state.Removals || v.state.Spared != nil {
		t.Fatalf("a confirmed version spares nothing: %+v", v.state)
	}
}

// A refused deploy changes nothing, so the binary from the host's flags stays the one a restart runs,
// and no copy of it is left behind.
func TestARefusedDeployLeavesTheHomeBinaryOffTheRecord(t *testing.T) {
	dir := t.TempDir()
	v, closeStore := openTestVersions(t, dir)
	d := &deployer{log: logging.Discard(), versions: v, home: script(t, "echo '{\"protocol\":"+strconv.Itoa(wire.Version)+"}'")}
	d.running = d.home
	resp := d.deploy(context.Background(), script(t, "echo broken >&2; exit 1"), false)
	if resp.Error == "" {
		t.Fatalf("a binary that fails its check was taken: %+v", resp)
	}
	closeStore()
	v, closeStore = openTestVersions(t, dir)
	defer closeStore()
	if _, ok := v.current(); ok {
		t.Fatalf("the refused deploy recorded a version: %+v", v.state)
	}
	if left, err := os.ReadDir(v.dir); err != nil || len(left) != 0 {
		t.Fatalf("got %v, %v", left, err)
	}
}

// Deploying the binary from the flags changes nothing, but it counts as deployed only if it passes
// its check.
func TestDeployingTheHomeBinaryItself(t *testing.T) {
	passes := "echo '{\"protocol\":" + strconv.Itoa(wire.Version) + "}'"
	for _, tc := range []struct {
		name, body string
		kept       bool
	}{
		{"passes its check", passes, true},
		{"fails its check", "echo broken >&2; exit 1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, closeStore := openTestVersions(t, t.TempDir())
			defer closeStore()
			d := &deployer{log: logging.Discard(), versions: v, home: script(t, tc.body)}
			d.running = d.home
			resp := d.deploy(context.Background(), d.home, false)
			if _, ok := v.current(); ok != tc.kept || resp.Unchanged != tc.kept || (resp.Error == "") != tc.kept {
				t.Fatalf("got %+v, %+v", resp, v.state)
			}
		})
	}
}

// The home binary goes on record with the deploy that gets past its check, in one write, so a crash
// at any point after it still has the home binary to go back to.
func TestBeginningFromTheHomeBinaryRecordsBoth(t *testing.T) {
	dir := t.TempDir()
	v, closeStore := openTestVersions(t, dir)
	home, next := version{ID: binary(t, v, "home")}, version{ID: binary(t, v, "next")}
	if err := v.beginFromHome(home, next, []string{"leak-shutoff"}); err != nil {
		t.Fatal(err)
	}
	if got := ids(v); !slices.Equal(got, []string{next.ID, home.ID}) || !v.state.Pending {
		t.Fatalf("got %v, %+v", got, v.state)
	}
	closeStore()
	v, closeStore = openTestVersions(t, dir)
	defer closeStore()
	if err := v.abort(); err != nil {
		t.Fatal(err)
	}
	if got := ids(v); !slices.Equal(got, []string{home.ID}) {
		t.Fatalf("got %v", got)
	}
}

// With nothing before the failed version there's nothing to bring back, so it isn't reported as done.
func TestRestoringWithNothingBeforeItRestoresNothing(t *testing.T) {
	v, closeStore := openTestVersions(t, t.TempDir())
	defer closeStore()
	id := binary(t, v, "first")
	if err := v.begin(version{ID: id}, nil); err != nil {
		t.Fatal(err)
	}
	d := &deployer{log: logging.Discard(), versions: v, swaps: make(chan *swap)}
	stopped := make(chan string, 1)
	go func() {
		sw := <-d.swaps
		stopped <- sw.path
		sw.started <- nil
	}()
	resp := d.restore(context.Background(), id, "", errors.New("it exited"))
	if resp.Restored || resp.Error != "it exited" || resp.Version != "" {
		t.Fatalf("got %+v", resp)
	}
	if path := <-stopped; path != "" {
		t.Fatalf("it ran %q", path)
	}
	if _, ok := v.current(); ok || v.state.Pending {
		t.Fatalf("got %+v", v.state)
	}
}

// A connection from before a disconnect is the process that's going, however late it gets here.
func TestAStaleConnectionNeverAttaches(t *testing.T) {
	s := &server{log: logging.Discard()}
	stale := &session{generation: s.generation}
	s.disconnect()
	if s.attach(stale, wire.Message{}, nil) {
		t.Fatal("it attached")
	}
}

func noneHeld(string, string) bool { return false }

func TestDroppingAControlThatHoldsASafetyClaimIsAFinding(t *testing.T) {
	entry := func(c string) wire.DeviceControl {
		return wire.DeviceControl{Device: "valve", Entry: control.Entry{Capability: c}}
	}
	old := wire.Manifest{Control: []wire.DeviceControl{entry("on"), entry("brightness")}}
	next := wire.Manifest{Control: []wire.DeviceControl{entry("on")}}
	held := func(device, name string) bool { return device == "valve" && name == "brightness" }
	if got := safetyFindings(old, next, nil, held); len(got) != 1 {
		t.Fatalf("findings %v, want one", got)
	}
	if got := safetyFindings(old, next, nil, noneHeld); len(got) != 0 {
		t.Fatalf("findings %v with nothing held", got)
	}
	if got := safetyFindings(old, old, nil, held); len(got) != 0 {
		t.Fatalf("findings %v with nothing dropped", got)
	}
}
