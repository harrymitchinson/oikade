package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/testkit"
)

// sample prints a table and every kind of line, as a command would.
func sample(p *Printer) {
	dropped := p.Table(Table{
		Columns: []Column{
			{Header: "NAME"},
			{Header: "ROOM"},
			{Header: "STATE", Shrink: true},
			{Header: "HARDWARE", Drop: 1, Dim: true},
			{Header: "ENDPOINT", Drop: 2},
			{Header: "CAPABILITIES", Shrink: true},
		},
		Rows: [][]string{
			{"hall-lamp", "hall", "paired", "hardware/ewelink-lan/1000aabbcc", "2", "on"},
			{"study-blind", "study", "paired, not in your code", "hardware/overkiz/io:%2F%2F0812-4471-2093%2F5", "3",
				"position, target-position, moving"},
			{"office-ac", "", "no LAN key: oikade setup ewelink-lan", "hardware/toshiba/5f1c0c2e-77b3-4f8e-9d4a-1b2c3d4e5f60",
				"4", "mode, on, setpoint, temperature, fan-speed, swing"},
		},
	})
	if len(dropped) > 0 {
		p.Hint("--wide shows %s", strings.Join(dropped, " and "))
	}
	p.Done("Deployed version %s", ShortVersion("f113018fb81c06e4d5fa3500102bfbc4"))
	p.Warn("the broker's password isn't set")
	p.Fail(UsageErrorf("device/salon/on takes true or false, not maybe").WithHint("oikade status salon shows it"), false)
	p.Fail(Wrap(&fs.PathError{Op: "mkdir", Path: "/var/lib/oikade", Err: fs.ErrPermission},
		"the host can't write its data directory, /var/lib/oikade").WithHint("run it as root, or pass --data-dir"), true)
}

func TestTheLook(t *testing.T) {
	for _, width := range []int{80, 120} {
		for name, printer := range map[string]func(*bytes.Buffer, int) *Printer{
			"plain":  func(b *bytes.Buffer, w int) *Printer { return Plain(b, w) },
			"colour": func(b *bytes.Buffer, w int) *Printer { return Styled(b, w) },
		} {
			var b bytes.Buffer
			sample(printer(&b, width))
			testkit.Golden(t, fmt.Sprintf("look-%s-%d", name, width), b.Bytes())
		}
	}
}

func TestWideKeepsEveryColumn(t *testing.T) {
	var b bytes.Buffer
	dropped := Plain(&b, 20).Table(Table{
		Columns: []Column{{Header: "NAME"}, {Header: "HARDWARE", Drop: 1}, {Header: "CAPABILITIES", Shrink: true}},
		Rows:    [][]string{{"hall-lamp", "hardware/ewelink-lan/1000aabbcc", "on, brightness, colour-temperature"}},
		Wide:    true,
	})
	if len(dropped) != 0 || !strings.Contains(b.String(), "hardware/ewelink-lan/1000aabbcc  on, brightness, colour-temperature") {
		t.Errorf("dropped %v:\n%s", dropped, b.String())
	}
}

func TestATableLeavesNoTrailingSpaceOrDash(t *testing.T) {
	var b bytes.Buffer
	Plain(&b, 80).Table(Table{
		Columns: []Column{{Header: "NAME"}, {Header: "ROOM"}},
		Rows:    [][]string{{"porch-light", ""}, {"kettle", "kitchen"}},
	})
	want := "NAME         ROOM\nporch-light\nkettle       kitchen\n"
	if b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}
}

func TestNoColourOffATerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		env      map[string]string
		styled   bool
	}{
		{"a terminal", true, nil, true},
		{"piped", false, nil, false},
		{"NO_COLOR", true, map[string]string{"NO_COLOR": "1"}, false},
		{"TERM=dumb", true, map[string]string{"TERM": "dumb"}, false},
	} {
		var b bytes.Buffer
		p := newPrinter(&b, tc.terminal, 0, func(k string) string { return tc.env[k] })
		if wantWidth := map[bool]int{true: DefaultWidth}[tc.terminal]; p.Styled != tc.styled || p.Width != wantWidth {
			t.Errorf("%s: styled %v, width %d", tc.name, p.Styled, p.Width)
		}
		sample(p)
		if got := strings.Contains(b.String(), "\x1b") || strings.ContainsAny(b.String(), "✓✗"); got != tc.styled {
			t.Errorf("%s: escape codes or glyphs %v:\n%s", tc.name, got, b.String())
		}
	}
	p := newPrinter(nil, false, 0, func(k string) string { return map[string]string{"COLUMNS": "132"}[k] })
	if p.Width != 132 {
		t.Errorf("COLUMNS=132 is %d wide", p.Width)
	}
}

func TestFailExitsTwoForUsageAndShowsTheCauseOnlyWithDebug(t *testing.T) {
	cause := errors.New("fork/exec /var/folders/x/home: permission denied")
	for _, tc := range []struct {
		err   error
		debug bool
		code  int
		want  string
	}{
		{UsageErrorf("oikade set takes a value"), false, ExitUsage, "Error: oikade set takes a value\n"},
		{Wrap(cause, "there's no home here"), false, ExitFailed, "Error: there's no home here\n"},
		{
			fmt.Errorf("replaying: %w", Wrap(cause, "there's no home here").WithHint("cd into one, or pass --module")),
			true, ExitFailed,
			"Error: there's no home here\nHint: cd into one, or pass --module\n" +
				"Debug: replaying: there's no home here: fork/exec /var/folders/x/home: permission denied\n",
		},
		{cause, false, ExitFailed, "Error: fork/exec /var/folders/x/home: permission denied\n"},
		// The command has said what's wrong.
		{fmt.Errorf("checking: %w", Silent()), true, ExitFailed, ""},
	} {
		var b bytes.Buffer
		if code := Plain(&b, 80).Fail(tc.err, tc.debug); code != tc.code || b.String() != tc.want {
			t.Errorf("Fail(%v, %v) = %d:\n%s\nwant %d:\n%s", tc.err, tc.debug, code, b.String(), tc.code, tc.want)
		}
	}
}

func TestDebugComesFromTheFlagOrTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		rest  string
		debug bool
	}{
		{[]string{"replay", "--debug", "--since", "1h", "--", "--debug"}, "replay --since 1h -- --debug", true},
		{[]string{"-debug", "status", "porch-light"}, "status porch-light", true},
		{[]string{"status", "--debug=false"}, "status", false},
		// What oikade test hands go test is go test's.
		{[]string{"test", "--debug", "./...", "-args", "-debug"}, "test ./... -args -debug", true},
		{[]string{"test", "./...", "-args", "-debug"}, "test ./... -args -debug", false},
	} {
		rest, debug := Debug(tc.args, "test")
		if debug != tc.debug || strings.Join(rest, " ") != tc.rest {
			t.Errorf("Debug(%q) = %q, %v", tc.args, rest, debug)
		}
	}
	t.Setenv("OIKADE_DEBUG", "1")
	if _, debug := Debug([]string{"status"}); !debug {
		t.Error("OIKADE_DEBUG=1 isn't debug")
	}
}

func TestAgoIsRelativeNearNow(t *testing.T) {
	loc := testkit.Location("Europe/Paris")
	now := time.Date(2026, 11, 18, 14, 30, 0, 0, loc)
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{now, "just now"},
		{now.Add(-8 * time.Second), "8 s ago"},
		{now.Add(-12 * time.Minute), "12 min ago"},
		{time.Date(2026, 11, 18, 8, 45, 0, 0, loc), "today 08:45"},
		{time.Date(2026, 11, 17, 21, 10, 0, 0, loc), "yesterday 21:10"},
		{time.Date(2026, 11, 15, 9, 0, 0, 0, loc), "3 days ago"},
		{time.Date(2026, 11, 10, 8, 45, 53, 0, loc), "2026-11-10 08:45"},
		{now.Add(time.Hour), "2026-11-18 15:30"},
		// The host's clock a little ahead of this one's.
		{now.Add(20 * time.Second), "just now"},
		// In now's time zone, whatever t's.
		{time.Date(2026, 11, 18, 7, 45, 0, 0, time.UTC), "today 08:45"},
	} {
		if got := Ago(tc.t, now); got != tc.want {
			t.Errorf("Ago(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestDayNamesTheDayAndSaysTodayAndYesterday(t *testing.T) {
	loc := testkit.Location("Europe/Paris")
	now := time.Date(2026, 11, 18, 14, 30, 0, 0, loc)
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{now, "Wed 18 Nov, today"},
		{time.Date(2026, 11, 18, 0, 0, 0, 0, loc), "Wed 18 Nov, today"},
		{time.Date(2026, 11, 17, 23, 59, 59, 0, loc), "Tue 17 Nov, yesterday"},
		{time.Date(2026, 11, 9, 8, 0, 0, 0, loc), "Mon 9 Nov"},
		{time.Date(2025, 12, 31, 8, 0, 0, 0, loc), "Wed 31 Dec 2025"},
		// In now's time zone, whatever t's: late on the 17th in UTC is already the 18th here.
		{time.Date(2026, 11, 17, 23, 30, 0, 0, time.UTC), "Wed 18 Nov, today"},
	} {
		if got := Day(tc.t, now); got != tc.want {
			t.Errorf("Day(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestShortVersion(t *testing.T) {
	for v, want := range map[string]string{
		"f113018fb81c06e4d5fa3500102bfbc4": "f113018",
		"v0.2.0-alpha.2":                   "v0.2.0-alpha.2",
		"dev":                              "dev",
	} {
		if got := ShortVersion(v); got != want {
			t.Errorf("ShortVersion(%s) = %s, want %s", v, got, want)
		}
	}
}
