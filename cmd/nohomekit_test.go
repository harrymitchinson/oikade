package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Oikade reaches Apple Home through Matter (OIK-76). HomeKit's own protocol and its library are
// gone, and nothing in the API or the internals should bring them back.
func TestNoHomeKit(t *testing.T) {
	banned := regexp.MustCompile(`(?i)home` + `kit|brutella|_hap\._tcp`)
	for _, dir := range []string{"../api", "../internal"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(raw), "\n") {
				if banned.MatchString(line) {
					t.Errorf("%s:%d mentions it: %s", path, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if banned.Match(mod) {
		t.Error("go.mod still requires it")
	}
}
