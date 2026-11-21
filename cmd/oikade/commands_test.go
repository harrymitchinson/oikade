package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
)

// cli.Parse keeps a negative number as an argument, which internal/cli tests. parseValue reads it.
func TestParseValueTakesNegativeNumbers(t *testing.T) {
	if v := parseValue("-5"); v != -5.0 {
		t.Errorf("got %v", v)
	}
}

func TestSetSaysWhatTheDeviceWasLeftAt(t *testing.T) {
	expires := time.Date(2026, 11, 8, 17, 30, 0, 0, time.Local)
	for _, tc := range []struct {
		name string
		c    *api.Capability
		want string
	}{
		{"unarbitrated", &api.Capability{Name: "on", Value: true}, "device/desk-plug/on is true"},
		{"held", &api.Capability{Name: "on", Value: false, Level: "Safety", Case: "hall-too-humid", Claims: []api.Claim{
			{Source: "case/desk-plug/on/hall-too-humid", Case: "hall-too-humid", Level: "Safety", Value: false, Winner: true},
		}}, "device/desk-plug/on is false, not true: case/desk-plug/on/hall-too-humid holds it"},
		{"for a while", &api.Capability{Name: "on", Value: true, Level: "Override", Claims: []api.Claim{
			{Source: "command", Level: "Override", Value: true, Winner: true, Expires: &expires},
		}}, "device/desk-plug/on is true: oikade set holds it until 17:30"},
		{"a flow", &api.Capability{Name: "on", Value: true, Level: "Normal", Claims: []api.Claim{
			{Source: "flow/bedtime", Level: "Normal", Value: true, Winner: true},
		}}, "device/desk-plug/on is true: flow/bedtime holds it"},
		{"held back", &api.Capability{
			Name: "on", Value: false, Level: "Safety", Limit: "MinimumOff", LimitUntil: &expires,
			Claims: []api.Claim{{Source: "case/desk-plug/on/frost", Case: "frost", Level: "Safety", Value: true, Winner: true}},
		}, "device/desk-plug/on is false, not true: case/desk-plug/on/frost holds it, resting until 17:30"},
		{"written back", &api.Capability{Name: "on", Value: false, Level: "Default"},
			"device/desk-plug/on is false, not true: no line applies, so its Default wins"},
	} {
		if got := outcome("desk-plug", "on", true, tc.c); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A driver's rounding isn't news: 33.1 is the 33 asked for, but 30 isn't.
func TestSetDoesntCallARoundedValueADifferentOne(t *testing.T) {
	got := outcome("lamp", "brightness", 33.0, &api.Capability{Name: "brightness", Value: 33.1})
	if got != "device/lamp/brightness is 33.1%" {
		t.Errorf("got %q", got)
	}
	got = outcome("lamp", "brightness", 33.0, &api.Capability{Name: "brightness", Value: 30.0})
	if got != "device/lamp/brightness is 30%, not 33%" {
		t.Errorf("got %q", got)
	}
}

func TestPauseNamesTheClaimsItEnded(t *testing.T) {
	got := paused("Paused", "hall-fan", api.Response{Ended: []string{"fan on", "lamp on"}, Kept: []string{"valve on"}})
	want := "Paused hall-fan, and ended its claims on fan on, lamp on. " +
		"It keeps its Safety claims on valve on, which only a release ends"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := paused("Paused", "hall-fan", api.Response{}); got != "Paused hall-fan. It had no claims to end" {
		t.Errorf("with no claims: %q", got)
	}
}

// streams is a host whose tail sends body.
type streams string

func (s streams) Dial(context.Context) (net.Conn, error) {
	client, host := net.Pipe()
	go func() {
		defer func() { _ = host.Close() }()
		if _, err := http.ReadRequest(bufio.NewReader(host)); err != nil {
			return
		}
		_, _ = io.WriteString(host, "HTTP/1.1 200 OK\r\nContent-Type: "+api.StreamContentType+
			"\r\nConnection: close\r\n\r\n"+string(s))
	}()
	return client, nil
}

func TestTailSaysItHasStartedBeforeAnyEvent(t *testing.T) {
	now := time.Now()
	body := streams(`{}` + "\n" + `{"time":"` + now.Format(time.RFC3339) +
		`","type":"change","device":"hall-lamp","capability":"on","value":true}` + "\n")
	for _, asJSON := range []bool{false, true} {
		var out lockedOutput
		_ = tail(t.Context(), body, "home", api.EventFilter{}, nil, asJSON, now, out.writer("stdout"), out.writer("stderr"))
		lines := out.lines()
		if len(lines) != 2 || lines[0] != "stderr: Following home's events" || !strings.HasPrefix(lines[1], "stdout: ") {
			t.Errorf("json %v: %q", asJSON, lines)
		}
		if asJSON && !strings.HasPrefix(lines[1], `stdout: {"`) {
			t.Errorf("--json printed more than JSON on stdout: %q", lines)
		}
	}
	var out lockedOutput
	_ = tail(t.Context(), body, thisMachine, api.EventFilter{}, nil, false, now, out.writer("stdout"),
		out.writer("stderr"))
	if lines := out.lines(); len(lines) == 0 || lines[0] != "stderr: Following the events of this machine's host" {
		t.Errorf("on this machine: %q", lines)
	}
	// Of some refs, it says which.
	lamp := []named{{ref: "device/hall-lamp"}, {ref: "flow/morning"}}
	for host, want := range map[string]string{
		"home":      "stderr: Following device/hall-lamp, flow/morning on home",
		thisMachine: "stderr: Following device/hall-lamp, flow/morning on this machine's host",
	} {
		var out lockedOutput
		_ = tail(t.Context(), body, host, api.EventFilter{}, lamp, false, now, out.writer("stdout"), out.writer("stderr"))
		if lines := out.lines(); len(lines) == 0 || lines[0] != want {
			t.Errorf("on %s: %q, want %q", host, lines, want)
		}
	}
}

// lockedOutput keeps stdout's and stderr's lines in the order they were written.
type lockedOutput struct {
	mu  sync.Mutex
	all []string
}

func (o *lockedOutput) writer(name string) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		for l := range strings.SplitSeq(strings.TrimSuffix(string(p), "\n"), "\n") {
			o.all = append(o.all, name+": "+l)
		}
		return len(p), nil
	})
}

func (o *lockedOutput) lines() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.all)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestStatusSaysWhatEachHoldIsDoing(t *testing.T) {
	now := time.Date(2026, 11, 8, 23, 7, 30, 0, time.Local)
	at := func(hms string) time.Time {
		v, err := time.ParseInLocation(time.TimeOnly, hms, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(now.Year(), now.Month(), now.Day(), v.Hour(), v.Minute(), v.Second(), 0, time.Local)
	}
	yes, no := true, false
	st := &api.Status{Rules: []api.Rule{
		{Name: "desk-fan", Ref: "flow/desk-fan", Kind: "hold", Active: &no, OnAt: at("23:08:00")},
		{
			Name: "pool-pump on frost", Ref: "case/pool-pump/on/frost", Kind: "case", Device: "pool-pump", Capability: "on",
			Case: "frost", Safety: true,
			Active: &no, WhyNot: "device/pool-water/temperature is 5, not below 3",
		},
		{Name: "gate", Kind: "moment"},
		{Name: "hall-heater", Kind: "hold", Active: &no, Stopped: "no device in room hall has reported occupancy"},
		{Name: "lounge-lamp", Kind: "hold", Active: &yes, Since: at("23:07:05"), OffAt: at("23:10:05")},
		{Name: "porch-light", Kind: "hold", Active: &yes, Since: at("21:00:00"), Latched: true},
		{Name: "towel-rail", Kind: "hold", Active: &no, Budget: &api.RuleBudget{
			Date: "2026-11-08", TargetMinutes: 120, PlannedMinutes: 90, RunMinutes: 45,
		}},
		{Name: "aquarium", Kind: "hold", Paused: true},
		{Name: "study-heater", Kind: "hold", Active: &yes, Since: at("08:00:00").AddDate(0, 0, -1), OffAt: at("23:00:00")},
		{Name: "garden-lights", Kind: "hold", Active: &no, OnAt: at("06:30:00").AddDate(0, 0, 1)},
	}}
	var out bytes.Buffer
	printStatus(cli.Plain(&out, 0), cli.Plain(io.Discard, 0), st, false, now)
	want := []string{
		"flow/desk-fan  hold  inactive, on at 23:08",
		"case/pool-pump/on/frost  case  Safety  inactive: device/pool-water/temperature is 5, not below 3",
		"gate  moment",
		"hall-heater  hold  inactive  stopped: no device in room hall has reported occupancy",
		"lounge-lamp  hold  active since 23:07, off at 23:10",
		"porch-light  hold  active since 21:00, latched",
		"towel-rail  hold  inactive, 2026-11-08: planned 90 of 120 min, run 45",
		"aquarium  hold  paused",
		"study-heater  hold  active since 2026-11-07 08:00:00, off at 23:00 (passed)",
		"garden-lights  hold  inactive, on at 2026-11-09 06:30:00",
	}
	for _, line := range want {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("no line %q in:\n%s", line, out.String())
		}
	}
}

func TestTailShowsARefusedWrite(t *testing.T) {
	got := eventLine(cli.Plain(io.Discard, 0), api.Event{
		Time: time.Date(2026, 11, 18, 14, 2, 7, 0, time.Local),
		Type: api.EventRefused, Device: "desk-plug", Capability: "on", Value: true, Ref: "device/desk-plug/on",
		Message: "case/desk-plug/on/hall-too-humid holds it at Safety",
	})
	want := "14:02:07  device/desk-plug/on  refused a Matter write of true  " +
		"case/desk-plug/on/hall-too-humid holds it at Safety"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPauseByThreeWordsGivesTheCasesRef(t *testing.T) {
	err := runPause(t.Context(), []string{"desk-plug", "on", "night"}, true)
	if err == nil || err.Error() != "a case is one ref: oikade pause case/desk-plug/on/night" {
		t.Errorf("err = %v", err)
	}
	// Words that make no ref are refused for what's wrong with them, rather than with a ref that fails.
	err = runPause(t.Context(), []string{"Desk-Plug", "on", "night"}, false)
	if err == nil || !strings.Contains(err.Error(), `"Desk-Plug" isn't a device name`) {
		t.Errorf("err = %v", err)
	}
}

// A set fails, with exit 1, when something holds the capability at another value, and says what lets
// go of it: pausing the rule, its release for a Safety one, or its limits running out. One whose
// device hasn't answered yet was sent, so it doesn't fail.
func TestSetFailsWhenItsHeldElsewhere(t *testing.T) {
	until := time.Date(2026, 11, 20, 17, 30, 0, 0, time.Local)
	for _, tc := range []struct {
		name string
		resp api.Response
		code int
		says string
	}{
		{"done", api.Response{Outcome: &api.Capability{Name: "on", Value: true}}, 0,
			"device/desk-plug/on is true\n"},
		{"not answered", api.Response{}, 0,
			"device/desk-plug/on hasn't answered yet: oikade status desk-plug shows it when it does\n"},
		{"a flow holds it", api.Response{Outcome: &api.Capability{Name: "on", Value: false, Level: "Normal",
			Claims: []api.Claim{{Source: "flow/bedtime", Level: "Normal", Value: false, Winner: true}}}}, 1,
			"Error: device/desk-plug/on is false, not true: flow/bedtime holds it\n" +
				"Hint: oikade pause flow/bedtime lets go of it until you resume it\n"},
		{"a Safety case refused it", api.Response{
			Error: "device/desk-plug/on: case/desk-plug/on/leak holds it, a safety line",
			Outcome: &api.Capability{Name: "on", Value: false, Level: "Safety", Claims: []api.Claim{
				{Source: "case/desk-plug/on/leak", Case: "leak", Level: "Safety", Value: false, Winner: true}}},
		}, 1, "Error: device/desk-plug/on: case/desk-plug/on/leak holds it, a safety line\n" +
			"Hint: a Safety claim ends only when its rule releases it, as a pause keeps it: oikade status " +
			"case/desk-plug/on/leak says what it's waiting for\n"},
		{"its limits hold it", api.Response{Outcome: &api.Capability{
			Name: "on", Value: false, Level: "Override", Limit: "MinimumOff", LimitUntil: &until,
			Claims: []api.Claim{{Source: "command", Level: "Override", Value: true, Winner: true}},
		}}, 1, "Error: device/desk-plug/on is false, not true: oikade set holds it, resting until 17:30\n" +
			"Hint: its limits let go when they're done: oikade status device/desk-plug/on says when\n"},
		{"written back", api.Response{Outcome: &api.Capability{Name: "on", Value: false, Level: "Default"}}, 1,
			"Error: device/desk-plug/on is false, not true: no line applies, so its Default wins\n" +
				"Hint: oikade status device/desk-plug/on says why\n"},
	} {
		dir := aSetHost(t, tc.resp)
		out, errs, code := runOikade(t, cli.Plain, "set", "device/desk-plug/on", "true", "--data-dir", dir)
		errs = strings.TrimPrefix(errs, "Setting device/desk-plug/on to true\n")
		if code != tc.code || out+errs != tc.says {
			t.Errorf("%s: exit %d:\n%s%s\nwant exit %d:\n%s", tc.name, code, out, errs, tc.code, tc.says)
		}
		out, _, _ = runOikade(t, cli.Plain, "set", "device/desk-plug/on", "true", "--json", "--data-dir", dir)
		if ok := strings.HasPrefix(out, `{"ok":true`); ok != (tc.code == 0) {
			t.Errorf("%s: --json says %s", tc.name, out)
		}
	}
}

// aSetHost is a host that answers every set with resp.
func aSetHost(t *testing.T, resp api.Response) (dir string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "oik") //nolint:usetesting // a Unix socket's path has to fit in about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, api.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+api.Prefix+"/set", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.ContentType)
		_ = json.NewEncoder(w).Encode(resp)
	})
	host := &http.Server{Handler: mux}
	go func() { _ = host.Serve(l) }()
	t.Cleanup(func() { _ = host.Close() })
	return dir
}
