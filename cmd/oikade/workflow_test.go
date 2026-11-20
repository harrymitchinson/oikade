package main

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/testkit"
)

func initWorkflow(t *testing.T) string {
	t.Helper()
	files, err := homeFiles()
	if err != nil {
		t.Fatal(err)
	}
	return string(files[".github/workflows/deploy.yml"])
}

// The workflow oikade init writes, as a new home gets it.
func TestInitWritesTheWorkflow(t *testing.T) {
	testkit.Golden(t, "init-workflow", []byte(initWorkflow(t)))
}

// A new home's first push passes: the deploy job runs only once OIKADE_ADDRESS is set, on a runner
// that can reach the host, and until then the test job says how to turn it on (OIK-286).
func TestTheWorkflowDeploysOnlyOnceItsTurnedOn(t *testing.T) {
	workflow := initWorkflow(t)
	if strings.Contains(workflow, "\t") {
		t.Error("the workflow indents with a tab, which YAML doesn't allow")
	}
	if strings.Contains(workflow, "OIKADE_VERSION") {
		t.Error("the workflow still says OIKADE_VERSION")
	}
	// Both jobs install the same oikade, the one that wrote the home.
	if n := strings.Count(workflow, "cmd/oikade@"+publishedVersion()+"\n"); n != 2 {
		t.Errorf("the workflow installs oikade@%s %d times, not once in each job", publishedVersion(), n)
	}

	want := map[string]map[string]string{
		"test": {"runs-on": "ubuntu-latest"},
		"deploy": {
			"needs":   "test",
			"if":      "github.event_name == 'push' && vars.OIKADE_ADDRESS != ''",
			"runs-on": "self-hosted",
		},
	}
	got := workflowJobs(workflow)
	if !maps.EqualFunc(got, want, maps.Equal) {
		t.Errorf("the workflow's jobs are %v, not %v", got, want)
	}

	off := "if: github.event_name == 'push' && vars.OIKADE_ADDRESS == ''"
	for _, s := range []string{off, "::notice title=Deploy is off::", deployFromCIDocs + `"`} {
		if !strings.Contains(workflow, s) {
			t.Errorf("the test job has no %s:\n%s", s, workflow)
		}
	}
}

// The notice links a section the docs have.
func TestTheWorkflowsNoticeLinksTheDocs(t *testing.T) {
	_, anchor, _ := strings.Cut(deployFromCIDocs, "#")
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "running-a-home.md"))
	if err != nil {
		t.Fatal(err)
	}
	headings := regexp.MustCompile(`(?m)^#+ (.+)$`).FindAllStringSubmatch(string(raw), -1)
	for _, h := range headings {
		if strings.ReplaceAll(strings.ToLower(h[1]), " ", "-") == anchor {
			return
		}
	}
	t.Errorf("docs/running-a-home.md has no heading for #%s", anchor)
}

// workflowJobs reads each job's keys that have a value on their own line, by indentation: a job is
// two spaces in under jobs:, and its keys are four.
func workflowJobs(workflow string) map[string]map[string]string {
	jobs := map[string]map[string]string{}
	var job string
	in := false
	for line := range strings.SplitSeq(workflow, "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		value = strings.TrimSpace(value)
		switch {
		case line == "jobs:":
			in = true
		case !in || line == "":
		case indent == 0:
			in = false
		case indent == 2 && value == "":
			job = key
			jobs[job] = map[string]string{}
		case indent == 4 && value != "" && job != "":
			jobs[job][key] = value
		}
	}
	return jobs
}
