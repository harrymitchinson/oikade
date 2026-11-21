// Package repo checks what the repo says about itself, across its code and docs.
package repo

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"oikade.dev/internal/capability"
)

const root = "../.."

// lines calls found for each line of the files under dirs whose names end in suffix.
func lines(t *testing.T, dirs []string, suffix string, found func(path string, number int, line string)) {
	t.Helper()
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, suffix) {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(raw), "\n") {
				found(path, i+1, line)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Grafana queries these by name, so a metric that goes blanks a panel with nothing to say why.
func TestTheDashboardQueriesMetricsThatExist(t *testing.T) {
	dashboard, err := os.ReadFile(filepath.Join(root, "config", "grafana", "oikade-home.json"))
	if err != nil {
		t.Fatal(err)
	}
	queried := regexp.MustCompile(`oikade_[a-z_]+`).FindAllString(string(dashboard), -1)
	if len(queried) == 0 {
		t.Fatal("the dashboard queries no metrics")
	}
	defined := map[string]bool{}
	name := regexp.MustCompile(`"(oikade_[a-z_]+)"`)
	lines(t, []string{"internal"}, ".go", func(path string, _ int, line string) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		for _, m := range name.FindAllStringSubmatch(line, -1) {
			defined[m[1]] = true
		}
	})
	for _, metric := range queried {
		// A histogram is defined by its name, and queried by its series.
		for _, series := range []string{"_bucket", "_count", "_sum"} {
			if base, ok := strings.CutSuffix(metric, series); ok && defined[base] {
				metric = base
			}
		}
		if !defined[metric] {
			t.Errorf("the dashboard queries %s, which nothing defines", metric)
		}
	}
}

// A query for a capability the registry doesn't have, such as an old name, matches nothing.
func TestTheDashboardQueriesCapabilitiesThatExist(t *testing.T) {
	dashboard, err := os.ReadFile(filepath.Join(root, "config", "grafana", "oikade-home.json"))
	if err != nil {
		t.Fatal(err)
	}
	queried := regexp.MustCompile(`capability=~?\\"([^\\]*)\\"`).FindAllStringSubmatch(string(dashboard), -1)
	if len(queried) == 0 {
		t.Fatal("the dashboard queries no capabilities")
	}
	for _, m := range queried {
		for name := range strings.SplitSeq(m[1], "|") {
			if _, ok := capability.Lookup(name); !ok {
				t.Errorf("the dashboard queries capability %q, which the registry doesn't have", name)
			}
		}
	}
}

func TestTheSiteTellsGoWhereTheModuleIs(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	module := regexp.MustCompile(`(?m)^module (\S+)$`).FindSubmatch(mod)
	if module == nil {
		t.Fatal("go.mod names no module")
	}
	want := `<meta name="go-import" content="` + string(module[1]) + ` git https://github.com/harrymitchinson/oikade">`
	for _, page := range []string{"index.html", "404.html"} {
		raw, err := os.ReadFile(filepath.Join(root, "site", page))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("site/%s doesn't have %s", page, want)
		}
	}
}
