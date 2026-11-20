package main

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// exited is a command's exit, which a test's exit panics with so that the command stops there.
type exited int

// runOikade runs oikade with args, printing as printer does at 80 columns, and returns what it
// printed and the code it exits with. It isn't parallel: std and exit are the package's.
func runOikade(t *testing.T, printer func(io.Writer, int) *cli.Printer, args ...string) (out, errs string, code int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	saved, savedExit := std, exit
	t.Cleanup(func() { std, exit = saved, savedExit })
	std.out, std.err = printer(&stdout, 80), printer(&stderr, 80)
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
			if name == "colour" && tc.golden != "help" && tc.golden != "deploy-help" && tc.golden != "unknown-flag" {
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
