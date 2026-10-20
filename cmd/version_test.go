package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the manager binary")
	}
	for _, tc := range []struct {
		name    string
		ldflags string
		want    string
	}{
		{name: "plain build", want: "dev"},
		{name: "release build", ldflags: "-X main.version=v0.1.0-alpha.1", want: "v0.1.0-alpha.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "manager")
			build := exec.Command("go", "build", "-ldflags", tc.ldflags, "-o", bin, ".")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("go build: %v\n%s", err, out)
			}
			out, err := exec.Command(bin, "--version").Output()
			if err != nil {
				t.Fatalf("manager --version: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("manager --version = %q, want %q", got, tc.want)
			}
		})
	}
}
