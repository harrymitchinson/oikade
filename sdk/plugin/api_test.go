package plugin

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/apicheck"
)

// Plugins live in other modules, and can't name anything internal.
func TestExportedAPIMentionsNothingInternal(t *testing.T) {
	found, err := apicheck.Check(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Error(f)
	}
}

func TestAPluginDoesNotLinkTheEngine(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("listing dependencies: %v\n%s", err, out)
	}
	for dep := range strings.Lines(string(out)) {
		if strings.Contains(dep, "/internal/engine") {
			t.Fatalf("the SDK depends on %s", dep)
		}
	}
}
