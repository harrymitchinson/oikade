package host

import (
	"bufio"
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/test/fixtures/simswitch/relays"
)

func TestSetupRunsAPluginsSetupFromTheCommandLine(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.until("quiet to need its setup", func() bool { return slices.Contains(r.status().NeedsSetup, "quiet") })
	const named = "Plugin quiet needs setup: run oikade setup quiet"
	if stderr, err := r.oikade(nil, "status"); err != nil || !strings.Contains(stderr, named) {
		t.Errorf("status doesn't name the setup: %v: %s", err, stderr)
	}

	cmd := exec.Command(binFor([]string{"setup"}), "setup", "--data-dir", r.dir, "quiet")
	// No browser: the test follows the redirect itself.
	cmd.Env = append(os.Environ(), "BROWSER=true")
	cmd.Stdin = strings.NewReader("Loft hub\npairing-code-7c1f\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(pipe)
	for lines.Scan() {
		stderr.WriteString(lines.Text() + "\n")
		if strings.HasPrefix(lines.Text(), "Waiting for the page") {
			resp, err := http.Get("http://127.0.0.1:8765/callback?code=abc")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("setup: %v\n%s", err, &stderr)
	}
	if got := stdout.String(); got != "Loft hub is set up: 1 relays\n" {
		t.Errorf("stdout: %q\nstderr:\n%s", got, &stderr)
	}
	for _, want := range []string{
		"Starting the setup of quiet on this machine's host",
		"Simulated relays pair with any code",
		"Hub name: Pairing code: ",
		"Opening https://example.com/simswitch/sign-in",
		"Kept secret " + relays.PairingSecret + " on the host",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, &stderr)
		}
	}
	if listed := string(r.stdout(nil, "secret", "list")); !strings.Contains(listed, relays.PairingSecret) {
		t.Errorf("secret list: %s", listed)
	}
	if strings.Contains(r.dump(), "pairing-code-7c1f") {
		t.Error("the host logged the pairing code")
	}
	r.until("quiet to be set up", func() bool { return !slices.Contains(r.status().NeedsSetup, "quiet") })

	// The other plugin has none.
	if stderr, err := r.oikade(nil, "setup", "relays"); err == nil || !strings.Contains(stderr, "relays has no setup") {
		t.Errorf("setup of a plugin with none: %v: %s", err, stderr)
	}
	if stderr, err := r.oikade(nil, "setup", "nope"); err == nil || !strings.Contains(stderr, "there's no plugin nope") {
		t.Errorf("setup of no plugin: %v: %s", err, stderr)
	}
}
