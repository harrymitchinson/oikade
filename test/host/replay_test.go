package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/simhome"
)

// moduleEnv builds the fixture's module with nothing to fetch.
var moduleEnv = []string{"GOFLAGS=-mod=mod", "GOPROXY=off"}

// goodDay gives a host a morning to look back on: a fan that runs, and a flow with an outside call.
func goodDay(r *rig) {
	r.t.Helper()
	r.sync()
	r.change("bathroom-sensor", humidityCap, 65)
	r.change("bathroom-sensor", humidityCap, 80)
	r.wrote("fan", true)
	r.change("bathroom-sensor", humidityCap, 50)
	r.wrote("fan", false)
	r.contact("flow-go", true)
	r.wrote("routine-done", true)
	r.sync()
}

const humidityCap = "sensor.relative-humidity"

// fixtureModule copies the home's module, edited, with devices.go from the host.
func fixtureModule(r *rig, edit func(string) string) string {
	r.t.Helper()
	dir := r.t.TempDir()
	from := filepath.Join(buildDir, "module")
	for _, name := range []string{"go.mod", "go.sum", "main.go"} {
		raw, err := os.ReadFile(filepath.Join(from, name))
		if err != nil {
			r.t.Fatal(err)
		}
		if name == "main.go" {
			raw = []byte(edit(string(raw)))
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	r.stdout(nil, "types", "--out", filepath.Join(dir, "devices.go"), "--package", "main")
	return dir
}

func (r *rig) replay(module string) simhome.Report {
	r.t.Helper()
	var report simhome.Report
	env := append(slices.Clip(moduleEnv), r.env...)
	out := r.stdout(env, "replay", "--module", module, "--json")
	if err := json.Unmarshal(out, &report); err != nil {
		r.t.Fatalf("reading the report: %v: %s", err, out)
	}
	return report
}

func calls(t *testing.T, r *rig) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReplayingUnchangedCodeShowsNoDifferences(t *testing.T) {
	t.Parallel()
	r := flowRig(t, "")
	r.launch()
	r.waitFor(func(l line) bool { return l.is(attached) })
	goodDay(r)
	calledBefore := calls(t, r)
	wroteBefore := r.count(wrote)

	report := r.replay(fixtureModule(r, func(s string) string { return s }))
	if len(report.Removed)+len(report.Added) != 0 {
		t.Fatalf("Differences from the same code: removed %v, added %v", report.Removed, report.Added)
	}
	if report.Deployed == 0 || report.Changes == 0 {
		t.Fatalf("Nothing was compared: %+v", report)
	}
	if len(report.Skipped) != 0 {
		t.Fatalf("Skipped = %v: the outside call has an answer on record", report.Skipped)
	}
	if after := calls(t, r); after != calledBefore {
		t.Fatalf("The replay made an outside call: %q, then %q", calledBefore, after)
	}
	if after := r.count(wrote); after != wroteBefore {
		t.Fatalf("The replay wrote to a device: %d writes, then %d", wroteBefore, after)
	}
}

// fanAt finds the fixture's fan threshold.
var fanAt = regexp.MustCompile(`s\.Get\(humidity\) > \d+`)

func TestReplayingAChangedThresholdShowsExactlyTheWritesThatGo(t *testing.T) {
	t.Parallel()
	r := flowRig(t, "")
	r.launch()
	r.waitFor(func(l line) bool { return l.is(attached) })
	goodDay(r)

	report := r.replay(fixtureModule(r, func(s string) string {
		if !fanAt.MatchString(s) {
			t.Fatal("the fixture has no fan threshold to change")
		}
		return fanAt.ReplaceAllString(s, `s.Get(humidity) > 90`)
	}))
	if len(report.Added) != 0 {
		t.Fatalf("Added = %v", report.Added)
	}
	if len(report.Removed) != 2 || report.Removed[0].Device != "fan" || report.Removed[1].Device != "fan" {
		t.Fatalf("Removed = %v, want the fan's two writes", report.Removed)
	}
}

func TestReplayNeedsAHost(t *testing.T) {
	t.Parallel()
	r := prepare(t)
	out, err := r.oikade(moduleEnv, "replay", "--module", filepath.Join(buildDir, "module"))
	if err == nil || !strings.Contains(out, "reaching the host") {
		t.Fatalf("got %v: %s", err, out)
	}
}
