package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixturePlugin builds the simswitch test fixture and returns its absolute path.
func fixturePlugin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "simswitch")
	out, err := exec.Command("go", "build", "-o", bin, "../../test/fixtures/simswitch").CombinedOutput()
	if err != nil {
		t.Fatalf("building simswitch: %v\n%s", err, out)
	}
	return bin
}

// fromTestdata runs the command from the suite's testdata, as a plugin repo runs it from its own
// checkout, so relative paths must resolve against the working directory and not the repository
// root.
func fromTestdata(t *testing.T) {
	t.Helper()
	t.Chdir("../../test/conformance/testdata")
}

func TestPassesAgainstAnImportedPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real plugin process")
	}
	bin := filepath.Join(t.TempDir(), "simhome")
	out, err := exec.Command("go", "build", "-o", bin, "../../test/fixtures/simswitch/home").CombinedOutput()
	if err != nil {
		t.Fatalf("building the home: %v\n%s", err, out)
	}
	fromTestdata(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--devices", "devices.json", "--plugin", "simswitch", "--", bin}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
}

func TestPassesAgainstTheFixtureFromAnotherDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real plugin process")
	}
	bin := fixturePlugin(t)
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

func TestFailsAPluginWhoseDiscoveredIDsChange(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real plugin process")
	}
	bin := fixturePlugin(t)
	devices := filepath.Join(t.TempDir(), "devices.json")
	unstable := `{"contract":1,"devices":[{"id":"desk-relay","name":"desk-lamp",` +
		`"displayName":"Desk lamp","parameters":{"simulate":"unstable-id"}}]}`
	if err := os.WriteFile(devices, []byte(unstable), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--devices", devices, "--timeout", "2s", "--", bin}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	out := stdout.String()
	for _, want := range []string{
		"--- PASS: goes offline on SIGTERM",
		"--- FAIL: comes back after a restart",
		"the plugin no longer discovers desk-relay-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "spare-relay") {
		t.Errorf("output blames spare-relay, whose ID is stable:\n%s", out)
	}
}

func TestFailsAPluginThatNamesAnOldCapability(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real plugin process")
	}
	bin := fixturePlugin(t)
	devices := filepath.Join(t.TempDir(), "devices.json")
	old := `{"contract":1,"devices":[{"id":"desk-relay","name":"desk-lamp",` +
		`"displayName":"Desk lamp","parameters":{"simulate":"old-name"}}]}`
	if err := os.WriteFile(devices, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--devices", devices, "--timeout", "2s", "--", bin}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	// The SDK refuses it before it reaches the wire.
	if out := stdout.String(); !strings.Contains(out, "unknown capability switch.on") {
		t.Errorf("output doesn't say the capability is unknown:\n%s", out)
	}
}

func TestFailsAPluginThatCannotStart(t *testing.T) {
	fromTestdata(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--devices", "devices.json", "--", "./no-such-plugin"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	if out := stdout.String(); !strings.Contains(out, "--- FAIL: suite") || strings.Contains(out, "--- PASS") {
		t.Errorf("want only the suite to fail, before any check:\n%s", out)
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
			if got != 2 {
				t.Errorf("exit %d, want 2\nstdout:\n%s\nstderr:\n%s", got, &stdout, &stderr)
			}
		})
	}
}
