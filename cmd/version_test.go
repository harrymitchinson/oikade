package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionDefaultsToDev(t *testing.T) {
	var out bytes.Buffer
	printVersion(&out)
	if got := strings.TrimSpace(out.String()); got != "dev" {
		t.Errorf("version without ldflags = %q, want dev", got)
	}
}

func TestVersionFromLdflags(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the manager binary")
	}
	const want = "v0.1.0-alpha.1"
	bin := filepath.Join(t.TempDir(), "manager")
	build := exec.Command("go", "build", "-ldflags", "-X main.version="+want, "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"--version"}, {"runtime", "--version"}, {"matter", "--version"}} {
		out, err := exec.Command(bin, args...).Output()
		if err != nil {
			t.Fatalf("manager %s: %v", strings.Join(args, " "), err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("manager %s = %q, want %q", strings.Join(args, " "), got, want)
		}
	}
}
