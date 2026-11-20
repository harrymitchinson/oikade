package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// target says which host a command is for.
type target struct {
	fs               *flag.FlagSet
	dataDir, context *string
}

// renamed keeps a flag that has a new name, so one typed from habit says it. A boolean takes no value.
func renamed(fs *flag.FlagSet, from, to string, boolean bool) {
	refuse := func(string) error { return fmt.Errorf("--%s is --%s now", from, to) }
	if boolean {
		fs.BoolFunc(from, "", refuse)
		return
	}
	fs.Func(from, "", refuse)
}

// The flags that say which host a command is for.
const (
	flagDataDir = "data-dir"
	flagContext = "context"
)

func addTarget(fs *flag.FlagSet) target {
	return target{
		fs:      fs,
		dataDir: fs.String(flagDataDir, cli.DefaultDataDir, "The host's data directory, on this machine."),
		context: fs.String(flagContext, "", "The host to reach over its network API, from oikade login."),
	}
}

// transport is --context, then the socket if --data-dir is given, then the current context, then the
// socket. A host it can't reach says so in the user's words.
func (t target) transport() api.Transport {
	transport, name := t.resolve()
	return api.Explained(transport, func(err error) error { return unreachable(transport, name, err) })
}

// explained is transport, and what to call the host it reaches, from one look at the config.
func (t target) explained() (api.Transport, string) {
	transport, name := t.resolve()
	return api.Explained(transport, func(err error) error { return unreachable(transport, name, err) }), name
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
				WithHint("oikade login it again, with the pin oikade-host add prints on it")
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
	t.fs.Visit(func(f *flag.Flag) { given = given || f.Name == flagDataDir })
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

func runTail(ctx context.Context, args []string) error {
	fs := newFlags("tail")
	target := addTarget(fs)
	since := fs.String("since", "", "Print the events from then first: a duration, a time of day or a date and time.")
	asJSON := fs.Bool("json", false, "Print a line of JSON for each event, whose fields stay stable for scripts.")
	refs, err := eventRefs("tail", cli.Parse(fs, args))
	if err != nil {
		return err
	}
	now := time.Now()
	filter := api.EventFilter{Refs: refStrings(refs)}
	if *since != "" {
		if filter.Since, _, err = parseWhen("--since", *since, now); err != nil {
			return err
		}
	}
	transport, host := target.explained()
	return tail(ctx, transport, host, filter, refs, *asJSON, now, std.out, std.err)
}

// tail follows a host's events onto stdout, those of refs if there are any, after the log's from
// filter.Since if it has one. It says on stderr once it's following, so that a quiet house doesn't
// look hung.
func tail(ctx context.Context, t api.Transport, host string, filter api.EventFilter, refs []named, asJSON bool,
	now time.Time, stdout, stderr io.Writer,
) error {
	opened := func() {
		_, _ = fmt.Fprintln(stderr, following(host, refs))
		// A ref that names nothing would otherwise follow in silence for good.
		for _, missing := range unrecorded(ctx, t, refs) {
			_, _ = fmt.Fprintln(stderr, missing)
		}
	}
	lines := &chronicle{p: printerOf(stdout), now: now, narrowed: len(refs) > 0}
	if asJSON {
		lines.json = json.NewEncoder(stdout)
	}
	if filter.Since.IsZero() {
		// What's happening now needs no heading to say it's today.
		lines.day = cli.Day(now, now)
	}
	return api.Tail(ctx, t, filter, opened, lines.add)
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
		if e.By != "" {
			cause = "by " + e.By
		}
		return "wrote " + value(e.Value), cause
	case api.EventRefused:
		return "refused a Matter write of " + value(e.Value), e.Message
	case api.EventDeploy:
		return "deployed version " + cli.ShortVersion(e.Message), ""
	case api.EventAvailability:
		if e.Message == api.AvailabilityOnline {
			return "came back online", e.Result
		}
		return "went offline", e.Result
	case api.EventPlugin:
		if e.Message == "" {
			return "no longer has a problem", ""
		}
		return "has a problem: " + e.Message, ""
	case api.EventMatter:
		if e.Value != nil {
			cause = "fabric " + valueText("", e.Value)
		}
		return e.Message, cause
	case api.EventForget:
		return "forgotten", ""
	case api.EventRename:
		if name, ok := e.Previous.(string); ok {
			cause = "was " + ident.DeviceRef(name).String()
		}
		return "renamed", cause
	case api.EventCall:
		if e.Result != "" {
			return "called " + e.Message + " and it failed", e.Result
		}
		if e.Value != nil {
			cause = "got " + callResult(e.Value)
		}
		return "called " + e.Message, cause
	}
	return "automations " + e.Message, ""
}

// callResult is what a flow's outside call returned, on one line: a value as it is, and anything
// with parts as JSON.
func callResult(v any) string {
	switch v.(type) {
	case bool, string, float64:
		return valueText("", v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

// runResult is how a rule's run ended, in words.
func runResult(result string) string {
	switch result {
	case engine.ResultSucceeded:
		return "ran"
	case engine.ResultFailed:
		return "ran and failed"
	case engine.ResultCancelled:
		return "was cancelled"
	case engine.ResultBusy:
		return "didn't run: it was still running"
	case engine.ResultConditionFalse:
		return "didn't run: its condition was false"
	case engine.ResultConditionError:
		return "didn't run: its condition couldn't be read"
	case engine.ResultMissed:
		return "missed its time"
	case flow.ResultGaveUp:
		return "gave up: it woke too late to carry on"
	case flow.ResultDropped:
		return "was dropped: its code changed under it"
	}
	return strings.ReplaceAll(result, "_", " ")
}

func runSet(ctx context.Context, args []string) error {
	fs := newFlags("set")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := cli.Parse(fs, args)
	if len(words) > 0 && strings.HasPrefix(words[0], string(ident.Secret)+"/") {
		return setSecret(ctx, target, words, *asJSON)
	}
	var device, name string
	var err error
	switch {
	case len(words) == 2:
		if device, name, err = capabilityOf(words[0]); err != nil {
			return err
		}
	case len(words) == 3 && !strings.Contains(words[0], "/"):
		return cli.UsageErrorf("oikade set takes a capability's ref and its value").
			WithHint("oikade set device/%s/%s %s", words[0], words[1], words[2])
	default:
		return cli.UsageErrorf("oikade set takes a capability's ref and its value, or a secret's ref")
	}
	value, err := valueOf(device, name, words[1])
	if err != nil {
		return err
	}
	ref := ident.CapabilityRef(device, name)
	_, _ = fmt.Fprintf(os.Stderr, "Setting %s to %v\n", ref, value)
	resp, err := api.Call(ctx, target.transport(), api.Request{
		Type: api.TypeSet, Device: device, Capability: name, Value: value,
	})
	if err != nil {
		return err
	}
	said := outcome(device, name, value, resp.Outcome)
	if resp.Error == "" && resp.Outcome != nil && !stuck(name, resp.Outcome.Value, value) {
		// Something holds it at another value, so the set didn't do what was asked.
		resp.Error = said
	}
	if resp.Error != "" && resp.Outcome != nil {
		// Held elsewhere, or by a Safety rule, which the host refused the set for: say what lets go.
		if *asJSON {
			_ = finished(resp, true, "", false)
		}
		return cli.Errorf("%s", resp.Error).WithHint("%s", heldHint(ref.String(), *resp.Outcome))
	}
	// A value that hasn't answered isn't a change, so it gets no ✓, and isn't a failure either.
	return finished(resp, *asJSON, said, resp.Outcome != nil)
}

// heldHint says what lets go of a capability something holds at another value: pausing the rule
// that holds it, its release for a Safety claim, which a pause keeps, or waiting out its limits.
func heldHint(ref string, c api.Capability) string {
	for _, claim := range c.Claims {
		if !claim.Winner {
			continue
		}
		if k, _, err := ident.ParseRef(claim.Source); err == nil && (k == ident.Case || k == ident.Flow) {
			if claim.Level == "Safety" {
				return fmt.Sprintf("a Safety claim ends only when its rule releases it, as a pause keeps it: "+
					"oikade status %s says what it's waiting for", claim.Source)
			}
			return fmt.Sprintf("oikade pause %s lets go of it until you resume it", claim.Source)
		}
	}
	if c.Limit != "" {
		return fmt.Sprintf("its limits let go when they're done: oikade status %s says when", ref)
	}
	return fmt.Sprintf("oikade status %s says why", ref)
}

// setSecret sets a secret, whose value comes from stdin or a prompt. One given as an argument is
// refused before anything is sent, as it's in the shell's history now.
func setSecret(ctx context.Context, target target, words []string, asJSON bool) error {
	// Before the name, so a value with a bad name is still told it's in the history.
	if len(words) > 1 {
		return cli.UsageErrorf("oikade set doesn't take a secret's value as an argument, so it sent nothing: the "+
			"value is in your shell's history now").
			WithHint("run oikade set %s alone, which asks for the value or reads it from stdin, and change the "+
				"secret where it came from if it matters", words[0])
	}
	name, err := nameOf(ident.Secret, words[0])
	if err != nil {
		return err
	}
	ref := ident.NewRef(ident.Secret, name)
	value, err := readSecret(os.Stdin)
	if err != nil {
		return err
	}
	resp, err := api.Call(ctx, target.transport(), api.Request{Type: api.TypeSecretSet, Secret: name, SecretValue: value})
	if err != nil {
		return err
	}
	return finished(resp, asJSON, fmt.Sprintf("Set %s. Your process and plugins read it when they next ask, "+
		"so one that reads it once needs a restart", ref), true)
}

// outcome says what a set left the capability at, and why, rather than what it asked for.
// stuck is whether a capability took the value asked of it, or the driver's rounding of it.
func stuck(name string, got, asked any) bool {
	return fmt.Sprint(got) == fmt.Sprint(asked) || rounded(name, got, asked)
}

func outcome(device, name string, asked any, c *api.Capability) string {
	if c == nil {
		return fmt.Sprintf("%s hasn't answered yet: oikade status %s shows it when it does",
			ident.CapabilityRef(device, name), device)
	}
	is := fmt.Sprintf("%s is %v", ident.CapabilityRef(device, name), or(valueText(name, c.Value), "-"))
	if !stuck(name, c.Value, asked) {
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

// printerOf is w as a printer: w itself if it's one, or a plain one, as a test's buffer is.
func printerOf(w io.Writer) *cli.Printer {
	if p, ok := w.(*cli.Printer); ok {
		return p
	}
	return cli.Plain(w, 0)
}

// done prints the outcome of a command that changes something.
func done(resp api.Response, asJSON bool, message string) error {
	return finished(resp, asJSON, message, true)
}

// finished prints how a request ended: its JSON, or message, on a ✓ line if it changed something.
func finished(resp api.Response, asJSON bool, message string, changed bool) error {
	if asJSON {
		out := struct {
			OK        bool            `json:"ok"`
			Error     string          `json:"error,omitempty"`
			Warning   string          `json:"warning,omitempty"`
			Forgotten []api.Forgotten `json:"forgotten,omitempty"`
			Outcome   *api.Capability `json:"outcome,omitempty"`
			Ended     []string        `json:"ended,omitempty"`
			Kept      []string        `json:"kept,omitempty"`
		}{resp.Error == "", resp.Error, resp.Warning, resp.Forgotten, resp.Outcome, resp.Ended, resp.Kept}
		if err := printJSON(std.out, out); err != nil {
			return err
		}
	} else {
		switch {
		case resp.Error != "":
		case changed:
			std.out.Done("%s", message)
		default:
			std.out.Println(message)
		}
		// A forget that failed part way still warns of what it did.
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
	return fmt.Sprintf("Secret %s isn't set: oikade set secret/%s", name, name)
}

// printUnsetSecrets says which secrets the version now running uses that aren't set. It comes after
// the deploy's result, which it doesn't change: the plugin that needs one waits for it.
func printUnsetSecrets(out io.Writer, names []string) {
	for _, name := range names {
		_, _ = fmt.Fprintln(out, unsetSecret(name))
	}
}
