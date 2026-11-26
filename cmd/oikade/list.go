package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"slices"
	"strings"
	"time"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/registry"
)

const verbList = "list"

const (
	kindDevices  = "devices"
	kindRules    = "rules"
	kindSignals  = "signals"
	kindPlugins  = "plugins"
	kindClients  = "clients"
	kindContexts = "contexts"
)

// listKinds are the kinds oikade list takes, in the order help gives them.
var listKinds = []string{kindDevices, kindRules, kindSignals, kindPlugins, kindClients, kindContexts}

// listKind is the kind a word names, singular or plural.
func listKind(word string) (string, bool) {
	for _, k := range listKinds {
		if word == k || word == strings.TrimSuffix(k, "s") {
			return k, true
		}
	}
	return "", false
}

// listOptions are how oikade list prints.
type listOptions struct{ json, wide, all bool }

func runList(ctx context.Context, args []string) error {
	fs := newFlags(verbList)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print an array, each thing with its ref, whose fields stay stable for scripts.")
	wide := fs.Bool("wide", false, "Show every column, however wide the terminal.")
	all := fs.Bool("all", false, "List forgotten devices too, which keep their Matter endpoints.")
	words := cli.Parse(fs, args)
	kinds := "oikade list takes " + orList(listKinds)
	if len(words) != 1 {
		return cli.UsageErrorf("oikade list takes one kind").WithHint("%s", kinds)
	}
	kind, ok := listKind(words[0])
	if words[0] == "secrets" || words[0] == "secret" {
		return cli.UsageErrorf("oikade list doesn't take %s: a secret is one of your values now", words[0]).
			WithHint("%s", valuesHint)
	}
	if !ok {
		return cli.UsageErrorf("oikade list doesn't take %s", words[0]).WithHint("%s", kinds)
	}
	if *all && kind != kindDevices {
		return cli.UsageErrorf("--all is only for oikade list devices")
	}
	o := listOptions{json: *asJSON, wide: *wide, all: *all}
	if kind == kindContexts { // The laptop's own, so no host.
		var target string
		fs.Visit(func(f *flag.Flag) {
			if f.Name == flagContext || f.Name == flagDataDir {
				target = f.Name
			}
		})
		if target != "" {
			return cli.UsageErrorf("--%s picks a host, and oikade list contexts asks none", target)
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return printContexts(std.out, std.err, cfg, o)
	}
	t := target.transport()
	now := time.Now()
	switch kind {
	case kindDevices:
		resp, err := devicesOf(ctx, t)
		if err != nil {
			return err
		}
		return printDeviceList(std.out, std.err, resp, o)
	case kindClients:
		resp, err := call(ctx, t, api.Request{Type: api.TypeClients})
		if err != nil {
			return err
		}
		return printClients(std.out, std.err, resp.Clients, o.json, now)
	}
	st, err := statusOf(ctx, t)
	if err != nil {
		return err
	}
	switch kind {
	case kindRules:
		return printRules(std.out, std.err, st, o)
	case kindSignals:
		return printSignals(std.out, std.err, st, o, now)
	}
	return printPlugins(std.out, std.err, st, o)
}

// call asks the host for something, and fails with what the host says if it refuses.
func call(ctx context.Context, t api.Transport, req api.Request) (api.Response, error) {
	resp, err := api.Call(ctx, t, req)
	if err != nil {
		return resp, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// statusOf is what the host knows now.
func statusOf(ctx context.Context, t api.Transport) (*api.Status, error) {
	resp, err := call(ctx, t, api.Request{Type: api.TypeStatus})
	if err != nil {
		return nil, err
	}
	if resp.Status == nil {
		return nil, errors.New("the host sent no status")
	}
	return resp.Status, nil
}

// printJSON prints v on out, as a line of JSON.
func printJSON(out *cli.Printer, v any) error { return json.NewEncoder(out).Encode(v) }

// printDeviceList prints the devices on record, a line each, without the forgotten ones unless all,
// then how many your home's code doesn't declare.
func printDeviceList(out, stderr *cli.Printer, resp api.Response, o listOptions) error {
	list := *resp.Registry
	forgotten := 0
	if !o.all {
		before := len(list.Devices)
		list.Devices = slices.DeleteFunc(slices.Clone(list.Devices), func(d registry.Device) bool { return d.Removed })
		forgotten = before - len(list.Devices)
	}
	marks := deviceMarks{unimported: map[string]bool{}, hidden: map[string]bool{}, declared: map[string]bool{}}
	for _, u := range resp.Unimported {
		marks.unimported[u.Device] = true
	}
	for _, key := range resp.HiddenFromMatter {
		marks.hidden[key] = true
	}
	for _, key := range resp.Declared {
		marks.declared[key] = true
	}
	if o.json {
		return printJSON(out, listed(list, marks))
	}
	if len(list.Devices) == 0 {
		noDevices(stderr, forgotten)
		return nil
	}
	undeclared, dropped := printDevices(out, list, marks, o.wide)
	if len(dropped) > 0 {
		stderr.Hint("--wide shows %s", andList(dropped))
	}
	countUndeclared(stderr, undeclared)
	stderr.Hint("oikade status shows what each is doing now, and what's wrong with it")
	return nil
}

// listedRule is a rule as oikade list rules --json prints it: how it's set up, and not what it's
// doing, which is oikade status's.
type listedRule struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	// Kind is case or flow. Empty is a paused rule no running code names.
	Kind       string `json:"kind,omitempty"`
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Case       string `json:"case,omitempty"`
	Safety     bool   `json:"safety,omitempty"`
	Paused     bool   `json:"paused,omitempty"`
}

// printRules prints the rules the host knows, a line each: a case's or a flow's ref, its kind, and
// whether it's Safety or paused.
func printRules(out, stderr *cli.Printer, st *api.Status, o listOptions) error {
	rules := make([]listedRule, 0, len(st.Rules))
	for _, r := range st.Rules {
		rules = append(rules, listedRule{
			Name: r.Name, Ref: r.Ref, Kind: r.Kind, Device: r.Device, Capability: r.Capability, Case: r.Case,
			Safety: r.Safety, Paused: r.Paused,
		})
	}
	if o.json {
		return printJSON(out, rules)
	}
	if len(rules) == 0 {
		if !st.Home.Process {
			stderr.Println("No rules known: the host knows your rules from your automations, which aren't connected. " +
				"oikade status says how they are.")
			return nil
		}
		stderr.Println("No rules. A case or a flow in your home's code is one, once oikade deploy runs it.")
		return nil
	}
	table := cli.Table{Columns: []cli.Column{{Header: nameHeader}, {Header: "KIND"}, {Header: stateHeader}}, Wide: o.wide}
	for _, r := range rules {
		var state []string
		if r.Safety {
			state = append(state, "Safety")
		}
		if r.Paused {
			state = append(state, "paused")
		}
		table.Rows = append(table.Rows, []string{or(r.Ref, r.Name), or(r.Kind, "gone"), strings.Join(state, ", ")})
	}
	out.Table(table)
	stderr.Hint("oikade status REF shows what one is doing now")
	return nil
}

// printSignals prints the running version's signals, a line each, in the order your home made them,
// with each one's value and why, or as JSON, the same as oikade status --json has them.
func printSignals(out, stderr *cli.Printer, st *api.Status, o listOptions, now time.Time) error {
	if o.json {
		return printJSON(out, append([]api.Signal{}, st.Signals...))
	}
	if len(st.Signals) == 0 {
		stderr.Println("No signals. home.Signal in your home's code makes one, once oikade deploy runs it.")
		return nil
	}
	table := cli.Table{
		Columns: []cli.Column{{Header: nameHeader}, {Header: "VALUE"}, {Header: "WHY", Shrink: true}},
		Wide:    o.wide,
	}
	for _, s := range st.Signals {
		value := strings.TrimPrefix(signalLine(s, now), s.Ref+"  ")
		table.Rows = append(table.Rows, []string{s.Ref, value, s.Why})
	}
	if dropped := out.Table(table); len(dropped) > 0 {
		stderr.Hint("--wide shows %s", andList(dropped))
	}
	stderr.Hint("oikade status signal/NAME says why one is what it is, and what it leaves out")
	return nil
}

// listedPlugin is a plugin as oikade list plugins --json prints it.
type listedPlugin struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	// NotImported is a plugin with devices on record that the running version doesn't import.
	NotImported bool `json:"notImported,omitempty"`
	// NeedsSetup is a plugin that says its setup needs running.
	NeedsSetup bool     `json:"needsSetup,omitempty"`
	Devices    []string `json:"devices"`
}

// printPlugins prints the plugins the host knows, a line each: those your home imports, those
// running, and those with devices on record, with whether they need their setup, or aren't imported,
// and their devices.
func printPlugins(out, stderr *cli.Printer, st *api.Status, o listOptions) error {
	unimported := map[string]bool{}
	for _, d := range st.Devices {
		if d.PluginNotImported && d.Plugin != "" {
			unimported[d.Plugin] = true
		}
	}
	plugins := make([]listedPlugin, 0, len(st.Plugins))
	for _, p := range st.Plugins {
		plugins = append(plugins, listedPlugin{
			Name: p.Name, Ref: p.Ref, NotImported: unimported[p.Name], NeedsSetup: p.NeedsSetup,
			Devices: append([]string{}, p.Devices...),
		})
	}
	if o.json {
		return printJSON(out, plugins)
	}
	if len(plugins) == 0 {
		stderr.Println("No plugins. Your home imports one with oikade.Plugin, and oikade deploy starts it.")
		return nil
	}
	table := cli.Table{
		Columns: []cli.Column{{Header: nameHeader}, {Header: stateHeader}, {Header: "DEVICES", Shrink: true}},
		Wide:    o.wide,
	}
	for _, p := range plugins {
		var state []string
		if p.NotImported {
			state = append(state, notImported)
		}
		if p.NeedsSetup {
			state = append(state, "needs setup: run oikade setup "+p.Name)
		}
		table.Rows = append(table.Rows, []string{p.Name, strings.Join(state, ", "), strings.Join(p.Devices, ", ")})
	}
	if dropped := out.Table(table); len(dropped) > 0 {
		stderr.Hint("--wide shows %s", andList(dropped))
	}
	stderr.Hint("oikade status plugin/NAME says what's wrong with one")
	return nil
}

// listedContext is a context as oikade list contexts --json prints it, and never its token.
type listedContext struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Pin     string `json:"pin"`
	// Current is the one commands use when they're given none.
	Current bool `json:"current,omitempty"`
	// TokenFromEnv is one whose token comes from OIKADE_TOKEN.
	TokenFromEnv bool `json:"tokenFromEnv,omitempty"`
}

// printContexts lists the hosts this machine has logged in to, with the current one marked.
func printContexts(out, stderr *cli.Printer, cfg clientConfig, o listOptions) error {
	contexts := make([]listedContext, 0, len(cfg.Contexts))
	for name, c := range cfg.Contexts {
		contexts = append(contexts, listedContext{
			Name: name, Address: c.Address, Pin: c.Pin, Current: name == cfg.Current, TokenFromEnv: c.Token == "",
		})
	}
	slices.SortFunc(contexts, func(a, b listedContext) int { return strings.Compare(a.Name, b.Name) })
	if o.json {
		return printJSON(out, contexts)
	}
	if len(contexts) == 0 {
		stderr.Println("No contexts, so commands go to this machine's host. oikade login adds one: " +
			"oikade-host add client/NAME, on the host's machine, prints the line to run.")
		return nil
	}
	table := cli.Table{
		Columns: []cli.Column{
			{Header: nameHeader}, {Header: "ADDRESS"}, {Header: stateHeader}, {Header: "PIN", Drop: 1, Dim: true},
		},
		Wide: o.wide,
	}
	for _, c := range contexts {
		var state []string
		if c.Current {
			state = append(state, "current")
		}
		if c.TokenFromEnv {
			state = append(state, "token from "+tokenEnv)
		}
		table.Rows = append(table.Rows, []string{c.Name, c.Address, strings.Join(state, ", "), c.Pin})
	}
	if dropped := out.Table(table); len(dropped) > 0 {
		stderr.Hint("--wide shows %s", andList(dropped))
	}
	if cfg.Current == "" {
		stderr.Hint("none is current, so commands go to this machine's host: --context NAME picks one")
	}
	return nil
}
