package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedVersionIsOneGoCanFetch(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "v0.0.0-2026-abc+dirty"
	if got := publishedVersion(); got != "main" {
		t.Errorf("a local build: %s", got)
	}
	version = "v0.2.0"
	if got := publishedVersion(); got != "v0.2.0" {
		t.Errorf("a release: %s", got)
	}
}

func TestParsePlatform(t *testing.T) {
	for uname, want := range map[string]string{"Linux aarch64\n": "arm64", "Linux x86_64\n": "amd64"} {
		goos, goarch, err := parsePlatform(uname)
		if err != nil || goos != "linux" || goarch != want {
			t.Errorf("%q: %s %s %v", uname, goos, goarch, err)
		}
	}
	for _, uname := range []string{"Darwin arm64", "Linux armv7l", ""} {
		if _, _, err := parsePlatform(uname); err == nil {
			t.Errorf("%q has a build", uname)
		}
	}
}

func TestReadSecretTakesOneLineEndingOff(t *testing.T) {
	for in, want := range map[string]string{
		"value\n": "value", "value\r\n": "value", "value": "value", "two\nlines\n": "two\nlines",
	} {
		path := filepath.Join(t.TempDir(), "in")
		if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readSecret(f)
		_ = f.Close()
		if err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
}

func TestReadSecretRefusesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := readSecret(f); err == nil {
		t.Error("an empty value was accepted")
	}
}
