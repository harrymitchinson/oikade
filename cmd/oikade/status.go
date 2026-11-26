package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/ident"
)

func runStatus(ctx context.Context, args []string) error {
	fs := newFlags("status")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	wide := fs.Bool("wide", false, "Show every column, however wide the terminal.")
	check := fs.Bool("check", false, "Exit 1 if something a command fixes is wrong, as well as printing it.")
	refs, err := statusRefs(cli.Parse(fs, args))
	if err != nil {
		return err
	}
	return status(ctx, target.transport(), refs, statusOptions{json: *asJSON, wide: *wide, check: *check})
}

// statusOptions are how oikade status prints, and whether it fails for what it finds wrong.
type statusOptions struct{ json, wide, check bool }

// status prints what the host knows now, of the whole home or of what refs name, and with check,
// fails, saying nothing more, if any of it needs fixing. A ref that names nothing fails it before
// anything prints.
func status(ctx context.Context, t api.Transport, refs []named, o statusOptions) error {
	resp, err := api.Call(ctx, t, api.Request{Type: api.TypeStatus})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	st := resp.Status
	if st == nil {
		return errors.New("the host sent no status")
	}
	if err := namesNothing(st, refs); err != nil {
		return err
	}
	now := time.Now()
	switch {
	case o.json:
		if err := json.NewEncoder(std.out).Encode(narrowed(st, refs)); err != nil {
			return err
		}
	case len(refs) == 0:
		printStatus(std.out, std.err, st, o.wide, now)
	default:
		events := func(ref string) ([]api.Event, error) { return lastEvents(ctx, t, ref, now) }
		printNamed(std.out, std.err, st, refs, events, o.wide, now)
	}
	if wrong := trouble(st, refs); o.check && len(wrong) > 0 {
		// The text says what's wrong, and JSON is for scripts, so stderr says it for a person.
		if o.json {
			for _, line := range wrong {
				std.err.Warn("%s", line)
			}
		}
		return cli.Silent()
	}
	return nil
}

// statusRefs is what oikade status is asked about, by ref: a device, a capability, a room, a case, a
// flow, a signal, a plugin or matter. Any other kind is a command typed wrong.
func statusRefs(args []string) ([]named, error) {
	refs, err := refsOf(args)
	if err != nil {
		return nil, err
	}
	for _, n := range refs {
		switch {
		case n.kind == ident.Device && len(n.parts) > 0, n.kind == ident.Room,
			n.kind == ident.Case, n.kind == ident.Flow, n.kind == ident.Signal, n.kind == ident.Plugin,
			n.kind == ident.Matter:
		default:
			return nil, cli.UsageErrorf("oikade status doesn't take %s", n.ref).
				WithHint("oikade status takes a device, a capability, a room, a case, a flow, a signal, a plugin or matter")
		}
	}
	return refs, nil
}

// namesNothing fails if a ref names nothing the host has, saying which.
func namesNothing(st *api.Status, refs []named) error {
	var missing []string
	var what, hint string
	for _, n := range refs {
		if w, h := missingFrom(st, n); w != "" {
			missing = append(missing, n.ref)
			what, hint = w, h
		}
	}
	switch len(missing) {
	case 0:
		return nil
	case 1:
		e := cli.Errorf("%s", lowerFirst(what))
		if hint != "" {
			e = e.WithHint("%s", hint)
		}
		return e
	}
	return cli.Errorf("%s name nothing the host has", andList(missing)).WithHint("oikade status shows what it has")
}

// missingFrom says what's missing if n names nothing st has, as a sentence, with the command that
// lists what there is, or nothing if n names something. A rule can't be missing from a host that
// can't say what its rules are.
func missingFrom(st *api.Status, n named) (what, hint string) {
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
	switch n.kind {
	case ident.Device:
		d, ok := device(n.parts[0])
		switch {
		case !ok:
			return fmt.Sprintf("There's no %s on record", ident.DeviceRef(n.parts[0])), "oikade list devices lists them"
		case len(n.parts) > 1 && !has(d, n.parts[1]):
			return fmt.Sprintf("%s has no %s", ident.DeviceRef(n.parts[0]), n.parts[1]),
				"oikade status " + n.parts[0] + " lists what it has"
		}
	case ident.Room:
		room := ident.NewRef(ident.Room, n.parts[0])
		switch {
		case len(inRoom(st, n.parts[:1])) == 0:
			return fmt.Sprintf("No device is in %s", room), "oikade status shows each device's room"
		case len(inRoom(st, n.parts)) == 0:
			return fmt.Sprintf("No device in %s has %s", room, n.parts[1]), "oikade status shows what each has"
		}
	case ident.Case, ident.Flow:
		// With no process and no rules, the host can't say what the rules are.
		known := st.Home.Process || len(st.Rules) > 0
		if known && !slices.ContainsFunc(st.Rules, func(r api.Rule) bool { return r.Ref == n.ref }) {
			return fmt.Sprintf("There's no %s in the running version", n.ref), "oikade status lists its rules"
		}
	case ident.Signal:
		known := st.Home.Process || len(st.Signals) > 0
		if known && !slices.ContainsFunc(st.Signals, func(s api.Signal) bool { return s.Ref == n.ref }) {
			return fmt.Sprintf("There's no %s in the running version", n.ref), "oikade list signals lists them"
		}
	case ident.Plugin:
		if !slices.ContainsFunc(st.Plugins, func(p api.Plugin) bool { return p.Name == n.parts[0] }) {
			return fmt.Sprintf("There's no %s: your home doesn't import it, and it reports no device on record", n.ref),
				"oikade status --json lists the plugins the host knows"
		}
	case ident.Matter:
		if st.Matter == nil {
			return "This host has Matter turned off", ""
		}
	}
	return "", ""
}

// inRoom are the devices in the room a room's ref names, in st's order, and given a capability, those
// that have it, with only it.
func inRoom(st *api.Status, parts []string) []api.Device {
	var out []api.Device
	for _, d := range st.Devices {
		if d.Room != parts[0] {
			continue
		}
		if len(parts) > 1 {
			d.Capabilities = slices.DeleteFunc(slices.Clone(d.Capabilities), func(c api.Capability) bool {
				return c.Name != parts[1]
			})
			if len(d.Capabilities) == 0 {
				continue
			}
		}
		out = append(out, d)
	}
	return out
}

// lowerFirst is s with its first letter lower case, as an error's message starts.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// narrowed is st as oikade status --json prints it for refs: all of it for none, and otherwise the
// home and what each ref names. A capability's device has only that capability, a room's devices
// and a plugin's are there in full, and the bridge only if matter is named. A kind no ref names is
// an empty array.
func narrowed(st *api.Status, refs []named) *api.Status {
	if len(refs) == 0 {
		return st
	}
	out := &api.Status{
		Home: st.Home, Devices: []api.Device{}, Rules: []api.Rule{}, Signals: []api.Signal{}, Plugins: []api.Plugin{},
	}
	whole, only := map[string]bool{}, map[string][]string{}
	rules, signals, plugins := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, n := range refs {
		switch n.kind {
		case ident.Device:
			if len(n.parts) == 1 {
				whole[n.parts[0]] = true
			} else {
				only[n.parts[0]] = append(only[n.parts[0]], n.parts[1])
			}
		case ident.Room:
			for _, d := range inRoom(st, n.parts) {
				if len(n.parts) == 1 {
					whole[d.Name] = true
				} else {
					only[d.Name] = append(only[d.Name], n.parts[1])
				}
			}
		case ident.Case, ident.Flow:
			rules[n.ref] = true
		case ident.Signal:
			signals[n.ref] = true
		case ident.Plugin:
			plugins[n.parts[0]] = true
		case ident.Matter:
			out.Matter = st.Matter
		}
	}
	for _, p := range st.Plugins {
		if plugins[p.Name] {
			out.Plugins = append(out.Plugins, p)
			for _, d := range p.Devices {
				whole[d] = true
			}
		}
	}
	for _, d := range st.Devices {
		switch {
		case whole[d.Name]:
			out.Devices = append(out.Devices, d)
		case only[d.Name] != nil:
			d.Capabilities = slices.DeleteFunc(slices.Clone(d.Capabilities), func(c api.Capability) bool {
				return !slices.Contains(only[d.Name], c.Name)
			})
			out.Devices = append(out.Devices, d)
		}
	}
	for _, r := range st.Rules {
		if rules[r.Ref] {
			out.Rules = append(out.Rules, r)
		}
	}
	for _, s := range st.Signals {
		if signals[s.Ref] {
			out.Signals = append(out.Signals, s)
		}
	}
	return out
}

// trouble is what oikade status --check fails for, a line each. With no refs, it's what a command
// fixes in the home: what status lists above its table as left to set up, and automations that are
// down. A device that's only offline isn't, as that's its hardware's, and a deploy neither causes nor
// fixes it. With refs, it's the same of what they name, where a device that's offline counts if it,
// a capability of it or its room is named.
func trouble(st *api.Status, refs []named) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	down := automationsDown(st.Home)
	devices := func(d api.Device, offline bool) {
		switch {
		case d.PluginNotImported:
			add("%s: its plugin isn't imported", d.Ref)
		case d.Problem != "":
			add("%s: %s", d.Ref, d.Problem)
		case offline && d.Online != nil && !*d.Online:
			add("%s: offline", d.Ref)
		}
	}
	plugin := func(p api.Plugin) {
		if p.NeedsSetup {
			add("%s: needs setup", p.Ref)
		}
		if p.Problem != "" {
			add("%s: %s", p.Ref, p.Problem)
		}
	}
	if len(refs) == 0 {
		switch {
		case st.Home.Undeployed():
			add("no home deployed")
		case down:
			add("automations down")
		}
		if st.Matter != nil && !st.Matter.Serving {
			add("the Matter bridge is down")
		}
		for _, p := range st.Plugins {
			plugin(p)
		}
		for _, d := range st.Devices {
			devices(d, false)
		}
		return out
	}
	shown := narrowed(st, refs)
	named := map[string]bool{}
	for _, n := range refs {
		switch n.kind {
		case ident.Device:
			named[n.parts[0]] = true
		case ident.Room:
			for _, d := range inRoom(st, n.parts) {
				named[d.Name] = true
			}
		case ident.Case, ident.Flow, ident.Signal:
			if down {
				add("%s: automations down", n.ref)
			}
		case ident.Matter:
			if st.Matter != nil && !st.Matter.Serving {
				add("the Matter bridge is down")
			}
		}
	}
	for _, p := range shown.Plugins {
		plugin(p)
	}
	for _, d := range shown.Devices {
		devices(d, named[d.Name])
	}
	return out
}

// eventsShown is how many of its last events a device's view ends with.
const eventsShown = 10

// lastEvents are ref's last eventsShown events in the host's log, oldest first, without the home's
// own. It reads the last day first, and the whole log only if that has too few, as a sensor's 14
// days can be thousands of events.
func lastEvents(ctx context.Context, t api.Transport, ref string, now time.Time) ([]api.Event, error) {
	var last []api.Event
	for _, since := range []time.Time{now.Add(-24 * time.Hour), {}} {
		last = nil
		err := api.Events(ctx, t, api.EventFilter{Refs: []string{ref}, Since: since}, func(api.EventLog) error { return nil },
			func(e api.Event) error {
				if !homes(e) {
					last = append(last, e)
					if len(last) > eventsShown {
						last = last[1:]
					}
				}
				return nil
			})
		if err != nil || len(last) == eventsShown {
			return last, err
		}
	}
	return last, nil
}

// printStatus prints the home and host on a line, what's left to set up, then a device a line with
// its values and what's wrong with it, fitted to out's width unless wide, and the rules.
func printStatus(out, stderr *cli.Printer, st *api.Status, wide bool, now time.Time) {
	for _, gap := range setupGaps(st) {
		stderr.Println(gap)
	}
	if line, down := homeLine(st, now); line != "" {
		out.Println(line)
		out.Println()
		if down && !st.Home.Deploying { // A deploy restarts them.
			stderr.Println("oikade tail shows when they come back, and the host's log says why they went.")
		}
	}
	if len(st.Devices) > 0 {
		dropped, decided := printDeviceLines(out, st.Devices, wide, now)
		if len(dropped) > 0 {
			stderr.Hint("--wide shows %s", andList(dropped))
		}
		countUndeclared(stderr, undeclaredOf(st.Devices))
		if decided != "" {
			stderr.Hint("oikade status %s says why each of its values is what it is", decided)
		}
	}
	if len(st.Devices) == 0 && !st.Home.Undeployed() && !st.Home.Deploying {
		noDevices(stderr, 0)
	}
	if len(st.Rules) > 0 {
		out.Println()
	}
	for _, r := range st.Rules {
		out.Println(ruleLine(r, now))
	}
	if len(st.Signals) > 0 {
		out.Println()
	}
	for _, s := range st.Signals {
		out.Println(signalLine(s, now))
	}
}

// printNamed prints what each ref names, in the order given, with a blank line between: a device in
// full, ending with its last events, a capability the same but only it, a room's devices a line each,
// a rule's line and what it holds, a plugin's state and its devices, and the Matter bridge. events
// reads a ref's last events.
func printNamed(out, stderr *cli.Printer, st *api.Status, refs []named, events func(string) ([]api.Event, error),
	wide bool, now time.Time,
) {
	shown := narrowed(st, refs)
	device := func(name string) api.Device {
		return st.Devices[slices.IndexFunc(st.Devices, func(d api.Device) bool { return d.Name == name })]
	}
	var dropped []string
	for i, n := range refs {
		if i > 0 {
			out.Println()
		}
		switch n.kind {
		case ident.Device:
			d := device(n.parts[0])
			if len(n.parts) > 1 {
				d.Capabilities = slices.DeleteFunc(slices.Clone(d.Capabilities), func(c api.Capability) bool {
					return c.Name != n.parts[1]
				})
			}
			printDevice(out, d, now)
			printLastEvents(out, stderr, n.ref, events, now)
		case ident.Room:
			out.Println(out.Bold(n.ref))
			more, _ := printDeviceLines(out, inRoom(st, n.parts), wide, now)
			dropped = append(dropped, more...)
		case ident.Case, ident.Flow:
			printRule(out, st, n.ref, now)
		case ident.Signal:
			if printSignal(out, st, n.ref, now) {
				printLastEvents(out, stderr, n.ref, events, now)
			}
		case ident.Plugin:
			more := printPlugin(out, st, n.parts[0], wide, now)
			dropped = append(dropped, more...)
		case ident.Matter:
			printMatter(out, st.Matter, out.Terminal)
		}
	}
	if dropped = slices.Compact(dropped); len(dropped) > 0 {
		stderr.Hint("--wide shows %s", andList(dropped))
	}
	countUndeclared(stderr, undeclaredOf(shown.Devices))
}

// printLastEvents ends a device's view with its last events, oldest first, under a heading for each
// day, or says it has none in the log.
func printLastEvents(out, stderr *cli.Printer, ref string, events func(string) ([]api.Event, error), now time.Time) {
	got, err := events(ref)
	if err != nil {
		stderr.Warn("Could not read the events of %s: %v", ref, err)
		return
	}
	out.Println()
	if len(got) == 0 {
		out.Println(out.Dim("No events of " + ref + " in the host's event log."))
		return
	}
	lines := &chronicle{p: out, now: now}
	for _, e := range got {
		_ = lines.add(e)
	}
}

// printRule prints a rule's line, as status does, and for a case, the capability it decides, with
// its value and why. A rule the host can't say it has, as its automations aren't connected, says so.
func printRule(out *cli.Printer, st *api.Status, ref string, now time.Time) {
	i := slices.IndexFunc(st.Rules, func(r api.Rule) bool { return r.Ref == ref })
	if i < 0 {
		out.Println(ref + "  " +
			out.Dim("unknown: the host can't say what the rules are while your automations aren't connected"))
		return
	}
	r := st.Rules[i]
	out.Println(ruleLine(r, now))
	if automationsDown(st.Home) {
		out.Println("Your automations are down: the host's log says why, and oikade tail shows when they come back.")
	}
	if r.Device == "" || r.Capability == "" {
		return
	}
	for _, d := range st.Devices {
		for _, c := range d.Capabilities {
			if d.Name == r.Device && c.Name == r.Capability {
				line := fmt.Sprintf("%s is %s", ident.CapabilityRef(d.Name, c.Name), or(valueText(c.Name, c.Value), "unknown"))
				if w := why(c); w != "" {
					line += ": " + w
				}
				out.Println(line)
			}
		}
	}
}

// printSignal prints a signal's line, as status does, what its conditions say and the devices it
// leaves out as dead, and says whether the host has it. One the host can't say it has, as your
// automations aren't connected, says so.
func printSignal(out *cli.Printer, st *api.Status, ref string, now time.Time) bool {
	i := slices.IndexFunc(st.Signals, func(s api.Signal) bool { return s.Ref == ref })
	if i < 0 {
		out.Println(ref + "  " +
			out.Dim("unknown: the host can't say what the signals are while your automations aren't connected"))
		return false
	}
	s := st.Signals[i]
	out.Println(signalLine(s, now))
	switch {
	case automationsDown(st.Home):
		out.Println("Your automations are down: the host's log says why, and oikade tail shows when they come back.")
	case s.Why != "":
		out.Println(upperFirst(s.Why))
	}
	if len(s.Dead) > 0 {
		out.Println("It leaves out " + andList(s.Dead) + ": offline, or quiet past its MaxAge.")
	}
	return true
}

// signalLine is a signal as status lists it: its ref, its value and since when, and when a Delay or
// Linger running ends.
func signalLine(s api.Signal, now time.Time) string {
	if s.Value == nil {
		return s.Ref + "  not worked out yet"
	}
	state := []string{fmt.Sprint(*s.Value)}
	if s.Since != nil {
		state[0] += " since " + clock(*s.Since, now)
	}
	if s.OnAt != nil {
		state = append(state, "true at "+upcoming(*s.OnAt, now))
	}
	if s.OffAt != nil {
		state = append(state, "false at "+upcoming(*s.OffAt, now))
	}
	return s.Ref + "  " + strings.Join(state, ", ")
}

// upcoming says when something to come is, and that it has gone by, as when the host is behind.
func upcoming(t, now time.Time) string {
	if t.Before(now) {
		return clock(t, now) + " (passed)"
	}
	return clock(t, now)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// printPlugin prints a plugin's ref and what's wrong with it, then its devices a line each, and
// returns the columns that didn't fit.
func printPlugin(out *cli.Printer, st *api.Status, name string, wide bool, now time.Time) []string {
	p := st.Plugins[slices.IndexFunc(st.Plugins, func(p api.Plugin) bool { return p.Name == name })]
	var state []string
	if p.NeedsSetup {
		state = append(state, "needs setup: run oikade setup "+p.Name)
	}
	if p.Problem != "" {
		state = append(state, p.Problem)
	}
	head := out.Bold(p.Ref)
	if len(state) > 0 {
		head += ": " + strings.Join(state, "; ")
	}
	out.Println(head)
	var devices []api.Device
	for _, d := range st.Devices {
		if slices.Contains(p.Devices, d.Name) {
			devices = append(devices, d)
		}
	}
	if len(devices) == 0 {
		out.Println(out.Dim("It reports no device on record."))
		return nil
	}
	dropped, _ := printDeviceLines(out, devices, wide, now)
	return dropped
}

// ruleLine is a rule as status lists it: its ref, its kind, Safety and paused if it is, and what a
// hold is doing.
func ruleLine(r api.Rule, now time.Time) string {
	line := []string{or(r.Ref, r.Name), or(r.Kind, "gone")}
	if r.Safety {
		line = append(line, "Safety")
	}
	if r.Paused {
		line = append(line, "paused")
	}
	line = append(line, holdState(r, now)...)
	return strings.Join(line, "  ")
}

// automationsDown is a home deployed whose automations aren't connected, and not for a deploy, which
// restarts them.
func automationsDown(h api.Home) bool { return h.Version != "" && !h.Process && !h.Deploying }

// undeclaredOf names the devices your home's code doesn't declare.
func undeclaredOf(devices []api.Device) []string {
	var out []string
	for _, d := range devices {
		if d.Declare != "" {
			out = append(out, d.Name)
		}
	}
	return out
}

// countUndeclared says, once, how many devices aren't in your code, naming one if it's alone, and
// where to read how to declare them.
func countUndeclared(stderr *cli.Printer, undeclared []string) {
	switch len(undeclared) {
	case 0:
	case 1:
		stderr.Println(stderr.Dim(undeclared[0] + " isn't in your code: oikade list devices shows how to declare it"))
	default:
		stderr.Println(stderr.Dim(fmt.Sprintf("%d devices aren't in your code: oikade list devices shows how to declare them",
			len(undeclared))))
	}
}

// homeLine is the home's version and its automations', such as "Home f113018 · automations
// connected 8 min ago", and whether they're down. A host with no home deployed has none.
func homeLine(st *api.Status, now time.Time) (line string, down bool) {
	home := "Home version unknown"
	if st.Home.Version != "" {
		home = "Home " + cli.ShortVersion(st.Home.Version)
	}
	when := func(prefix string) string {
		if st.Home.Since == nil || st.Home.Since.IsZero() {
			return ""
		}
		return prefix + cli.Ago(*st.Home.Since, now)
	}
	switch {
	case st.Home.Version == "" && st.Home.Deploying:
		return home + " while a deploy is under way · automations " +
			map[bool]string{true: "connected", false: "down"}[st.Home.Process], false
	case st.Home.Undeployed():
		return "", false
	case st.Home.Process:
		line = home + " · automations connected" + when(" ")
	default:
		line, down = home+" · automations down", true
		if w := when(" "); w != "" {
			line = home + " · automations went down" + w
		}
	}
	if st.Home.Deploying {
		line += " · a deploy is under way"
	}
	return line, down
}

// printDeviceLines prints a device a line: its name, room, values and what's wrong with it. It
// returns the columns that didn't fit, and a device with a decided value, for the hint that says
// where to read why.
func printDeviceLines(out *cli.Printer, devices []api.Device, wide bool, now time.Time) (
	dropped []string, decided string,
) {
	table := cli.Table{
		Columns: []cli.Column{
			{Header: nameHeader},
			{Header: "ROOM", Drop: 1},
			{Header: "VALUES", Shrink: true},
			// A problem names its fix, so it never shortens.
			{Header: stateHeader, DimIf: func(cell string) bool { return cell == notInYourCode }},
		},
		Wide: wide,
	}
	for _, d := range devices {
		var values []string
		for _, c := range d.Capabilities {
			if c.Value != nil {
				values = append(values, c.Name+" "+valueText(c.Name, c.Value))
			}
			if c.Level != "" && decided == "" {
				decided = d.Name
			}
		}
		table.Rows = append(table.Rows, []string{d.Name, d.Room, strings.Join(values, ", "), stateOf(d, offline(d, now))})
	}
	return out.Table(table), decided
}

// printDevice prints one device in full: its name, room and state, with when it went offline if it
// has, then a capability a line, with its value and why it has it.
func printDevice(out *cli.Printer, d api.Device, now time.Time) {
	head := out.Bold(d.Name)
	if d.Room != "" {
		head += " in " + d.Room
	}
	if state := stateOf(d, offline(d, now)); state == notInYourCode {
		head += out.Dim(": " + state)
	} else if state != "" {
		head += ": " + state
	}
	out.Println(head)
	if len(d.Capabilities) == 0 {
		return
	}
	out.Println()
	// In full, so nothing shortens.
	table := cli.Table{Columns: []cli.Column{{Header: "CAPABILITY"}, {Header: "VALUE"}, {Header: "WHY"}}, Wide: true}
	for _, c := range d.Capabilities {
		table.Rows = append(table.Rows, []string{c.Name, valueText(c.Name, c.Value), why(c)})
	}
	out.Table(table)
}

// deviceState is what's wrong with a device, if anything, and whether your home's code declares it.
func deviceState(d api.Device) string { return stateOf(d, "offline") }

// offline says a device is offline, and when it went if the host saw it go, such as "went offline 8
// min ago".
func offline(d api.Device, now time.Time) string {
	if d.OfflineSince == nil {
		return "offline"
	}
	return "went offline " + cli.Ago(*d.OfflineSince, now)
}

// stateOf is deviceState, saying a device is offline as down does.
func stateOf(d api.Device, down string) string {
	var state []string
	if d.PluginNotImported {
		state = append(state, notImported)
	}
	// Offline comes first, and the driver's problem may say why.
	off := d.Online != nil && !*d.Online && !d.PluginNotImported
	switch {
	case off && d.Problem != "":
		state = append(state, down+": "+d.Problem)
	case off:
		state = append(state, down)
	case d.Problem != "":
		state = append(state, d.Problem)
	}
	if d.Declare != "" {
		state = append(state, notInYourCode)
	}
	return strings.Join(state, ", ")
}

// valueText is a capability's value as a person reads it, with its unit if it has one, such as 21°C.
// One that hasn't been reported is blank.
func valueText(capabilityName string, v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case bool, string:
		return fmt.Sprint(v)
	case float64:
		s := strconv.FormatFloat(v, 'f', -1, 64)
		if spec, ok := capability.Lookup(capabilityName); ok {
			s += spec.Unit
		}
		return s
	}
	return fmt.Sprint(v)
}

// setupGaps are what's left to set up in a home, a line each, with the command that fixes it. A
// home that's fully set up has none.
func setupGaps(st *api.Status) []string {
	var gaps []string
	bridge := st.Matter
	if st.Home.Undeployed() {
		gaps = append(gaps, "No home deployed. Run oikade deploy from your home's module.")
	}
	// A bridge that's down may not know its controllers, so it's the one to fix first.
	switch {
	case bridge == nil:
	case !bridge.Serving:
		gaps = append(gaps, fmt.Sprintf("The Matter bridge is down%s. oikade status matter says more.",
			map[bool]string{true: "", false: ": " + bridge.Problem}[bridge.Problem == ""]))
	case len(bridge.Fabrics) == 0:
		gaps = append(gaps, "The Matter bridge has no controller. oikade status matter says how to pair one.")
	}
	// A plugin that needs its setup can't serve its devices until it's run.
	for _, p := range st.Plugins {
		if p.NeedsSetup {
			gaps = append(gaps, fmt.Sprintf("Plugin %s needs setup: run oikade setup %s", p.Name, p.Name))
		}
	}
	// A plugin's own problem, such as a refused login, often explains its devices' problems.
	for _, p := range st.Plugins {
		if p.Problem != "" {
			gaps = append(gaps, fmt.Sprintf("Plugin %s: %s", p.Name, p.Problem))
		}
	}
	var problems, unimported []string
	for _, d := range st.Devices {
		switch {
		case d.PluginNotImported:
			unimported = append(unimported, d.Name)
		case d.Problem != "":
			problems = append(problems, d.Name)
		}
	}
	if len(problems) > 0 {
		gaps = append(gaps, fmt.Sprintf("Devices with a problem: %s. Their STATE says what's wrong.", names(problems)))
	}
	if len(unimported) > 0 {
		gaps = append(gaps, fmt.Sprintf("Devices whose plugin isn't imported: %s. "+
			"Import it in your home and run oikade deploy, or forget its devices with oikade forget --plugin NAME.",
			names(unimported)))
	}
	return gaps
}

// names lists devices by name, the first few of them if there are many.
func names(devices []string) string {
	const most = 5
	if len(devices) <= most {
		return strings.Join(devices, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(devices[:most], ", "), len(devices)-most)
}

// clock says when something is, as a time if it's today or a date and time if it isn't.
func clock(t, now time.Time) string {
	at, now := t.Local(), now.Local()
	if y, m, d := at.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return at.Format("15:04")
	}
	return at.Format(time.DateTime)
}

// holdState says what a hold is doing, such as "active since 23:07, off at 23:10", and why it
// stopped, if it did. A rule that isn't a running hold has none. A time to come that has gone by,
// as when the host is behind, says so.
func holdState(r api.Rule, now time.Time) []string {
	if r.Active == nil {
		return nil
	}
	next := func(t time.Time) string { return upcoming(t, now) }
	var state []string
	switch {
	case *r.Active && !r.Since.IsZero():
		state = append(state, "active since "+clock(r.Since, now))
	case *r.Active:
		state = append(state, "active")
	default:
		state = append(state, "inactive")
	}
	if !r.OnAt.IsZero() {
		state = append(state, "on at "+next(r.OnAt))
	}
	if !r.OffAt.IsZero() {
		state = append(state, "off at "+next(r.OffAt))
	}
	if r.Latched {
		state = append(state, "latched")
	}
	if b := r.Budget; b != nil {
		day := fmt.Sprintf("%s: planned %d of %d min, run %d", b.Date, b.PlannedMinutes, b.TargetMinutes, b.RunMinutes)
		if b.MissedMinutes > 0 {
			day += fmt.Sprintf(", missed %d", b.MissedMinutes)
		}
		if !b.ReplannedAt.IsZero() {
			day += ", replanned at " + clock(b.ReplannedAt, now)
		}
		state = append(state, day)
	}
	line := []string{strings.Join(state, ", ")}
	if r.WhyNot != "" {
		line[0] += ": " + r.WhyNot
	}
	if r.Stopped != "" {
		line = append(line, "stopped: "+r.Stopped)
	}
	return line
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// why says which line of a capability's decision wins, and what holds it back.
func why(c api.Capability) string {
	if c.Level == "" {
		return ""
	}
	wins := "Default"
	for _, claim := range c.Claims {
		if claim.Winner {
			wins = fmt.Sprintf("%s wants %v", lineName(claim), or(valueText(c.Name, claim.Value), "nothing"))
		}
	}
	if l := limited(c); l != "" {
		wins += ", " + l
	}
	return wins
}

// lineName is how a claim's line reads: a case's or a flow's ref, or where an outside change came
// from.
func lineName(c api.Claim) string {
	switch c.Source {
	case "command":
		return "oikade set"
	case "device":
		return "a change on the device"
	case string(ident.Matter):
		return "a change from Matter"
	}
	return c.Source
}

// limited says how a capability's Limits hold its winner back, if they do.
func limited(c api.Capability) string {
	until := ""
	if c.LimitUntil != nil {
		until = " until " + c.LimitUntil.Local().Format("15:04")
	}
	switch c.Limit {
	case "":
		return ""
	case "MinimumOn":
		return "on for its MinimumOn" + until
	case "MinimumOff":
		return "resting" + until
	case "MaximumOn":
		return "resting after its MaximumOn" + until
	case "MaximumStartsPerDay":
		return "out of starts for the day" + until
	}
	return "held by " + c.Limit + until
}
