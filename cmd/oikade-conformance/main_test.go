package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// examplePlugin builds plugins/example-switch and returns its absolute path.
func examplePlugin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "example-switch")
	out, err := exec.Command("go", "build", "-o", bin, "../../plugins/example-switch").CombinedOutput()
	if err != nil {
		t.Fatalf("building example-switch: %v\n%s", err, out)
	}
	return bin
}

// fromTestdata runs the command from testdata, as a plugin repo runs it from its own checkout,
// so relative paths must resolve against the working directory and not the repository root.
func fromTestdata(t *testing.T) {
	t.Helper()
	t.Chdir("testdata")
}

func TestPassesAgainstExampleSwitchFromAnotherDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real plugin process")
	}
	bin := examplePlugin(t)
	fromTestdata(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--devices", "devices.json", "--", bin}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	for _, want := range []string{
		"--- PASS: announces itself and every declared device",
		"--- PASS: leaves a Last Will when killed",
		"\nPASS\n",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, &stdout)
		}
	}
}

func TestFailsAPluginThatNeverComesOnline(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real plugin process")
	}
	fromTestdata(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--devices", "devices.json", "--timeout", "500ms", "--", "sleep", "30"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	out := stdout.String()
	for _, want := range []string{
		"--- FAIL: announces itself and every declared device",
		"timed out after 500ms waiting for status online with contract 1",
		"\nFAIL\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	fromTestdata(t)
	for name, args := range map[string][]string{
		"no devices":   {"--", "./plugin"},
		"no command":   {"--devices", "devices.json"},
		"unknown flag": {"--nope", "--devices", "devices.json", "--", "./plugin"},
		"bad devices":  {"--devices", "missing.json", "--", "./plugin"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := run(args, &stdout, &stderr)
			want := 2
			if name == "bad devices" {
				want = 1 // the suite reports it, as a setup failure
			}
			if got != want {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", got, want, &stdout, &stderr)
			}
		})
	}
}
