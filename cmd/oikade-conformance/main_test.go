package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

// TestMain removes the fixtures the tests built.
func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// fixture is the absolute path of a fixture binary: simswitch, the test plugin, or home, a home that
// imports it. The first test to ask builds both, with one go build, for every test.
func fixture(t *testing.T, name string) string {
	t.Helper()
	buildOnce.Do(func() {
		if binDir, buildErr = os.MkdirTemp("", "oikconf"); buildErr != nil {
			return
		}
		// From this package's directory, whichever directory a test has moved to.
		_, file, _, _ := runtime.Caller(0)
		cmd := exec.Command("go", "build", "-o", binDir+string(filepath.Separator),
			"./test/fixtures/simswitch", "./test/fixtures/simswitch/home")
		cmd.Dir = filepath.Join(filepath.Dir(file), "..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building the fixtures: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(binDir, name)
}

// fixturePlugin is the simswitch test plugin.
func fixturePlugin(t *testing.T) string {
	t.Helper()
	return fixture(t, "simswitch")
}

// fromTestdata runs the command from the suite's testdata, as a plugin repo runs it from its own
// checkout, so relative paths must resolve against the working directory and not the repository
// root.
func fromTestdata(t *testing.T) {
	t.Helper()
	t.Chdir("../../test/conformance/testdata")
}

func TestPassesAgainstAnImportedPlugin(t *testing.T) {
	bin := fixture(t, "home")
	fromTestdata(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--devices", "devices.json", "--plugin", "simswitch", "--", bin}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
}

func TestPassesAgainstTheFixtureFromAnotherDirectory(t *testing.T) {
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

func TestAnswersASetupFromItsReplies(t *testing.T) {
	bin := fixturePlugin(t)
	fromTestdata(t)
	replies := filepath.Join(t.TempDir(), "replies.json")
	if err := os.WriteFile(replies, []byte(`["Loft hub", "1234", "code=abc"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--devices", "devices.json", "--setup-replies", replies, "--", bin}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	for _, want := range []string{
		"--- PASS: runs its setup, twice",
		"setup conformance-1: Loft hub is set up",
		"the plugin says it needs setup",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, &stdout)
		}
	}
}

func TestFailsAPluginWhoseSetupFails(t *testing.T) {
	bin := fixturePlugin(t)
	devices := filepath.Join(t.TempDir(), "devices.json")
	refused := `{"contract":1,"devices":[{"id":"desk-relay.refused-setup","name":"desk-lamp",` +
		`"displayName":"Desk lamp"}]}`
	if err := os.WriteFile(devices, []byte(refused), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	// 5 s for a loaded machine, as TestFailsAPluginWhoseDiscoveredIDsChange explains.
	code := run([]string{"--devices", devices, "--timeout", "5s", "--", bin}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	out := stdout.String()
	for _, want := range []string{
		"--- FAIL: runs its setup, twice",
		"setup conformance-1 failed: the simulated hub refused the pairing code",
		"--- PASS: goes offline on SIGTERM",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFailsAPluginWhoseDiscoveredIDsChange(t *testing.T) {
	bin := fixturePlugin(t)
	devices := filepath.Join(t.TempDir(), "devices.json")
	unstable := `{"contract":1,"devices":[{"id":"desk-relay.unstable-id","name":"desk-lamp",` +
		`"displayName":"Desk lamp"}]}`
	if err := os.WriteFile(devices, []byte(unstable), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	// A loaded machine can take more than 2 s to bring the plugin online, and the step that
	// fails waits out the whole timeout, so it's 5 s rather than more.
	code := run([]string{"--devices", devices, "--timeout", "5s", "--", bin}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	out := stdout.String()
	for _, want := range []string{
		"--- PASS: goes offline on SIGTERM",
		"--- FAIL: comes back after a restart",
		"the plugin no longer discovers desk-relay.unstable-id-",
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
	bin := fixturePlugin(t)
	devices := filepath.Join(t.TempDir(), "devices.json")
	old := `{"contract":1,"devices":[{"id":"desk-relay.old-name","name":"desk-lamp",` +
		`"displayName":"Desk lamp"}]}`
	if err := os.WriteFile(devices, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	// 5 s for a loaded machine, as TestFailsAPluginWhoseDiscoveredIDsChange explains.
	code := run([]string{"--devices", devices, "--timeout", "5s", "--", bin}, &stdout, &stderr)
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
		"bad replies":  {"--devices", "devices.json", "--setup-replies", "missing.json", "--", "./plugin"},
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
