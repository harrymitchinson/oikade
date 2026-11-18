package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
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
		{"client-add-help", []string{"client", "add", "--help"}, 0},
		{"client-add-help", []string{"help", "client", "add"}, 0},
		{"unknown-command", []string{"instal"}, cli.ExitUsage},
		{"unknown-command-deploy", []string{"deploy"}, cli.ExitUsage},
		{"unknown-flag", []string{"install", "--bogus"}, cli.ExitUsage},
		{"bad-flag-value", []string{"--settle", "soon"}, cli.ExitUsage},
		{"usage-error", []string{"client", "list"}, cli.ExitUsage},
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
	// Anything else, such as a secret it may not read, is as it was.
	other := &fs.PathError{Op: "open", Path: "/etc/oikade/secrets/broker", Err: fs.ErrPermission}
	if got := dataDirError(other, "/var/lib/oikade"); !errors.Is(got, other) || got != error(other) {
		t.Errorf("got %v", got)
	}
}
