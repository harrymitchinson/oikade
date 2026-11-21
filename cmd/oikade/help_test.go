package main

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/testkit"
)

// exited is a command's exit, which a test's exit panics with so that the command stops there.
type exited int

// runOikade runs oikade with args, printing as printer does at 80 columns, and returns what it
// printed and the code it exits with. It isn't parallel: std and exit are the package's.
func runOikade(t *testing.T, printer func(io.Writer, int) *cli.Printer, args ...string) (out, errs string, code int) {
	t.Helper()
	return runOikadeAt(t, printer, 80, args...)
}

// runOikadeAt is runOikade at width columns.
func runOikadeAt(t *testing.T, printer func(io.Writer, int) *cli.Printer, width int, args ...string) (
	out, errs string, code int,
) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	saved, savedExit := std, exit
	t.Cleanup(func() { std, exit = saved, savedExit })
	std.out, std.err = printer(&stdout, width), printer(&stderr, width)
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

func TestHelpAndUsage(t *testing.T) {
	for _, tc := range []struct {
		golden string
		args   []string
		code   int
	}{
		{"help", []string{"help"}, 0},
		{"help", []string{"help", "help"}, 0},
		{"help-to-stderr", nil, cli.ExitUsage},
		{"deploy-help", []string{"deploy", "--help"}, 0},
		{"deploy-help", []string{"help", "deploy"}, 0},
		{"help-device", []string{"help", "device"}, 0},
		{"help-device", []string{"help", "devices"}, 0},
		{"deploy-help", []string{"deploy", "-h"}, 0},
		{"unknown-command", []string{"statu"}, cli.ExitUsage},
		{"unknown-command-bogus", []string{"bogus"}, cli.ExitUsage},
		{"unknown-command-bogus", []string{"help", "bogus"}, cli.ExitUsage},
		{"unknown-flag", []string{"deploy", "--bogus"}, cli.ExitUsage},
		{"flag-needs-a-value", []string{"replay", "--since"}, cli.ExitUsage},
		{"bad-flag-value", []string{"pair", "matter", "--for", "soon"}, cli.ExitUsage},
		{"usage-error", []string{"set", "desk-plug"}, cli.ExitUsage},
	} {
		for name, printer := range printers {
			if name == "colour" && !slices.Contains([]string{"help", "help-device", "deploy-help", "unknown-flag"}, tc.golden) {
				continue
			}
			stdout, stderr, code := runOikade(t, printer, tc.args...)
			if code != tc.code {
				t.Errorf("oikade %s exits %d, want %d", strings.Join(tc.args, " "), code, tc.code)
			}
			testkit.Golden(t, fmt.Sprintf("%s-%s", tc.golden, name),
				fmt.Appendf(nil, "exit %d\n--- stdout\n%s--- stderr\n%s", code, stdout, stderr))
		}
	}
}

// unnamedValue is a flag in help whose value is named for nothing but its type.
var unnamedValue = regexp.MustCompile(`(?m)^  --\S+ VALUE `)

// Every command has help, and every one that takes flags lists them.
func TestEveryCommandHasHelp(t *testing.T) {
	for _, word := range words() {
		if word == "help" {
			continue
		}
		stdout, stderr, code := runOikade(t, cli.Plain, word, "--help")
		if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "oikade "+word+": ") ||
			!strings.Contains(stdout, "\nUsage:\n  oikade "+word) {
			t.Errorf("oikade %s --help exits %d:\n%s%s", word, code, stdout, stderr)
		}
		if unnamedValue.MatchString(stdout) {
			t.Errorf("oikade %s --help leaves a flag's value unnamed:\n%s", word, stdout)
		}
	}
	runners := runners()
	for _, word := range words() {
		if _, ok := runners[word]; !ok && word != "help" {
			t.Errorf("nothing runs oikade %s", word)
		}
	}
	if len(runners) != len(words())-1 {
		t.Errorf("%d runners for %d commands", len(runners), len(words())-1)
	}
}

// The help of a deploy, a deploy given both --keep-vars and --env-file, and what a deploy refused for
// a value says, with and without --keep-vars, at 80 and 120 columns.
func TestTheKeepVarsHelpUsageAndHints(t *testing.T) {
	host := startRecordingHost(t, api.Response{
		Error:       "the version uses TOSHIBA_PASSWORD, which isn't among the host's values",
		MissingVars: []string{"TOSHIBA_PASSWORD"},
	})
	var err, lacked error
	// In the module, so the file is named as you'd type it, and back here for the goldens.
	t.Run("in the module", func(t *testing.T) {
		t.Chdir(homeWithValues(t, ""))
		_, err = readValues(".", "", []string{"TOSHIBA_PASSWORD"}, func(string) (string, bool) { return "", false })
		_, lacked = keepingValues(t, host, ".")
	})
	if err == nil || lacked == nil {
		t.Fatalf("got %v and %v", err, lacked)
	}
	for name, printer := range printers {
		for _, width := range []int{80, 120} {
			var b bytes.Buffer
			stdout, stderr, code := runOikadeAt(t, printer, width, "deploy", "--help")
			if code != 0 || stderr != "" {
				t.Errorf("oikade deploy --help exits %d: %s", code, stderr)
			}
			b.WriteString(stdout + "---\n")
			_, stderr, code = runOikadeAt(t, printer, width, "deploy", "--keep-vars", "--env-file", "test-host.env")
			if code != cli.ExitUsage {
				t.Errorf("oikade deploy --keep-vars --env-file exits %d", code)
			}
			b.WriteString(stderr + "---\n")
			p := printer(&b, width)
			p.Fail(err, false)
			p.Fail(lacked, false)
			testkit.Golden(t, fmt.Sprintf("keep-vars-%s-%d", name, width), b.Bytes())
		}
	}
}

// The help of the commands that give your code your values, and the warning for one that nothing
// gives, at 80 and 120 columns.
func TestTheValuesHelpAndWarning(t *testing.T) {
	for name, printer := range printers {
		for _, width := range []int{80, 120} {
			var b bytes.Buffer
			for _, command := range []string{"check", "dev"} {
				stdout, stderr, code := runOikadeAt(t, printer, width, command, "--help")
				if code != 0 || stderr != "" {
					t.Errorf("oikade %s --help exits %d: %s", command, code, stderr)
				}
				b.WriteString(stdout + "---\n")
			}
			p := printer(&b, width)
			v := values{file: ".env"}
			v.warnMissing(p, []string{"TAHOMA_PIN"}, "a deploy from here would stop")
			v.warnMissing(p, []string{"TAHOMA_PIN", "TOSHIBA_USER"}, `oikade.Var gives "" for them here`)
			testkit.Golden(t, fmt.Sprintf("values-%s-%d", name, width), b.Bytes())
		}
	}
}
