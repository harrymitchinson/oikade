package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
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
// socket.
func (t target) transport() api.Transport {
	transport, _ := t.resolve()
	return transport
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
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	devices := cli.Parse(fs, args)
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
			return fmt.Errorf("no device %s", strings.Join(devices, ", "))
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
	printStatus(os.Stdout, os.Stderr, st, bridge, summary, time.Now())
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

// printStatus prints a host's status. A summary, which is the whole home rather than the devices
// asked for, starts with what's left to set up, on stderr, and bridge is the host's Matter bridge, if
// it has one.
func printStatus(out, stderr io.Writer, st *api.Status, bridge *api.Matter, summary bool, now time.Time) {
	if summary {
		for _, gap := range setupGaps(st, bridge) {
			_, _ = fmt.Fprintln(stderr, gap)
		}
		switch {
		case st.Version == "" && st.Deploying:
			_, _ = fmt.Fprintf(out, "Version unknown while a deploy is under way, automations %s\n\n",
				map[bool]string{true: "connected", false: "down"}[st.Process])
		case undeployed(st):
		case st.Process:
			_, _ = fmt.Fprintf(out, "Version %s, automations connected\n\n", or(st.Version, "unknown"))
		default:
			_, _ = fmt.Fprintf(out, "Version %s, automations down%s\n\n", st.Version, since(st.Since, now))
			_, _ = fmt.Fprintln(stderr, "oikade tail shows when they come back, and the host's log says why they went.")
		}
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	var declare []string
	for _, d := range st.Devices {
		if d.Declare != "" {
			declare = append(declare, d.Declare)
		}
		for i, c := range d.Capabilities {
			state := why(c)
			if d.PluginNotImported {
				state = strings.TrimSpace(notImported + "  " + state)
			}
			if d.Declare != "" && i == 0 {
				state = strings.TrimSpace(notInYourCode + "  " + state)
			}
			if d.Problem != "" {
				state = strings.TrimSpace(d.Problem + "  " + state)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", d.Name, c.Name, display(c.Value), state)
		}
		if len(d.Capabilities) == 0 && (d.Problem != "" || d.Declare != "") {
			state := d.Problem
			if d.Declare != "" {
				state = strings.TrimSpace(notInYourCode + "  " + state)
			}
			_, _ = fmt.Fprintf(w, "%s\t-\t-\t%s\n", d.Name, state)
		}
	}
	_ = w.Flush()
	if !summary {
		return
	}
	printUndeclared(stderr, declare)
	_, _ = fmt.Fprintln(out)
	for _, r := range st.Rules {
		name := r.Name
		if r.Case != "" {
			name = fmt.Sprintf("%s %s: %s", r.Device, r.Capability, r.Case)
		}
		line := []string{name, or(r.Kind, "gone")}
		if r.Safety {
			line = append(line, "Safety")
		}
		if r.Paused {
			line = append(line, "paused")
		}
		line = append(line, holdState(r, now)...)
		_, _ = fmt.Fprintln(out, strings.Join(line, "  "))
	}
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

// since says when something began, as a time today or a date before.
func since(t *time.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return " since " + clock(*t, now)
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

func display(v any) any {
	if v == nil {
		return "-"
	}
	return v
}

// why says which line of a capability's decision wins, and what holds it back.
func why(c api.Capability) string {
	if c.Level == "" {
		return ""
	}
	wins := "Default"
	for _, claim := range c.Claims {
		if claim.Winner {
			wins = fmt.Sprintf("%s wants %v", lineName(claim), display(claim.Value))
		}
	}
	if l := limited(c); l != "" {
		wins += ", " + l
	}
	return wins
}

// lineName is how a claim's line reads: a case's name, a rule's, or where an outside change came
// from.
func lineName(c api.Claim) string {
	if c.Case != "" {
		return c.Case
	}
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
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print a line of JSON for each event, whose fields stay stable for scripts.")
	_ = fs.Parse(args)
	transport, host := target.resolve()
	return tail(ctx, transport, host, *asJSON, os.Stdout, os.Stderr)
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
	return api.Tail(ctx, t, opened, func(e api.Event) error {
		if asJSON {
			return out.Encode(e)
		}
		_, err := fmt.Fprintln(stdout, e.Time.Local().Format("15:04:05"), describe(e))
		return err
	})
}

func describe(e api.Event) string {
	switch e.Type {
	case api.EventChange:
		return fmt.Sprintf("%s %s = %v", e.Device, e.Capability, display(e.Value))
	case api.EventRule:
		return fmt.Sprintf("rule %s %s", e.Rule, e.Result)
	case api.EventPause:
		return fmt.Sprintf("rule %s %s", e.Rule, e.Message)
	case api.EventWrite:
		return fmt.Sprintf("wrote %s %s = %v", e.Device, e.Capability, display(e.Value))
	case api.EventRefused:
		return fmt.Sprintf("refused Matter write %s %s = %v: %s", e.Device, e.Capability, display(e.Value), e.Message)
	case api.EventDeploy:
		return "deployed " + e.Message
	}
	return "automations " + e.Message
}

func runSet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := cli.Parse(fs, args)
	if len(words) != 3 {
		return errors.New("usage: oikade set DEVICE CAPABILITY VALUE")
	}
	value := parseValue(words[2])
	_, _ = fmt.Fprintf(os.Stderr, "Setting %s %s to %v\n", words[0], words[1], value)
	resp, err := api.Call(ctx, target.transport(), api.Request{
		Type: api.TypeSet, Device: words[0], Capability: words[1], Value: value,
	})
	if err != nil {
		return err
	}
	return done(resp, *asJSON, outcome(words[0], words[1], value, resp.Outcome))
}

// outcome says what a set left the capability at, and why, rather than what it asked for.
func outcome(device, name string, asked any, c *api.Capability) string {
	if c == nil {
		return fmt.Sprintf("%s hasn't answered yet: oikade status %s shows its %s when it does", device, device, name)
	}
	is := fmt.Sprintf("%s %s is %v", device, name, display(c.Value))
	if fmt.Sprint(c.Value) != fmt.Sprint(asked) && !rounded(name, c.Value, asked) {
		is += fmt.Sprintf(", not %v", asked)
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

// parseValue reads a value from the command line. The host checks that it suits the capability.
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
	fs := flag.NewFlagSet(name, flag.ExitOnError)
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
		return fmt.Errorf("a case is one ref: oikade %s %s", name, ref)
	}
	if len(words) != 1 {
		return fmt.Errorf("usage: oikade %s REF, a case's, such as case/desk-plug/on/night, "+
			"or a flow's, such as flow/morning", name)
	}
	rule := words[0]
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
			fmt.Println(message)
		}
		// A remove that failed part way still warns of what it did.
		if resp.Warning != "" {
			fmt.Fprintln(os.Stderr, "Warning:", resp.Warning)
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
