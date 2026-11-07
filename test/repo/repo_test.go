// Package repo checks what the repo says about itself, across its code and docs.
package repo

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const root = "../.."

// sourceDirs holds the code.
var sourceDirs = []string{"cmd", "contract", "framework", "internal", "plugins", "sdk", "test"}

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

// noImports fails for each line of the code that matches banned.
func noImports(t *testing.T, banned *regexp.Regexp) {
	t.Helper()
	lines(t, sourceDirs, ".go", func(path string, number int, line string) {
		if banned.MatchString(line) && !strings.HasSuffix(path, "repo_test.go") {
			t.Errorf("%s:%d imports it: %s", path, number, strings.TrimSpace(line))
		}
	})
}

// Oikade has no API types of its own any more: each type lives in the package that owns it.
func TestNothingImportsAnAPIPackage(t *testing.T) {
	noImports(t, regexp.MustCompile(`"github\.com/harrymitchinson/oikade/api(/|")`))
}

// Oikade has no Kubernetes target (OIK-143), so it uses nothing from Kubernetes, down to the clock.
func TestNoKubernetesModules(t *testing.T) {
	banned := regexp.MustCompile(`(sigs\.)?k8s\.io/[\w.-]+`)
	noImports(t, banned)
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, found := range banned.FindAllString(string(mod), -1) {
		t.Errorf("go.mod requires %s", found)
	}
}

// Oikade logs with log/slog alone (OIK-169).
func TestNoLogr(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "go-logr") {
		t.Error("go.mod requires go-logr")
	}
}

// Oikade reaches Apple Home through Matter (OIK-76). HomeKit's own protocol and its library are
// gone, and nothing in the internals should bring them back.
func TestNoHomeKit(t *testing.T) {
	banned := regexp.MustCompile(`(?i)home` + `kit|brutella|_hap\._tcp`)
	lines(t, []string{"internal"}, "", func(path string, number int, line string) {
		if banned.MatchString(line) {
			t.Errorf("%s:%d mentions it: %s", path, number, strings.TrimSpace(line))
		}
	})
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if banned.Match(mod) {
		t.Error("go.mod still requires it")
	}
}

// Nothing tells an agent to scaffold with kubebuilder, regenerate manifests or edit a CRD, because
// there are none: the host is the only thing Oikade runs (OIK-143).
func TestNoDocTellsAnAgentToUseTheOperatorsTools(t *testing.T) {
	banned := regexp.MustCompile(
		`(?i)kubebuilder|make manifests|make generate|zz_generated|\bcrds?\b|envtest|kustomize|test-e2e`)
	found := func(path string, number int, line string) {
		if banned.MatchString(line) {
			t.Errorf("%s:%d: %s", path, number, strings.TrimSpace(line))
		}
	}
	lines(t, []string{".claude/skills", "docs"}, ".md", found)
	for _, file := range []string{"AGENTS.md", "README.md"} {
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			found(file, i+1, line)
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
		if !defined[metric] {
			t.Errorf("the dashboard queries %s, which nothing defines", metric)
		}
	}
}
