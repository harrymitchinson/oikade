package clusters

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// Metrics label a controller's writes with Name, so a cluster missing from it is counted as "other".
func TestEveryClusterHasAName(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`(?m)^\s*(ID[A-Z]\w*)\s+uint32\s*=\s*(0x[0-9A-Fa-f]+)`)
	found := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range id.FindAllStringSubmatch(string(raw), -1) {
			found++
			v, err := strconv.ParseUint(m[2], 0, 32)
			if err != nil {
				t.Fatal(err)
			}
			if Name(uint32(v)) == "other" {
				t.Errorf("%s (%s) has no name in names", m[1], m[2])
			}
		}
	}
	if found < 10 {
		t.Errorf("found %d cluster IDs: the pattern no longer matches how they're declared", found)
	}
}
