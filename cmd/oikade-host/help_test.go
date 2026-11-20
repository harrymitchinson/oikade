package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// exited is a command's exit, which a test's exit panics with so that the command stops there.
type exited int

// runHostCommand runs oikade-host with args, and returns what it printed and the code it exits
// with. It isn't parallel: std and exit are the package's.
func runHostCommand(t *testing.T, args ...string) (out, errs string, code int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	saved, savedExit := std, exit
	t.Cleanup(func() { std, exit = saved, savedExit })
	std.out, std.err = cli.Plain(&stdout, 80), cli.Plain(&stderr, 80)
	exit = func(code int) { panic(exited(code)) }
	func() {
		defer func() {
			if r := recover(); r != nil {
				e, ok := r.(exited)
				if !ok {
					panic(r)
				}
				code = int(e)
			}
		}()
		code = dispatch(t.Context(), args, false)
	}()
	return stdout.String(), stderr.String(), code
}

func TestHostHelpAndUsage(t *testing.T) {
	for _, tc := range []struct {
		golden string
		args   []string
		code   int
	}{
		{"help", []string{"help"}, 0},
		{"help", []string{"--help"}, 0},
		{"help", []string{"-h"}, 0},
		{"help", []string{"help", "help"}, 0},
		{"add-help", []string{"add", "--help"}, 0},
		{"add-help", []string{"help", "add"}, 0},
		{"unknown-command", []string{"instal"}, cli.ExitUsage},
		{"unknown-command-deploy", []string{"deploy"}, cli.ExitUsage},
		{"unknown-flag", []string{"install", "--bogus"}, cli.ExitUsage},
		{"bad-flag-value", []string{"--settle", "soon"}, cli.ExitUsage},
		{"usage-error", []string{"add", "device/porch-light"}, cli.ExitUsage},
		{"client-add", []string{"client", "add", "laptop"}, cli.ExitUsage},
		{"secrets-dir", []string{"--secrets-dir", "/etc/oikade/secrets"}, cli.ExitUsage},
	} {
		stdout, stderr, code := runHostCommand(t, tc.args...)
		if code != tc.code {
			t.Errorf("oikade-host %s exits %d, want %d", strings.Join(tc.args, " "), code, tc.code)
		}
		testkit.Golden(t, tc.golden, fmt.Appendf(nil, "exit %d\n--- stdout\n%s--- stderr\n%s", code, stdout, stderr))
	}
}

func TestADataDirectoryTheHostMayNotWriteSaysHowToRunIt(t *testing.T) {
	denied := fmt.Errorf("opening the data directory: %w",
		fmt.Errorf("create data directory: %w", &fs.PathError{Op: "mkdir", Path: "/var/lib/oikade", Err: fs.ErrPermission}))
	var b bytes.Buffer
	code := cli.Plain(&b, 80).Fail(dataDirError(denied, "/var/lib/oikade"), false)
	want := "Error: the host may not write its data directory, /var/lib/oikade\n" +
		"Hint: run it as root, or pass --data-dir DIR for one its user may write\n"
	if code != cli.ExitFailed || b.String() != want {
		t.Errorf("exit %d:\n%s", code, b.String())
	}
	// Its data directory however it's named, and not a directory beside it.
	for dir, want := range map[string]bool{
		"/var/lib/oikade/": true, "/var/lib/../lib/oikade": true, "/var/lib/oik": false,
	} {
		if got := dataDirError(denied, dir) != denied; got != want {
			t.Errorf("--data-dir %s: reworded %v, want %v", dir, got, want)
		}
	}
	// Anything else, such as a secret it may not read, is as it was.
	other := &fs.PathError{Op: "open", Path: "/etc/oikade/secrets/broker", Err: fs.ErrPermission}
	if got := dataDirError(other, "/var/lib/oikade"); !errors.Is(got, other) || got != error(other) {
		t.Errorf("got %v", got)
	}
}

// Nothing sets the broker's password for the host, so a host without it stops and says where it goes.
func TestAHostWithoutItsBrokerPasswordSaysWhereToPutIt(t *testing.T) {
	dir := t.TempDir()
	_, stderr, code := runHostCommand(t, "--data-dir", dir, "--outside-plugins", "kettle",
		"--broker", "mqtt://oikade@127.0.0.1:1", "--broker-password-secret", "broker")
	want := "Error: could not read the broker's password from " + filepath.Join(dir, "secrets", "broker") + "\n" +
		"Hint: put the password in that file, readable only by the host's user, and start the host again\n"
	if code != cli.ExitFailed || stderr != want {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	// The directory is there for the file, as only the host's user can read it.
	if info, err := os.Stat(filepath.Join(dir, "secrets")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the secrets directory: %v, %v", info, err)
	}
}

// A command word of only spaces is an unknown command, not a crash.
func TestABlankCommandIsUnknown(t *testing.T) {
	for _, args := range [][]string{{" "}, {"help", " "}} {
		_, stderr, code := runHostCommand(t, args...)
		if code != cli.ExitUsage || !strings.Contains(stderr, "unknown command") {
			t.Errorf("oikade-host %q exits %d: %s", args, code, stderr)
		}
	}
}
