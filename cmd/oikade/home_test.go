package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGo puts a go on PATH that prints Go's own lines and fails on the subcommand failOn, if any.
func fakeGo(t *testing.T, failOn string) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
echo "go: to add module requirements and sums:" >&2
echo "	go mod tidy" >&2
echo "go: running $1" 
if [ "$1" = "` + failOn + `" ]; then
	echo "go: $1 went wrong" >&2
	exit 1
fi
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestModuleFromReleaseHidesGoWhenItPasses(t *testing.T) {
	fakeGo(t, "")
	var stderr bytes.Buffer
	if err := moduleFromRelease(context.Background(), &stderr, t.TempDir(), "home"); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); strings.Contains(got, "go:") || strings.Contains(got, "go mod tidy") {
		t.Errorf("passed on Go's lines:\n%s", got)
	}
	if !strings.HasPrefix(stderr.String(), "Fetching Oikade ") {
		t.Errorf("didn't say what it's doing: %q", stderr.String())
	}
}

func TestModuleFromReleaseShowsGoWhenItFails(t *testing.T) {
	fakeGo(t, "get")
	var stderr bytes.Buffer
	err := moduleFromRelease(context.Background(), &stderr, t.TempDir(), "home")
	if err == nil || !strings.Contains(err.Error(), "go get") {
		t.Fatalf("got %v", err)
	}
	for _, want := range []string{"go: to add module requirements and sums:", "go: running get", "go: get went wrong"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("no %q in:\n%s", want, stderr.String())
		}
	}
	if strings.Count(stderr.String(), "go: running") != 1 {
		t.Errorf("showed the output of commands that passed:\n%s", stderr.String())
	}
}
