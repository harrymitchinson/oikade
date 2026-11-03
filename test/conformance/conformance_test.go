// Package conformance runs the oikade/v1 conformance suite (internal/conformance) under go test,
// for make test-conformance. Plugin repos run the same suite with cmd/oikade-conformance.
//
// It runs only when OIKADE_CONFORMANCE_PLUGIN is set:
//
//	OIKADE_CONFORMANCE_PLUGIN   the command that starts the plugin, split on spaces, run from the
//	                            repository root, for example "bin/simswitch"
//	OIKADE_CONFORMANCE_DEVICES  a devices.json to hand the plugin, relative to the repository root
//	                            (default test/conformance/testdata/devices.json)
//	OIKADE_CONFORMANCE_SECRETS  a directory to pass as OIKADE_SECRETS_DIR (optional)
package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"

	"github.com/harrymitchinson/oikade/internal/conformance"
)

func TestConformance(t *testing.T) {
	command := strings.Fields(os.Getenv("OIKADE_CONFORMANCE_PLUGIN"))
	if len(command) == 0 {
		t.Skip("OIKADE_CONFORMANCE_PLUGIN is not set")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cfg := conformance.Config{
		Command: command,
		Dir:     repo,
		Devices: fromRepo(repo, "test/conformance/testdata/devices.json"),
		Log:     testr.New(t),
	}
	if f := os.Getenv("OIKADE_CONFORMANCE_DEVICES"); f != "" {
		cfg.Devices = fromRepo(repo, f)
	}
	if dir := os.Getenv("OIKADE_CONFORMANCE_SECRETS"); dir != "" {
		cfg.Secrets = fromRepo(repo, dir)
	}
	s := conformance.New(t, cfg)
	for _, c := range s.Checks() {
		t.Run(c.Name, func(t *testing.T) { c.Run(t) })
	}
}

// fromRepo resolves a path given relative to the repository root, leaving absolute paths alone.
func fromRepo(repo, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(repo, path)
}
