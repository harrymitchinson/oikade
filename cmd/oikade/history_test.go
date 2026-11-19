package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// thursday is now, in these tests: the afternoon of Thursday 19 November 2026, on this machine.
var thursday = time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local)

// A WHEN is how long ago, a time of day, which is the last one, or a date and time. Anything else is
// a command typed wrong.
func TestWhenTakesADurationATimeOfDayOrADateAndTime(t *testing.T) {
	local := func(y int, m time.Month, d, h, mi, s int) time.Time {
		return time.Date(y, m, d, h, mi, s, 0, time.Local)
	}
	for s, want := range map[string]time.Time{
		"24h":                  thursday.Add(-24 * time.Hour),
		"90m":                  thursday.Add(-90 * time.Minute),
		"1h30m":                thursday.Add(-90 * time.Minute),
		"3d":                   local(2026, 11, 16, 14, 0, 0),
		"03:00":                local(2026, 11, 19, 3, 0, 0),
		"14:00":                thursday,
		"03:00:30":             local(2026, 11, 19, 3, 0, 30),
		"15:00":                local(2026, 11, 18, 15, 0, 0), // not yet today, so yesterday's
		"2026-11-18 03:00":     local(2026, 11, 18, 3, 0, 0),
		"2026-11-18 03:00:05":  local(2026, 11, 18, 3, 0, 5),
		"2026-11-18T03:00":     local(2026, 11, 18, 3, 0, 0),
		"2026-11-18":           local(2026, 11, 18, 0, 0, 0),
		"2026-11-18T03:00:00Z": time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC),
	} {
		got, ofDay, err := parseWhen("--since", s, thursday)
		if err != nil || !got.Equal(want) || ofDay != (len(s) > 2 && s[2] == ':') {
			t.Errorf("%q is %s, of a day %t, %v, want %s", s, got, ofDay, err, want)
		}
	}
	for _, s := range []string{
		"", "yesterday", "0h", "-2h", "0d", "25:00", "3 days", "2026-13-01", "18/11/2026", "3", "noon", "03:00 2026-11-18",
	} {
		_, _, err := parseWhen("--since", s, thursday)
		if e, ok := errors.AsType[*cli.Error](err); !ok || !e.Usage || !strings.HasPrefix(e.Message, "--since "+s) {
			t.Errorf("%q: %v, want it refused as --since typed wrong", s, err)
		}
	}
	for _, args := range [][]string{
		{"history", "--since", "tomorrow"}, {"history", "--until", "later"}, {"tail", "--since", "soon"},
		{"history", "--since", "2h", "--until", "3h"},
	} {
		if _, errs, code := runOikade(t, cli.Plain, args...); code != cli.ExitUsage || !strings.HasPrefix(errs, "Error: --") {
			t.Errorf("oikade %s exits %d: %s", strings.Join(args, " "), code, errs)
		}
	}
	// An --until before a --since nobody gave says what that --since is, and what to do.
	_, errs, code := runOikade(t, cli.Plain, "history", "--until", "3d")
	if code != cli.ExitUsage || !strings.Contains(errs, "--until 3d is before --since, which is 24h ago unless you say") ||
		!strings.Contains(errs, "Hint: give --since WHEN too") {
		t.Errorf("oikade history --until 3d exits %d: %s", code, errs)
	}
}

// An --until time of day is the last one, as --since's is, unless that's before --since: then it's
// the next one, so that a morning and an afternoon are one day's window.
func TestUntilATimeOfDayComesAfterSince(t *testing.T) {
	for _, c := range []struct{ since, until string }{{"10:00", "16:00"}, {"22:00", "23:30"}, {"23:00", "01:00"}} {
		dir, queries := aHost(t, api.EventLog{}, nil, api.Status{})
		_, errs, code := runOikade(t, cli.Plain, "history", "--json", "--data-dir", dir,
			"--since", c.since, "--until", c.until)
		if code != 0 {
			t.Fatalf("from %s to %s exits %d: %s", c.since, c.until, code, errs)
		}
		q, err := url.ParseQuery(queries()[0])
		if err != nil {
			t.Fatal(err)
		}
		f, err := api.ParseEventFilter(q)
		if err != nil {
			t.Fatal(err)
		}
		// Whatever the time is now, it's those times of day, the second less than a day after the first,
		// and the first already been.
		if f.Since.Local().Format("15:04") != c.since || f.Until.Local().Format("15:04") != c.until ||
			!f.Until.After(f.Since) || f.Until.Sub(f.Since) > 25*time.Hour || f.Since.After(time.Now()) {
			t.Errorf("from %s to %s asked for %s to %s", c.since, c.until, f.Since.Local(), f.Until.Local())
		}
	}
}

// history, tail and status take what they're about by its ref, a device by its name alone, and the
// Matter bridge as matter. What isn't one, or has no events, is a command typed wrong.
func TestHistoryTakesRefsAndBareDeviceNames(t *testing.T) {
	got, err := eventRefs("history", []string{
		"porch-light", "device/porch-light/on", "matter", "device/matter", "room/hall", "room/hall/on",
		"case/porch-light/on/dusk", "flow/morning", "plugin/overkiz", "command",
	})
	// A bare word is a device's name, whatever else it could have meant.
	want := []string{
		"device/porch-light", "device/porch-light/on", "matter", "device/matter", "room/hall", "room/hall/on",
		"case/porch-light/on/dusk", "flow/morning", "plugin/overkiz", "device/command",
	}
	if err != nil || !slices.Equal(refStrings(got), want) {
		t.Errorf("refs %q, %v, want %q", refStrings(got), err, want)
	}
	for arg, want := range map[string]string{
		"secret/token":      "secret/token has no events",
		"client/laptop":     "client/laptop has no events",
		"group/lamps":       "group/lamps has no events",
		"hardware/x/y":      "hardware/x/y has no events",
		"Porch Light":       "isn't a device's name",
		"lamp/porch":        "isn't a ref",
		"flow/morning/late": "isn't a ref",
	} {
		_, err := eventRefs("history", []string{arg})
		if e, ok := errors.AsType[*cli.Error](err); !ok || !e.Usage || !strings.Contains(e.Message, want) {
			t.Errorf("%s: %v, want %q, as a command typed wrong", arg, err, want)
		}
	}
	for _, command := range []string{"history", "tail"} {
		if _, errs, code := runOikade(t, cli.Plain, command, "secret/token"); code != cli.ExitUsage ||
			!strings.Contains(errs, "Hint: oikade "+command+" takes a device") {
			t.Errorf("oikade %s secret/token exits %d: %s", command, code, errs)
		}
	}
}

// aHost answers oikade history as a host would: this log, narrowed as the host narrows it for these
// refs, and a status with these devices and rules.
func aHost(t *testing.T, log api.EventLog, events []api.Event, st api.Status) (dir string, asked func() []string) {
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
	var mu sync.Mutex
	var queries []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.Prefix+"/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", api.StreamContentType)
		out := json.NewEncoder(w)
		_ = out.Encode(log)
		for _, e := range events {
			_ = out.Encode(e)
		}
		_ = out.Encode(api.LogEnd(""))
	})
	mux.HandleFunc("GET "+api.Prefix+"/tail", func(w http.ResponseWriter, _ *http.Request) {
		// Listening, and then gone, as a host that stops is.
		w.Header().Set("Content-Type", api.StreamContentType)
		_ = json.NewEncoder(w).Encode(api.Event{})
	})
	mux.HandleFunc("GET "+api.Prefix+"/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.ContentType)
		_ = json.NewEncoder(w).Encode(api.Response{Status: &st})
	})
	host := &http.Server{Handler: mux}
	go func() { _ = host.Serve(l) }()
	t.Cleanup(func() { _ = host.Close() })
	return dir, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(queries)
	}
}

// aNight is what the porch light did overnight, as the host sends a history of it: a deploy the
// evening before, the dusk case turning it on, a controller turning it off, the light dropping off
// and coming back, and the morning flow.
func aNight() []api.Event {
	at := func(day, h, m, s int) time.Time { return time.Date(2026, 11, day, h, m, s, 0, time.Local) }
	const light, on, brightness = "device/porch-light", "on", "brightness"
	wrote := func(t time.Time, name string, value any, by string) api.Event {
		return api.Event{Time: t, Type: api.EventWrite, Ref: light + "/" + name, Capability: name, Value: value, By: by}
	}
	changed := func(t time.Time, name string, value, previous any) api.Event {
		return api.Event{
			Time: t, Type: api.EventChange, Ref: light + "/" + name, Capability: name, Value: value, Previous: previous,
		}
	}
	went := func(t time.Time, message, why string) api.Event {
		return api.Event{Time: t, Type: api.EventAvailability, Ref: light, Message: message, Result: why}
	}
	return []api.Event{
		{Time: at(18, 16, 20, 3), Type: api.EventDeploy, Message: "f113018fb81c06e4d5fa3500102bfbc4"},
		{Time: at(18, 16, 20, 4), Type: api.EventProcess, Message: "connected"},
		wrote(at(18, 16, 41, 0), on, true, "case/porch-light/on/dusk"),
		changed(at(18, 16, 41, 1), on, true, false),
		wrote(at(18, 23, 5, 12), on, false, "matter"),
		changed(at(18, 23, 5, 12), on, false, true),
		went(at(19, 3, 0, 40), api.AvailabilityOffline, "no answer on the LAN"),
		went(at(19, 3, 2, 9), api.AvailabilityOnline, ""),
		wrote(at(19, 7, 0, 0), brightness, 40.0, "flow/morning"),
		changed(at(19, 7, 0, 1), brightness, 40.0, 100.0),
		{Time: at(19, 9, 30, 0), Type: api.EventProcess, Message: "disconnected"},
	}
}

// oikade history prints a line an event under a heading for each day, with the home's own dimmed
// when it's of some refs, says where the log starts if that's after --since, and, with nothing to
// show, says so and what would fill it.
func TestHistoryLooksTheSame(t *testing.T) {
	porch := []named{{ref: "device/porch-light", kind: "device", parts: []string{"porch-light"}}}
	typo := []named{{ref: "device/porch-lihgt", kind: "device", parts: []string{"porch-lihgt"}}}
	flow := []named{{ref: "flow/evening", kind: "flow", parts: []string{"evening"}}}
	since, longAgo, anHourAgo := thursday.Add(-24*time.Hour), thursday.Add(-264*time.Hour), thursday.Add(-time.Hour)
	st := api.Status{
		Devices: []api.Device{{Name: "porch-light", Room: "porch", Capabilities: []api.Capability{{Name: "on"}}}},
		Rules:   []api.Rule{{Name: "morning", Ref: "flow/morning"}},
	}
	started := api.EventLog{Start: since.Add(-72 * time.Hour)}
	for _, c := range []struct {
		name   string
		h      history
		log    api.EventLog
		events []api.Event
	}{
		{"device", history{refs: porch, filter: api.EventFilter{Since: since}}, started, aNight()},
		{"everything", history{filter: api.EventFilter{Since: since}}, started, aNight()},
		{"log-starts-later", history{refs: porch, filter: api.EventFilter{Since: longAgo}}, started, aNight()},
		// Only the home's own events came, so there's nothing of the device to show.
		{"empty-typo", history{refs: typo, filter: api.EventFilter{Since: since}}, started, aNight()[:2]},
		{"empty-rule", history{refs: flow, filter: api.EventFilter{Since: since, Until: anHourAgo}}, started, nil},
		{"empty-on-record", history{refs: porch, filter: api.EventFilter{Since: longAgo}}, started, nil},
		{"empty-quiet", history{refs: porch, filter: api.EventFilter{Since: since}}, started, nil},
		{"empty-log", history{filter: api.EventFilter{Since: since}}, api.EventLog{}, nil},
	} {
		dir, _ := aHost(t, c.log, c.events, st)
		for name, printer := range printers {
			for _, width := range []int{80, 120} {
				var out, errs bytes.Buffer
				h := c.h
				h.t, h.host, h.now = api.Local{DataDir: dir}, thisMachine, thursday
				if err := h.print(t.Context(), printer(&out, width), printer(&errs, width)); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				out.WriteString("--- stderr\n")
				out.Write(errs.Bytes())
				testkit.Golden(t, fmt.Sprintf("history-%s-%s-%d", c.name, name, width), out.Bytes())
			}
		}
	}
}

// history --json is a line of JSON an event, as the host sent it, and no lines for none. It asks the
// host for what it was asked for.
func TestHistoryAsJSONIsALineAnEvent(t *testing.T) {
	dir, asked := aHost(t, api.EventLog{}, aNight(), api.Status{})
	out, errs, code := runOikade(t, cli.Plain, "history", "porch-light", "flow/morning",
		"--since", "2026-11-18 16:00", "--until", "2026-11-19 12:00", "--json", "--data-dir", dir)
	if code != 0 || errs != "" {
		t.Fatalf("exit %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(aNight()) {
		t.Fatalf("%d lines for %d events:\n%s", len(lines), len(aNight()), out)
	}
	for i, line := range lines {
		var e api.Event
		want := aNight()[i]
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.Type != want.Type || !e.Time.Equal(want.Time) {
			t.Errorf("line %d is %s, %v, want event %+v", i, line, err, want)
		}
	}
	from := time.Date(2026, 11, 18, 16, 0, 0, 0, time.Local)
	want := api.EventFilter{
		Since: from, Until: from.Add(20 * time.Hour), Refs: []string{"device/porch-light", "flow/morning"},
	}.Query().Encode()
	if got := asked(); len(got) != 1 || got[0] != want {
		t.Errorf("it asked the host for %q, want %q", got, want)
	}

	dir, _ = aHost(t, api.EventLog{}, nil, api.Status{})
	out, errs, code = runOikade(t, cli.Plain, "history", "--json", "--data-dir", dir)
	if code != 0 || out != "" || errs != "" {
		t.Errorf("an empty history as JSON: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	// Only the home's own events came, so there's nothing of the device, and nothing to print.
	dir, _ = aHost(t, api.EventLog{}, aNight()[:2], api.Status{})
	out, errs, code = runOikade(t, cli.Plain, "history", "porch-lihgt", "--json", "--data-dir", dir)
	if code != 0 || out != "" || errs != "" {
		t.Errorf("a history of nothing as JSON: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// A tail of something the host has no record of says so once it's following, rather than follow in
// silence for good. One of something it has says nothing more.
func TestTailSaysARefNamesNothing(t *testing.T) {
	st := api.Status{Process: true, Devices: []api.Device{{Name: "porch-light", Room: "porch"}}}
	dir, _ := aHost(t, api.EventLog{}, nil, st)
	_, errs, _ := runOikade(t, cli.Plain, "tail", "porch-lihgt", "room/porch", "flow/morning", "--data-dir", dir)
	want := "Following device/porch-lihgt, room/porch, flow/morning on this machine's host\n" +
		"There's no device/porch-lihgt on record: oikade devices lists them.\n" +
		"There's no flow/morning in the running version: oikade status lists its rules.\n"
	if !strings.HasPrefix(errs, want) {
		t.Errorf("stderr:\n%s\nwant it to start:\n%s", errs, want)
	}
	_, errs, _ = runOikade(t, cli.Plain, "tail", "porch-light", "--data-dir", dir)
	if !strings.HasPrefix(errs, "Following device/porch-light on this machine's host\nError: ") {
		t.Errorf("stderr of a tail of a device on record:\n%s", errs)
	}
}

// oikade tail prints the past it asked for under its days' headings, and what happens now under none.
func TestTailPrintsItsPastUnderItsDays(t *testing.T) {
	night := aNight()
	now := night[len(night)-1].Time.Add(time.Hour)
	for name, c := range map[string]struct {
		since  time.Time
		events []api.Event
	}{"past": {now.Add(-48 * time.Hour), night}, "now": {time.Time{}, night[len(night)-3:]}} {
		for mode, printer := range printers {
			var out bytes.Buffer
			lines := &chronicle{p: printer(&out, 80), now: now, narrowed: true}
			if c.since.IsZero() {
				lines.day = cli.Day(now, now)
			}
			for _, e := range c.events {
				if err := lines.add(e); err != nil {
					t.Fatal(err)
				}
			}
			testkit.Golden(t, "tail-"+name+"-"+mode, out.Bytes())
		}
	}
}
