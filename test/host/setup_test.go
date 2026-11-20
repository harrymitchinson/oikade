package host

import (
	"bufio"
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/harrymitchinson/oikade/test/fixtures/simswitch/relays"
)

// callback is held by the setup that's listening on its page's redirect, 127.0.0.1:8765, which
// only one may at a time.
var callback sync.Mutex

// setupQuiet runs oikade setup quiet as a person would, answering its questions and following its
// page's redirect, and returns what it printed.
func (r *rig) setupQuiet() (stdout, stderr string, err error) {
	r.t.Helper()
	callback.Lock()
	defer callback.Unlock()
	cmd := exec.Command(binFor([]string{"setup"}), "setup", "--data-dir", r.dir, "quiet")
	// No browser: the test follows the redirect itself.
	cmd.Env = append(os.Environ(), "BROWSER=true")
	cmd.Stdin = strings.NewReader("Loft hub\npairing-code-7c1f\n")
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	pipe, err := cmd.StderrPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	lines := bufio.NewScanner(pipe)
	for lines.Scan() {
		errs.WriteString(lines.Text() + "\n")
		if strings.HasPrefix(lines.Text(), "Waiting for the page") {
			resp, err := http.Get("http://127.0.0.1:8765/callback?code=abc")
			if err != nil {
				r.t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	err = cmd.Wait()
	return out.String(), errs.String(), err
}

// keptSecret is what a plugin's setup kept as name, which only the host's user may read.
func (r *rig) keptSecret(name string) string {
	r.t.Helper()
	path := filepath.Join(r.dir, "secrets", name)
	info, err := os.Stat(path)
	if err != nil {
		r.t.Fatalf("setup kept no %s: %v", name, err)
	}
	if info.Mode().Perm() != 0o600 {
		r.t.Errorf("%s has mode %o", name, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the rig's own data directory
	if err != nil {
		r.t.Fatal(err)
	}
	return string(raw)
}

func TestSetupRunsAPluginsSetupFromTheCommandLine(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.until("quiet to need its setup", func() bool { return pluginIn(r.status(), "quiet").NeedsSetup })
	const named = "Plugin quiet needs setup: run oikade setup quiet"
	if stderr, err := r.oikade(nil, "status"); err != nil || !strings.Contains(stderr, named) {
		t.Errorf("status doesn't name the setup: %v: %s", err, stderr)
	}

	stdout, stderr, err := r.setupQuiet()
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, stderr)
	}
	if stdout != "Loft hub is set up: 1 relays\n" {
		t.Errorf("stdout: %q\nstderr:\n%s", stdout, stderr)
	}
	for _, want := range []string{
		"Starting the setup of quiet on this machine's host",
		"Simulated relays pair with any code",
		"Hub name: Pairing code: ",
		"Opening https://example.com/simswitch/sign-in",
		"Kept secret " + relays.PairingSecret + " on the host",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	kept := r.keptSecret(relays.PairingSecret)
	if kept != "pairing-code-7c1f" {
		t.Errorf("setup kept %q", kept)
	}
	if strings.Contains(r.dump(), "pairing-code-7c1f") {
		t.Error("the host logged the pairing code")
	}
	r.until("quiet to be set up", func() bool { return !pluginIn(r.status(), "quiet").NeedsSetup })

	// A deploy starts the plugins again, from the new version, and leaves what setup kept for them.
	r.mustDeploy("plugins-landing")
	r.awaitCount(1, "Read kept secret", "variant", "plugins-landing", "sha", digest(kept))
	if got := r.keptSecret(relays.PairingSecret); got != kept {
		t.Errorf("after a deploy, setup's secret is %q", got)
	}

	// The other plugin has none.
	if stderr, err := r.oikade(nil, "setup", "relays"); err == nil || !strings.Contains(stderr, "relays has no setup") {
		t.Errorf("setup of a plugin with none: %v: %s", err, stderr)
	}
	if stderr, err := r.oikade(nil, "setup", "nope"); err == nil || !strings.Contains(stderr, "there's no plugin nope") {
		t.Errorf("setup of no plugin: %v: %s", err, stderr)
	}
}
