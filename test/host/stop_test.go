package host

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// signalled stops the host with sig and returns how it exited.
func (r *rig) signalled(sig os.Signal) error {
	r.t.Helper()
	if err := r.cmd.Process.Signal(sig); err != nil {
		r.t.Fatal(err)
	}
	select {
	case err := <-r.exit:
		r.exit <- err
		return err
	case <-time.After(waitLimit):
		r.t.Fatalf("the host didn't stop on %v. The log:\n%s", sig, r.dump())
		return nil
	}
}

// gone waits for process pid to have ended.
func (r *rig) gone(what string, pid int) {
	r.t.Helper()
	r.until(what, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
}

// A stop that can't finish within --stop-timeout is cut off there: the host exits non-zero and kills
// the process it couldn't stop, and the next start on the same data is a clean one.
func TestAStopCutOffAtItsTimeoutKillsWhatIsLeft(t *testing.T) {
	t.Parallel()
	r := prepare(t)
	trapped := filepath.Join(r.dir, "trapped")
	r.env = append(r.env, "OIKADE_TEST_TRAPPED="+trapped)
	r.launch("--stop-timeout=1s")
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.sync()
	automations := r.pid()
	// The home's condition never returns, so its process can't stop when asked, for 5 s.
	r.change("trap", "open", true)
	r.until("the condition to spin", func() bool { _, err := os.Stat(trapped); return err == nil })

	began := time.Now()
	err := r.signalled(syscall.SIGTERM)
	if took := time.Since(began); took > 4*time.Second {
		t.Errorf("the host took %s to stop with --stop-timeout=1s", took)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the host exited with %v, want exit status 1. The log:\n%s", err, r.dump())
	}
	r.waitFor(func(l line) bool {
		return l.is("Cut off host shutdown at its deadline", "phase", "Stopping automations")
	})
	r.gone("the automations process to be killed", automations)

	r.launch("--stop-timeout=1s")
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.sync()
	if err := r.signalled(syscall.SIGTERM); err != nil {
		t.Fatalf("the next start didn't stop cleanly: %v. The log:\n%s", err, r.dump())
	}
}

// systemd stops the host with SIGTERM, and a terminal with SIGINT: either is a clean stop, a phase at
// a time, exiting 0.
func TestSIGTERMStopsTheHostAsSIGINTDoes(t *testing.T) {
	t.Parallel()
	for name, sig := range map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := start(t)
			r.sync()
			automations := r.pid()
			if err := r.signalled(sig); err != nil {
				t.Fatalf("the host exited with %v on %v. The log:\n%s", err, sig, r.dump())
			}
			for _, phase := range []string{
				"Stopping host", "Stopping API", "Stopping automations", "Finishing device writes",
				"Stopping plugins", "Closing store", "Stopped host",
			} {
				r.waitFor(func(l line) bool { return l.is(phase) })
			}
			r.gone("the automations process to stop", automations)
		})
	}
}

// A plugin that fails as it starts is restarted after 1, 2 and 4 s, its wait doubling each time, and
// never sooner.
func TestACrashLoopingPluginIsRestartedWithBackoff(t *testing.T) {
	t.Parallel()
	r := preparePlugins(t)
	if err := os.WriteFile(filepath.Join(r.dir, "plugin-down"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.launchPlugins()
	for _, backoff := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		// waitFor holds the log while it matches, so the checks go after it.
		var restarting, started line
		r.waitFor(func(l line) bool {
			restarting = l
			return l.is(pluginRestarted, "plugin", "relays")
		})
		r.waitFor(func(l line) bool {
			started = l
			return l.is(pluginStarted, "plugin", "relays")
		})
		if got, _ := restarting["backoff"].(string); got != backoff.String() {
			t.Fatalf("restarting with a backoff of %v, want %v. The log:\n%s", restarting["backoff"], backoff, r.dump())
		}
		if waited := timeOf(t, started).Sub(timeOf(t, restarting)); waited < backoff {
			t.Fatalf("started again %v after saying it would wait %v. The log:\n%s", waited, backoff, r.dump())
		}
	}
}
