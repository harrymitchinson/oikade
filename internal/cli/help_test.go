package cli

import (
	"bytes"
	"flag"
	"strings"
	"testing"
	"time"
)

func TestClosestIsASlip(t *testing.T) {
	names := []string{"status", "set", "setup", "secret", "deploy", "devices", "tail"}
	for word, want := range map[string]string{
		"statu":  "status",
		"stauts": "status",
		"dev":    "devices",
		"deplyo": "deploy",
		"secrte": "secret",
		"bogus":  "",
		"x":      "",
	} {
		if got := Closest(word, names); got != want {
			t.Errorf("Closest(%q) = %q, want %q", word, got, want)
		}
	}
}

func TestFlagErrorsInTheUsersWords(t *testing.T) {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.Duration("since", time.Hour, "")
	fs.Uint("port", 0, "")
	fs.Bool("json", false, "")
	fs.String("module", ".", "")
	c := Command{Name: "oikade replay"}
	for message, want := range map[string]string{
		"flag provided but not defined: -bogus":                   "oikade replay has no flag --bogus",
		"flag needs an argument: -since":                          "--since needs a value",
		`invalid value "soon" for flag -since: parse error`:       `--since is a duration, such as 90s or 10m, not "soon"`,
		`invalid value "x" for flag -port: parse error`:           `--port is a number, not "x"`,
		`invalid boolean value "maybe" for -json: parse error`:    `--json is true or false, not "maybe"`,
		`invalid value "a" for flag -module: --module is gone`:    "--module is gone",
		"something the flag package hasn't said before, as it is": "something the flag package hasn't said before, as it is",
	} {
		if got := FlagError(c, fs, message); got != want {
			t.Errorf("FlagError(%q) = %q, want %q", message, got, want)
		}
	}
}

func TestHelpNamesFlagValuesAndDefaults(t *testing.T) {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.String("data-dir", "/var/lib/oikade", "The host's data directory.")
	fs.String("module", ".", "Your home's module.")
	fs.Bool("allow-safety-removal", false, "Go ahead.")
	fs.Duration("event-log-age", 336*time.Hour, "How long the log keeps events.")
	fs.Duration("settle", 10*time.Minute, "How long a version must stay up.")
	fs.String("gone", "", "Gone.")
	var b bytes.Buffer
	Plain(&b, 80).CommandHelp(Command{
		Name: "oikade deploy", Summary: "Deploy.", Usage: []string{"oikade deploy [--module DIR] [--allow-safety-removal]"},
		Values: map[string]string{"data-dir": "DIR"}, Hide: []string{"gone"},
	}, fs)
	want := `oikade deploy: Deploy.

Usage:
  oikade deploy [--module DIR] [--allow-safety-removal]

Flags:
  --module DIR              Your home's module. (default .)
  --allow-safety-removal    Go ahead.
  --data-dir DIR            The host's data directory. (default /var/lib/oikade)
  --event-log-age DURATION  How long the log keeps events. (default 336h)
  --settle DURATION         How long a version must stay up. (default 10m)
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestHelpWrapsToTheWidth(t *testing.T) {
	var b bytes.Buffer
	p := Plain(&b, 40)
	got := p.wrap("Where to serve the API over TLS, for clients with a token, such as :8443.", 10)
	for i, line := range strings.Split(got, "\n") {
		if i > 0 && !strings.HasPrefix(line, strings.Repeat(" ", 10)) {
			t.Errorf("line %d isn't indented: %q", i, line)
		}
		if n := width(line); (i == 0 && n > 30) || n > 40 {
			t.Errorf("line %d is %d wide: %q", i, n, line)
		}
	}
}
