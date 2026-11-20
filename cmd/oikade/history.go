package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// defaultHistory is how far back oikade history looks unless --since says.
const defaultHistory = "24h"

// readingAfter is how long oikade history waits for the log's first event before it says it's
// reading: the host reads its log through, which takes a moment for one device in a full log.
const readingAfter = time.Second

func runHistory(ctx context.Context, args []string) error {
	fs := newFlags("history")
	target := addTarget(fs)
	since := fs.String("since", defaultHistory, "How far back to start: a duration, a time of day or a date and time.")
	until := fs.String("until", "", "Where to stop, as --since takes it. Now, by default.")
	asJSON := fs.Bool("json", false, "Print a line of JSON for each event, whose fields stay stable for scripts.")
	refs, err := eventRefs("history", cli.Parse(fs, args))
	if err != nil {
		return err
	}
	now := time.Now()
	filter := api.EventFilter{Refs: refStrings(refs)}
	if filter.Since, _, err = parseWhen("--since", *since, now); err != nil {
		return err
	}
	if *until != "" {
		var ofDay bool
		if filter.Until, ofDay, err = parseWhen("--until", *until, now); err != nil {
			return err
		}
		if ofDay && filter.Until.Before(filter.Since) {
			// The last time it was that time is before --since, so it's the next one that's meant.
			filter.Until = filter.Until.AddDate(0, 0, 1)
		}
		sinceGiven := false
		fs.Visit(func(f *flag.Flag) { sinceGiven = sinceGiven || f.Name == "since" })
		switch {
		case !filter.Until.Before(filter.Since):
		case sinceGiven:
			return cli.UsageErrorf("--until %s is before --since %s, so nothing is between them", *until, *since)
		default:
			return cli.UsageErrorf("--until %s is before --since, which is %s ago unless you say", *until, defaultHistory).
				WithHint("give --since WHEN too, from before --until")
		}
	}
	transport, host := target.explained()
	h := history{t: transport, host: host, filter: filter, refs: refs, asJSON: *asJSON, now: now}
	return h.print(ctx, std.out, std.err)
}

func refStrings(refs []named) []string {
	out := make([]string, 0, len(refs))
	for _, n := range refs {
		out = append(out, n.ref)
	}
	return out
}

// days is a whole number of days, which Go's durations don't take.
var days = regexp.MustCompile(`^(\d+)d$`)

// whenLayouts are the dates and times a WHEN may be, in this machine's time zone unless it says.
var whenLayouts = []string{
	"2006-01-02 15:04", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02", time.RFC3339,
}

// parseWhen reads a flag's WHEN: how long ago, as a duration (24h, 90m, 3d), a time of day (03:00),
// which is the last one, as ofDay says, or a date and time (2026-11-18 03:00), in this machine's
// time zone. Anything else is a command typed wrong, and says what it takes.
func parseWhen(name, s string, now time.Time) (at time.Time, ofDay bool, err error) {
	if m := days.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return now.AddDate(0, 0, -n), false, nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, false, cli.UsageErrorf("%s %s is how long ago, so more than zero", name, s)
		}
		return now.Add(-d), false, nil
	}
	for _, layout := range []string{"15:04", "15:04:05"} {
		if at, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			y, m, d := now.Date()
			at = time.Date(y, m, d, at.Hour(), at.Minute(), at.Second(), 0, now.Location())
			if at.After(now) {
				// The last time it was that time.
				at = time.Date(y, m, d-1, at.Hour(), at.Minute(), at.Second(), 0, now.Location())
			}
			return at, true, nil
		}
	}
	for _, layout := range whenLayouts {
		if at, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return at, false, nil
		}
	}
	return time.Time{}, false, cli.UsageErrorf("%s %s isn't a time", name, s).
		WithHint("give how long ago (24h, 90m, 3d), a time of day (03:00) or a date and time (\"2026-11-18 03:00\")")
}

// history is one oikade history: what it asks the host for, and how it prints.
type history struct {
	t      api.Transport
	host   string
	filter api.EventFilter
	refs   []named
	asJSON bool
	now    time.Time
}

// print reads the host's log and prints what passes: a line of JSON an event, or a line each under a
// heading for its day. What isn't an event goes to stderr: that it's reading, when that takes a while,
// where the log starts if that's after --since, and what an empty answer means.
func (h history) print(ctx context.Context, stdout, stderr *cli.Printer) error {
	// reading says so once, if nothing has come by then.
	var said sync.Once
	reading := time.AfterFunc(readingAfter, func() {
		said.Do(func() { stderr.Println("Reading " + logOf(h.host)) })
	})
	defer reading.Stop()
	quiet := func() {
		reading.Stop()
		said.Do(func() {})
	}

	lines := &chronicle{p: stdout, now: h.now, narrowed: len(h.refs) > 0, hold: true}
	if h.asJSON {
		lines.json = json.NewEncoder(stdout)
	}
	var head api.EventLog
	err := api.Events(ctx, h.t, h.filter, func(log api.EventLog) error {
		head = log
		return nil
	}, func(e api.Event) error {
		quiet()
		return lines.add(e)
	})
	quiet()
	if err != nil {
		return err
	}
	if h.asJSON {
		// A script reads stdout, and no lines is its answer.
		return nil
	}
	if lines.about == 0 {
		return h.empty(ctx, stderr, head)
	}
	if !head.Start.IsZero() && head.Start.After(h.filter.Since) {
		stderr.Println(fmt.Sprintf("The event log starts %s: nothing from before then is kept.", cli.Ago(head.Start, h.now)))
	}
	return nil
}

// logOf is what oikade history calls a host's event log.
func logOf(host string) string {
	if host == thisMachine {
		return "the event log of " + thisMachine
	}
	return host + "'s event log"
}

// empty says a history is empty, and what would fill it: nothing is on record under a ref, the log
// doesn't go back as far as was asked, or nothing happened.
func (h history) empty(ctx context.Context, stderr *cli.Printer, head api.EventLog) error {
	between := "since " + cli.Ago(h.filter.Since, h.now)
	if !h.filter.Until.IsZero() {
		between = fmt.Sprintf("from %s to %s", cli.Ago(h.filter.Since, h.now), cli.Ago(h.filter.Until, h.now))
	}
	if len(h.refs) == 0 {
		stderr.Println("No events " + between + ".")
	} else {
		stderr.Println("No events of " + strings.Join(refStrings(h.refs), ", ") + " " + between + ".")
	}
	missing := unrecorded(ctx, h.t, h.refs)
	for _, line := range missing {
		stderr.Println(line)
	}
	switch {
	case head.Start.IsZero():
		stderr.Println("The host's event log is empty: it fills as devices report and rules run.")
	case head.Start.After(h.filter.Since):
		stderr.Println(fmt.Sprintf("The event log starts %s: nothing from before then is kept.", cli.Ago(head.Start, h.now)))
	case len(missing) == 0:
		stderr.Hint("--since WHEN looks further back: the event log starts %s", cli.Ago(head.Start, h.now))
	}
	return nil
}

// unrecorded says which of the refs asked for name nothing the host has on record, a line each with
// the command that lists what it has. A host that can't say leaves them out.
func unrecorded(ctx context.Context, t api.Transport, refs []named) []string {
	if len(refs) == 0 {
		return nil
	}
	resp, err := api.Call(ctx, t, api.Request{Type: api.TypeStatus})
	if err != nil || resp.Error != "" || resp.Status == nil {
		return nil
	}
	st := resp.Status
	device := func(name string) (api.Device, bool) {
		i := slices.IndexFunc(st.Devices, func(d api.Device) bool { return d.Name == name })
		if i < 0 {
			return api.Device{}, false
		}
		return st.Devices[i], true
	}
	has := func(d api.Device, name string) bool {
		return slices.ContainsFunc(d.Capabilities, func(c api.Capability) bool { return c.Name == name })
	}
	// With no process and no rules, the host can't say what the rules are.
	rulesKnown := st.Home.Process || len(st.Rules) > 0
	var out []string
	for _, n := range refs {
		switch n.kind {
		case ident.Device:
			d, ok := device(n.parts[0])
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("There's no %s on record: oikade devices lists them.", ident.DeviceRef(n.parts[0])))
			case len(n.parts) > 1 && !has(d, n.parts[1]):
				out = append(out, fmt.Sprintf("%s has no %s: oikade status %s lists what it has.",
					ident.DeviceRef(n.parts[0]), n.parts[1], n.parts[0]))
			}
		case ident.Room:
			room := ident.NewRef(ident.Room, n.parts[0])
			in := func(d api.Device) bool { return d.Room == n.parts[0] }
			switch {
			case !slices.ContainsFunc(st.Devices, in):
				out = append(out, fmt.Sprintf("No device is in %s: oikade status shows each device's room.", room))
			case len(n.parts) > 1 && !slices.ContainsFunc(st.Devices, func(d api.Device) bool {
				return in(d) && has(d, n.parts[1])
			}):
				out = append(out, fmt.Sprintf("No device in %s has %s: oikade status shows what each has.", room, n.parts[1]))
			}
		case ident.Case, ident.Flow:
			if rulesKnown && !slices.ContainsFunc(st.Rules, func(r api.Rule) bool { return r.Ref == n.ref }) {
				out = append(out, fmt.Sprintf("There's no %s in the running version: oikade status lists its rules.", n.ref))
			}
		}
	}
	return out
}

// chronicle prints events a line each, as tail does, under a heading for each day they're from.
type chronicle struct {
	p   *cli.Printer
	now time.Time
	// narrowed is whether the events were asked for by ref. The home's own, a deploy or your
	// automations connecting, come anyway, as they're often the reason, and print dimmed.
	narrowed bool
	// hold keeps the home's own back until there's an event of what was asked for, so that an answer
	// with none is empty rather than a list of deploys.
	hold bool
	held []api.Event
	// json, if set, prints each event as a line of JSON instead, with no headings.
	json *json.Encoder
	// day is the heading the last line is under, and about how many events of what was asked for
	// have printed. A tail of what's happening now starts under today's, unsaid.
	day   string
	about int
}

// homes says an event is the home's own, which comes whatever was asked for.
func homes(e api.Event) bool { return e.Type == api.EventProcess || e.Type == api.EventDeploy }

func (c *chronicle) add(e api.Event) error {
	own := c.narrowed && homes(e)
	if own && c.hold && c.about == 0 {
		c.held = append(c.held, e)
		return nil
	}
	if !own {
		c.about++
	}
	for _, earlier := range c.held {
		if err := c.line(earlier, true); err != nil {
			return err
		}
	}
	c.held = nil
	return c.line(e, own)
}

func (c *chronicle) line(e api.Event, own bool) error {
	if c.json != nil {
		return c.json.Encode(e)
	}
	if day := cli.Day(e.Time, c.now); day != c.day {
		if _, err := fmt.Fprintln(c.p, c.p.Bold(day)); err != nil {
			return err
		}
		c.day = day
	}
	line := eventLine(c.p, e)
	if own {
		line = c.p.Dim(eventLine(cli.Plain(io.Discard, 0), e))
	}
	_, err := fmt.Fprintln(c.p, line)
	return err
}

// following is what oikade tail says once the host is listening, so a quiet house doesn't look hung.
func following(host string, refs []named) string {
	switch {
	case len(refs) > 0:
		return "Following " + strings.Join(refStrings(refs), ", ") + " on " + host
	case host == thisMachine:
		return "Following the events of " + thisMachine
	}
	return "Following " + host + "'s events"
}
