package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// target says which host a command is for.
type target struct {
	fs               *flag.FlagSet
	dataDir, context *string
}

func addTarget(fs *flag.FlagSet) target {
	return target{
		fs:      fs,
		dataDir: fs.String("data-dir", cli.DefaultDataDir, "The host's data directory, on this machine."),
		context: fs.String("context", "", "The host to reach over its network API, from oikade login."),
	}
}

// transport is --context, then the socket if --data-dir is given, then the current context, then the
// socket. A host it can't reach says so in the user's words.
func (t target) transport() api.Transport {
	transport, name := t.resolve()
	return api.Explained(transport, func(err error) error { return unreachable(transport, name, err) })
}

// isDNS is a name that didn't resolve.
func isDNS(err error) bool {
	_, ok := errors.AsType[*net.DNSError](err)
	return ok
}

// unreachable is err, from dialing t, in the user's words, if it's one that a host out of reach
// gives. Anything else, such as a cancel, is as it was.
func unreachable(t api.Transport, name string, err error) error {
	if _, ok := errors.AsType[*cli.Error](err); ok {
		return err
	}
	switch t := t.(type) {
	case api.Local:
		socket := filepath.Join(t.DataDir, api.SocketName)
		switch {
		case errors.Is(err, os.ErrPermission):
			return cli.Wrap(err, "you may not reach %s: its socket, %s, is the host's user's", thisMachine, socket).
				WithHint("run it with sudo, or pass --context NAME for a host you've logged in to")
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
			return cli.Wrap(err, "no host is running on this machine with its data in %s", t.DataDir).
				WithHint("start one with oikade-host, pass --data-dir DIR for another, " +
					"or --context NAME for a host you've logged in to")
		}
	case api.Remote:
		if errors.Is(err, api.ErrOtherHost) {
			return cli.Wrap(err, "%s, at %s, isn't the host you logged in to: its key has another pin", name, t.Address).
				WithHint("oikade login it again, with the pin oikade-host client add prints on it")
		}
		if _, isNet := errors.AsType[*net.OpError](err); isNet || isDNS(err) {
			return cli.Wrap(err, "couldn't reach %s, at %s", name, t.Address).
				WithHint("check the host is running there with --api-addr, and that this machine can reach it")
		}
	}
	return err
}

// thisMachine is what a command calls the host on this machine's socket.
const thisMachine = "this machine's host"

// resolve is transport, and what to call the host it reaches: its context's name, or thisMachine.
func (t target) resolve() (api.Transport, string) {
	given := false
	t.fs.Visit(func(f *flag.Flag) { given = given || f.Name == "data-dir" })
	if given && *t.context == "" {
		return api.Local{DataDir: *t.dataDir}, thisMachine
	}
	cfg, err := loadConfig()
	if err != nil {
		if *t.context != "" {
			return failing{err}, *t.context
		}
		// Asked for nothing, so this machine's host, as before there was a config. The problem is
		// still worth hearing about.
		_, _ = fmt.Fprintln(os.Stderr, "Warning: not using any context:", err)
		return api.Local{DataDir: *t.dataDir}, thisMachine
	}
	name := *t.context
	if name == "" {
		name = cfg.Current
	}
	if name == "" {
		return api.Local{DataDir: *t.dataDir}, thisMachine
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		return failing{fmt.Errorf("there's no context called %s: oikade login makes one", name)}, name
	}
	return remote(c), name
}

func runStatus(ctx context.Context, args []string) error {
	fs := newFlags("status")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	wide := fs.Bool("wide", false, "Show every column, however wide the terminal.")
	devices, err := namesOf(ident.Device, cli.Parse(fs, args))
	if err != nil {
		return err
	}
	transport := target.transport()
	resp, err := api.Call(ctx, transport, api.Request{Type: api.TypeStatus})
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
	if len(devices) > 0 {
		st.Devices = slices.DeleteFunc(st.Devices, func(d api.Device) bool { return !slices.Contains(devices, d.Name) })
		if len(st.Devices) == 0 {
			return cli.Errorf("there's no device %s", strings.Join(devices, ", ")).WithHint("oikade devices lists them")
		}
	}
	if *asJSON {
		return cli.PrintJSON(st)
	}
	summary := len(devices) == 0
	var bridge *api.Matter
	if summary {
		bridge = matterOf(ctx, transport)
	}
	printStatus(std.out, std.err, st, bridge, summary, *wide, time.Now())
	return nil
}

// matterOf is the host's Matter bridge, or nil if there's none to pair: Matter is turned off, or the
// host couldn't say.
func matterOf(ctx context.Context, t api.Transport) *api.Matter {
	resp, err := api.Call(ctx, t, api.Request{Type: api.TypeMatter})
	if err != nil || resp.Error != "" {
		return nil
	}
	return resp.Matter
}

// printStatus prints the home and host on a line, then a device a line with its values, fitted to
// out's width unless wide, and the rules. Asked about some devices (not summary), it shows each in
// full, a capability a line with why it has its value.
func printStatus(out, stderr *cli.Printer, st *api.Status, bridge *api.Matter, summary, wide bool, now time.Time) {
	var undeclared []string
	for _, d := range st.Devices {
		if d.Declare != "" {
			undeclared = append(undeclared, d.Name)
		}
	}
	if !summary {
		for i, d := range st.Devices {
			if i > 0 {
				out.Println()
			}
			printDevice(out, d)
		}
		countUndeclared(stderr, undeclared)
		return
	}
	for _, gap := range setupGaps(st, bridge) {
		stderr.Println(gap)
	}
	if line, down := homeLine(st, now); line != "" {
		out.Println(line)
		out.Println()
		if down && !st.Deploying { // A deploy restarts them.
			stderr.Println("oikade tail shows when they come back, and the host's log says why they went.")
		}
	}
	if len(st.Devices) > 0 {
		dropped, decided := printDeviceLines(out, st.Devices, wide)
		if len(dropped) > 0 {
			stderr.Hint("--wide shows %s", strings.Join(dropped, " and "))
		}
		countUndeclared(stderr, undeclared)
		if decided != "" {
			stderr.Hint("oikade status %s says why each of its values is what it is", decided)
		}
	}
	if len(st.Devices) == 0 && !undeployed(st) && !st.Deploying {
		noDevices(stderr, 0)
	}
	if len(st.Rules) > 0 {
		out.Println()
	}
	for _, r := range st.Rules {
		line := []string{or(r.Ref, r.Name), or(r.Kind, "gone")}
		if r.Safety {
			line = append(line, "Safety")
		}
		if r.Paused {
			line = append(line, "paused")
		}
		line = append(line, holdState(r, now)...)
		out.Println(strings.Join(line, "  "))
	}
}

// countUndeclared says, once, how many devices aren't in your code, naming one if it's alone, and
// where to read how to declare them.
func countUndeclared(stderr *cli.Printer, undeclared []string) {
	switch len(undeclared) {
	case 0:
	case 1:
		stderr.Println(stderr.Dim(undeclared[0] + " isn't in your code: oikade devices shows how to declare it"))
	default:
		stderr.Println(stderr.Dim(fmt.Sprintf("%d devices aren't in your code: oikade devices shows how to declare them",
			len(undeclared))))
	}
}

// homeLine is the home's version and its automations', such as "Home f113018 · automations
// connected 8 min ago", and whether they're down. A host with no home deployed has none.
func homeLine(st *api.Status, now time.Time) (line string, down bool) {
	home := "Home version unknown"
	if st.Version != "" {
		home = "Home " + cli.ShortVersion(st.Version)
	}
	when := func(prefix string) string {
		if st.Since == nil || st.Since.IsZero() {
			return ""
		}
		return prefix + cli.Ago(*st.Since, now)
	}
	switch {
	case st.Version == "" && st.Deploying:
		return home + " while a deploy is under way · automations " +
			map[bool]string{true: "connected", false: "down"}[st.Process], false
	case undeployed(st):
		return "", false
	case st.Process:
		line = home + " · automations connected" + when(" ")
	default:
		line, down = home+" · automations down", true
		if w := when(" "); w != "" {
			line = home + " · automations went down" + w
		}
	}
	if st.Deploying {
		line += " · a deploy is under way"
	}
	return line, down
}

// printDeviceLines prints a device a line: its name, room, values and what's wrong with it. It
// returns the columns that didn't fit, and a device with a decided value, for the hint that says
// where to read why.
func printDeviceLines(out *cli.Printer, devices []api.Device, wide bool) (dropped []string, decided string) {
	table := cli.Table{
		Columns: []cli.Column{
			{Header: nameHeader},
			{Header: "ROOM", Drop: 1},
			{Header: "VALUES", Shrink: true},
			// A problem names its fix, so it never shortens.
			{Header: "STATE", DimIf: func(cell string) bool { return cell == notInYourCode }},
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
		table.Rows = append(table.Rows, []string{d.Name, d.Room, strings.Join(values, ", "), deviceState(d)})
	}
	return out.Table(table), decided
}

// printDevice prints one device in full: its name, room and state, then a capability a line, with
// its value and why it has it.
func printDevice(out *cli.Printer, d api.Device) {
	head := out.Bold(d.Name)
	if d.Room != "" {
		head += " in " + d.Room
	}
	if state := deviceState(d); state == notInYourCode {
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
func deviceState(d api.Device) string {
	var state []string
	if d.PluginNotImported {
		state = append(state, notImported)
	}
	// Offline comes first, and the driver's problem may say why, as oikade devices has it (OIK-340).
	offline := d.Online != nil && !*d.Online && !d.PluginNotImported
	switch {
	case offline && d.Problem != "":
		state = append(state, "offline: "+d.Problem)
	case offline:
		state = append(state, "offline")
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

// undeployed is a host that has never had a home deployed.
func undeployed(st *api.Status) bool { return st.Version == "" && !st.Process && !st.Deploying }

// setupGaps are what's left to set up in a home, a line each, with the command that fixes it. A
// home that's fully set up has none.
func setupGaps(st *api.Status, bridge *api.Matter) []string {
	var gaps []string
	if undeployed(st) {
		gaps = append(gaps, "No home deployed. Run oikade deploy from your home's module.")
	}
	// A bridge that's down may not know its controllers, so it's the one to fix first.
	switch {
	case bridge == nil:
	case !bridge.Serving:
		gaps = append(gaps, fmt.Sprintf("The Matter bridge is down%s. oikade matter says more.",
			map[bool]string{true: "", false: ": " + bridge.Problem}[bridge.Problem == ""]))
	case len(bridge.Fabrics) == 0:
		gaps = append(gaps, "The Matter bridge has no controller. oikade matter says how to pair one.")
	}
	// A plugin that needs its setup can't serve its devices until it's run.
	for _, name := range st.NeedsSetup {
		gaps = append(gaps, fmt.Sprintf("Plugin %s needs setup: run oikade setup %s", name, name))
	}
	// A missing secret is often why a plugin can't log in, so it comes before the plugins' problems.
	for _, name := range st.UnsetSecrets {
		gaps = append(gaps, unsetSecret(name))
	}
	// A plugin's own problem, such as a refused login, often explains its devices' problems.
	for _, name := range slices.Sorted(maps.Keys(st.PluginProblems)) {
		gaps = append(gaps, fmt.Sprintf("Plugin %s: %s", name, st.PluginProblems[name]))
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
		gaps = append(gaps, fmt.Sprintf("Devices with a problem: %s. oikade devices says what's wrong.", names(problems)))
	}
	if len(unimported) > 0 {
		gaps = append(gaps, fmt.Sprintf("Devices whose plugin isn't imported: %s. "+
			"Import it in your home and run oikade deploy, or remove the device with oikade devices remove.", names(unimported)))
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
	next := func(t time.Time) string {
		if t.Before(now) {
			return clock(t, now) + " (passed)"
		}
		return clock(t, now)
	}
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
		state = append(state, fmt.Sprintf("%s: planned %d of %d min, run %d",
			b.Date, b.PlannedMinutes, b.TargetMinutes, b.RunMinutes))
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
	case "matter":
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

func runTail(ctx context.Context, args []string) error {
	fs := newFlags("tail")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print a line of JSON for each event, whose fields stay stable for scripts.")
	_ = fs.Parse(args)
	transport, host := target.resolve()
	return tail(ctx, transport, host, *asJSON, std.out, std.err)
}

// tail follows a host's events onto stdout, and says on stderr once it has started, so that a quiet
// house doesn't look hung.
func tail(ctx context.Context, t api.Transport, host string, asJSON bool, stdout, stderr io.Writer) error {
	out := json.NewEncoder(stdout)
	opened := func() {
		if host == thisMachine {
			_, _ = fmt.Fprintln(stderr, "Following the events of this machine's host")
		} else {
			_, _ = fmt.Fprintf(stderr, "Following %s's events\n", host)
		}
	}
	p := printerOf(stdout)
	return api.Tail(ctx, t, opened, func(e api.Event) error {
		if asJSON {
			return out.Encode(e)
		}
		p.Println(eventLine(p, e))
		return nil
	})
}

// eventLine is an event as tail prints it: its time, the ref of what it's about, what happened, and
// its cause, dimmed: "14:02:07  device/desk-plug/on  true  was false".
func eventLine(p *cli.Printer, e api.Event) string {
	what, cause := describe(e)
	line := []string{e.Time.Local().Format("15:04:05")}
	if e.Ref != "" {
		line = append(line, e.Ref)
	}
	line = append(line, what)
	if cause != "" {
		line = append(line, p.Dim(cause))
	}
	return strings.Join(line, "  ")
}

// describe is what an event says happened, and what caused it, if it says.
func describe(e api.Event) (what, cause string) {
	value := func(v any) string { return or(valueText(e.Capability, v), "nothing") }
	switch e.Type {
	case api.EventChange:
		if e.Previous != nil {
			cause = "was " + value(e.Previous)
		}
		return value(e.Value), cause
	case api.EventRule:
		return runResult(e.Result), ""
	case api.EventPause:
		return e.Message, ""
	case api.EventWrite:
		return "wrote " + value(e.Value), ""
	case api.EventRefused:
		return "refused a Matter write of " + value(e.Value), e.Message
	case api.EventDeploy:
		return "deployed version " + cli.ShortVersion(e.Message), ""
	}
	return "automations " + e.Message, ""
}

// runResult is how a rule's run ended, in words.
func runResult(result string) string {
	switch result {
	case "succeeded":
		return "ran"
	case "failed":
		return "ran and failed"
	case "cancelled":
		return "was cancelled"
	case "busy":
		return "didn't run: it was still running"
	case "condition_error":
		return "didn't run: its condition couldn't be read"
	case "missed":
		return "missed its time"
	}
	return strings.ReplaceAll(result, "_", " ")
}

func runSet(ctx context.Context, args []string) error {
	fs := newFlags("set")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := cli.Parse(fs, args)
	var device, name string
	var err error
	switch {
	case len(words) == 2 && strings.Contains(words[0], "/"):
		if device, name, err = capabilityOf(words[0]); err != nil {
			return err
		}
	case len(words) == 3:
		if device, err = nameOf(ident.Device, words[0]); err != nil {
			return err
		}
		name = words[1]
	default:
		return cli.UsageErrorf("oikade set takes a capability and its value")
	}
	value, err := valueOf(device, name, words[len(words)-1])
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "Setting %s to %v\n", ident.CapabilityRef(device, name), value)
	resp, err := api.Call(ctx, target.transport(), api.Request{
		Type: api.TypeSet, Device: device, Capability: name, Value: value,
	})
	if err != nil {
		return err
	}
	return done(resp, *asJSON, outcome(device, name, value, resp.Outcome))
}

// outcome says what a set left the capability at, and why, rather than what it asked for.
func outcome(device, name string, asked any, c *api.Capability) string {
	if c == nil {
		return fmt.Sprintf("%s hasn't answered yet: oikade status %s shows it when it does",
			ident.CapabilityRef(device, name), device)
	}
	is := fmt.Sprintf("%s is %v", ident.CapabilityRef(device, name), or(valueText(name, c.Value), "-"))
	if fmt.Sprint(c.Value) != fmt.Sprint(asked) && !rounded(name, c.Value, asked) {
		is += ", not " + valueText(name, asked)
	}
	if c.Level == "" {
		return is
	}
	for _, claim := range c.Claims {
		if claim.Winner {
			is += fmt.Sprintf(": %s holds it", lineName(claim))
			if claim.Expires != nil {
				is += " until " + claim.Expires.Local().Format("15:04")
			}
			if l := limited(*c); l != "" {
				is += ", " + l
			}
			return is
		}
	}
	return is + ": no line applies, so its Default wins"
}

// rounded reports whether a number the device reported is the one asked for, as a driver rounds it:
// brightness 33 comes back as 33.1.
func rounded(name string, reported, asked any) bool {
	r, ok := reported.(float64)
	a, ok2 := asked.(float64)
	return ok && ok2 && capability.Near(name, r, a)
}

// parseValue reads a value from the command line: a bool, a number or a string. valueOf checks it
// suits the capability, as the host does again.
func parseValue(s string) any {
	switch s {
	case "true", "on":
		return true
	case "false", "off":
		return false
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n
	}
	return s
}

func runPause(ctx context.Context, args []string, pause bool) error {
	name, kind, did := "resume", api.TypeResume, "Resumed"
	if pause {
		name, kind, did = "pause", api.TypePause, "Paused"
	}
	fs := newFlags(name)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	allow := fs.Bool("allow-safety", false,
		"Pause a safety line, a case above its decision's override line. Its claims stay.")
	words := cli.Parse(fs, args)
	if len(words) == 3 {
		ref := ident.CaseRef(words[0], words[1], words[2])
		if _, _, err := ident.ParseRef(ref.String()); err != nil {
			return err
		}
		return cli.UsageErrorf("a case is one ref: oikade %s %s", name, ref)
	}
	if len(words) != 1 {
		return cli.UsageErrorf("oikade %s takes one rule's ref, a case's, such as case/desk-plug/on/night, "+
			"or a flow's, such as flow/morning", name)
	}
	rule := words[0]
	if err := ruleOf(name, rule); err != nil {
		return err
	}
	resp, err := api.Call(ctx, target.transport(), api.Request{Type: kind, Rule: rule, AllowSafety: *allow})
	if err != nil {
		return err
	}
	if resp.NeedsAllowSafety {
		fmt.Fprintln(os.Stderr, "Pass --allow-safety to pause it anyway.")
	}
	return done(resp, *asJSON, paused(did, rule, resp))
}

// paused says what a pause or a resume did to the rule's claims.
func paused(did, rule string, resp api.Response) string {
	if did != "Paused" {
		return fmt.Sprintf("%s %s: it runs again, and claims as it does", did, rule)
	}
	message := fmt.Sprintf("%s %s. It had no claims to end", did, rule)
	if len(resp.Ended) > 0 {
		message = fmt.Sprintf("%s %s, and ended its claims on %s", did, rule, strings.Join(resp.Ended, ", "))
	}
	if len(resp.Kept) > 0 {
		message += fmt.Sprintf(". It keeps its Safety claims on %s, which only a release ends", strings.Join(resp.Kept, ", "))
	}
	return message
}

// done prints the outcome of a command that changes something.
// printerOf is w as a printer: w itself if it's one, or a plain one, as a test's buffer is.
func printerOf(w io.Writer) *cli.Printer {
	if p, ok := w.(*cli.Printer); ok {
		return p
	}
	return cli.Plain(w, 0)
}

func done(resp api.Response, asJSON bool, message string) error {
	if asJSON {
		out := struct {
			OK      bool            `json:"ok"`
			Error   string          `json:"error,omitempty"`
			Warning string          `json:"warning,omitempty"`
			Removed []string        `json:"removed,omitempty"`
			Outcome *api.Capability `json:"outcome,omitempty"`
			Ended   []string        `json:"ended,omitempty"`
			Kept    []string        `json:"kept,omitempty"`
		}{resp.Error == "", resp.Error, resp.Warning, resp.Removed, resp.Outcome, resp.Ended, resp.Kept}
		if err := cli.PrintJSON(out); err != nil {
			return err
		}
	} else {
		if resp.Error == "" {
			std.out.Done("%s", message)
		}
		// A remove that failed part way still warns of what it did.
		if resp.Warning != "" {
			std.err.Warn("%s", resp.Warning)
		}
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

// unsetSecret says a secret the home uses isn't set, and how to set it.
func unsetSecret(name string) string {
	return fmt.Sprintf("Secret %s isn't set: oikade secret set %s", name, name)
}

// printUnsetSecrets says which secrets the version now running uses that aren't set. It comes after
// the deploy's result, which it doesn't change: the plugin that needs one waits for it.
func printUnsetSecrets(out io.Writer, names []string) {
	for _, name := range names {
		_, _ = fmt.Fprintln(out, unsetSecret(name))
	}
}
