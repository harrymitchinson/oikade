package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

func TestParseValueTakesNegativeNumbers(t *testing.T) {
	for _, args := range [][]string{
		{"dev", "cap", "-5", "--json"},
		{"--json", "dev", "cap", "-5"},
		{"dev", "--json", "cap", "-5"},
	} {
		fs := flag.NewFlagSet("set", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "")
		got := cli.Parse(fs, args)
		if !slices.Equal(got, []string{"dev", "cap", "-5"}) || !*asJSON {
			t.Errorf("cli.Parse(%v) = %v, json %v", args, got, *asJSON)
		}
	}
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
		{"unarbitrated", &api.Capability{Name: "on", Value: true}, "desk-plug on is true"},
		{"held", &api.Capability{Name: "on", Value: false, Level: "Safety", Claims: []api.Claim{
			{Source: "Rule/hall-too-humid", Level: "Safety", Value: false, Winner: true},
		}}, "desk-plug on is false, not true: Rule/hall-too-humid holds it at Safety"},
		{"for a while", &api.Capability{Name: "on", Value: true, Level: "Override", Claims: []api.Claim{
			{Source: "Command", Level: "Override", Value: true, Winner: true, Expires: &expires},
		}}, "desk-plug on is true: Command holds it at Override until 17:30"},
		{"written back", &api.Capability{Name: "on", Value: false, Level: "Default"},
			"desk-plug on is false, not true: nothing claims it, so its Default wins"},
	} {
		if got := outcome("desk-plug", "on", true, tc.c); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A driver's rounding isn't news: 33.1 is the 33 asked for, but 30 isn't.
func TestSetDoesntCallARoundedValueADifferentOne(t *testing.T) {
	got := outcome("lamp", "brightness", 33.0, &api.Capability{Name: "brightness", Value: 33.1})
	if got != "lamp brightness is 33.1" {
		t.Errorf("got %q", got)
	}
	got = outcome("lamp", "brightness", 33.0, &api.Capability{Name: "brightness", Value: 30.0})
	if got != "lamp brightness is 30, not 33" {
		t.Errorf("got %q", got)
	}
}

func TestSetOnADeviceThatHasntAnsweredSaysSoAndPasses(t *testing.T) {
	got := outcome("desk-plug", "on", true, nil)
	if !strings.Contains(got, "desk-plug hasn't answered yet") {
		t.Errorf("got %q", got)
	}
	if err := done(api.Response{}, false, got); err != nil {
		t.Errorf("a set the device hasn't answered failed: %v", err)
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
	body := streams(`{}` + "\n" + `{"type":"change","device":"hall-lamp","capability":"on","value":true}` + "\n")
	for _, asJSON := range []bool{false, true} {
		var out lockedOutput
		_ = tail(t.Context(), body, "home", asJSON, out.writer("stdout"), out.writer("stderr"))
		lines := out.lines()
		if len(lines) != 2 || lines[0] != "stderr: Following home's events" || !strings.HasPrefix(lines[1], "stdout: ") {
			t.Errorf("json %v: %q", asJSON, lines)
		}
		if asJSON && !strings.HasPrefix(lines[1], `stdout: {"`) {
			t.Errorf("--json printed more than JSON on stdout: %q", lines)
		}
	}
	var out lockedOutput
	_ = tail(t.Context(), body, thisMachine, false, out.writer("stdout"), out.writer("stderr"))
	if lines := out.lines(); len(lines) == 0 || lines[0] != "stderr: Following the events of this machine's host" {
		t.Errorf("on this machine: %q", lines)
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
		{Name: "desk-fan", Kind: "hold", Active: &no, OnAt: at("23:08:00")},
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
	printStatus(&out, io.Discard, st, true, now)
	want := []string{
		"desk-fan  hold  inactive, on at 23:08",
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
	got := describe(api.Event{
		Type: api.EventRefused, Device: "desk-plug", Capability: "on", Value: true,
		Message: "Rule/hall-too-humid holds it at Safety",
	})
	if want := "refused Matter write desk-plug on = true: Rule/hall-too-humid holds it at Safety"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
