package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/ident"
)

// runForget drops devices from the host's record, which come back if their plugin reports them
// again.
func runForget(ctx context.Context, args []string) error {
	fs := newFlags("forget")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	allow := fs.Bool("allow-safety", false, "Forget a device a Safety claim holds, and end that claim.")
	plugin := fs.String("plugin", "", "Forget every device of this plugin, such as one your home no longer imports.")
	words := cli.Parse(fs, args)
	devices, err := forgettable(words)
	if err != nil {
		return err
	}
	if *plugin, err = nameOf(ident.Plugin, *plugin); err != nil {
		return err
	}
	switch {
	case *plugin != "" && len(devices) > 0:
		return cli.UsageErrorf("oikade forget takes devices or --plugin NAME, not both")
	case *plugin == "" && len(devices) == 0:
		return cli.UsageErrorf("oikade forget takes the devices to forget, or --plugin NAME")
	}
	req := api.Request{Type: api.TypeForget, Devices: devices, Plugin: *plugin, AllowSafety: *allow}
	resp, callErr := api.Call(ctx, target.transport(), req)
	if *asJSON {
		// What it forgot, even if it then failed, as a forget of a plugin's devices can part way.
		out := refsJSON{OK: callErr == nil && resp.Error == "", Error: resp.Error, Warning: resp.Warning,
			Refs: []string{}, Forgotten: resp.Forgotten}
		if callErr != nil {
			out.Error = callErr.Error()
		}
		for _, f := range resp.Forgotten {
			out.Refs = append(out.Refs, f.Ref)
		}
		_ = printJSON(std.out, out)
		if out.OK {
			return nil
		}
		return errors.New(out.Error)
	}
	if callErr != nil {
		return callErr
	}
	printForgotten(std.out, resp.Forgotten, *plugin)
	if resp.Warning != "" {
		std.err.Warn("%s", resp.Warning)
	}
	switch {
	case resp.NeedsAllowSafety:
		return cli.Errorf("%s", resp.Error).WithHint("--allow-safety forgets it anyway, and ends that claim")
	case resp.Error != "":
		return errors.New(resp.Error)
	}
	return nil
}

// forgettable is the devices forget's arguments name. Any other kind is a command typed wrong, and
// one that's removed rather than forgotten names remove.
func forgettable(args []string) ([]string, error) {
	refs, err := refsOf(args)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(refs))
	for _, n := range refs {
		switch {
		case n.kind == ident.Device && len(n.parts) == 1:
			if !slices.Contains(names, n.parts[0]) {
				names = append(names, n.parts[0])
			}
		case n.kind == ident.Client:
			return nil, cli.UsageErrorf("oikade forget takes devices, and a %s is removed, not forgotten", n.kind).
				WithHint("oikade remove %s removes it", n.ref)
		case n.kind == ident.Plugin && len(n.parts) == 1:
			return nil, cli.UsageErrorf("oikade forget takes devices, not %s", n.ref).
				WithHint("oikade forget --plugin %s forgets every device of it", n.parts[0])
		case n.kind == ident.Matter:
			return nil, cli.UsageErrorf("oikade forget takes devices, and matter is the Matter bridge").
				WithHint("a device called matter is device/matter")
		default:
			return nil, cli.UsageErrorf("oikade forget takes devices, not %s", n.ref).
				WithHint("give each as device/NAME, or its name alone")
		}
	}
	return names, nil
}

// printForgotten says what became of each device a forget forgot: the claims it ended, that it left
// the Matter bridge, and then whether they come back. plugin is the plugin whose devices they were,
// if the forget named one.
func printForgotten(out *cli.Printer, forgotten []api.Forgotten, plugin string) {
	if len(forgotten) == 0 {
		return
	}
	for _, f := range forgotten {
		var did []string
		if len(f.Ended) > 0 {
			did = append(did, "ended its claims by "+andList(f.Ended))
		}
		if f.OffMatter {
			did = append(did, "took it off the Matter bridge")
		}
		line := "Forgot " + f.Ref
		if len(did) > 0 {
			line += ": " + strings.Join(did, ", and ")
		}
		out.Done("%s", line)
	}
	if plugin != "" {
		out.Println(comesBack(forgotten, plugin, fateOf(forgotten[0]), true))
		return
	}
	// A line for each plugin, and for what becomes of its devices, which is usually one line.
	type group struct {
		plugin string
		fate   fate
	}
	var groups []group
	byGroup := map[group][]api.Forgotten{}
	for _, f := range forgotten {
		k := group{f.Plugin, fateOf(f)}
		if _, ok := byGroup[k]; !ok {
			groups = append(groups, k)
		}
		byGroup[k] = append(byGroup[k], f)
	}
	for _, k := range groups {
		out.Println(comesBack(byGroup[k], k.plugin, k.fate, len(groups) == 1))
	}
}

// fate is what becomes of a forgotten device.
type fate int

const (
	// backAtNextReport is a device of a driver that isn't a plugin, which its next report brings back.
	backAtNextReport fate = iota
	// forgottenWhileListed is a plugin's device: it comes back only if it leaves the plugin's list and
	// returns, as a device re-paired in its app does.
	forgottenWhileListed
	// nothingReports is a device whose plugin your home doesn't import.
	nothingReports
)

func fateOf(f api.Forgotten) fate {
	switch {
	case f.ComesBack:
		return backAtNextReport
	case f.Unimported:
		return nothingReports
	}
	return forgottenWhileListed
}

// comesBack says what becomes of devices of a plugin. alone says they're all the forget forgot, so
// "it" or "they" names them.
func comesBack(devices []api.Forgotten, plugin string, f fate, alone bool) string {
	refs := make([]string, 0, len(devices))
	for _, d := range devices {
		refs = append(refs, d.Ref)
	}
	one := len(devices) == 1
	subject, them, each := andList(refs), "them", "each "
	switch {
	case alone && one:
		subject = "it"
	case alone:
		subject = "they"
	}
	if one {
		them, each = "it", ""
	}
	verb := func(single, plural string) string {
		if one {
			return single
		}
		return plural
	}
	switch f {
	case backAtNextReport:
		return fmt.Sprintf("%s %s back when %s driver next reports %s", capital(subject), verb("comes", "come"),
			verb("its", "their"), them)
	case nothingReports:
		return fmt.Sprintf("Your home doesn't import %s, so %s %s gone", plugin, subject, verb("stays", "stay"))
	}
	return fmt.Sprintf("%s %s forgotten while %s lists %s, and %scomes back if it leaves the list and returns, as a "+
		"device re-paired in its app does", capital(subject), verb("stays", "stay"), plugin, them, each)
}

// capital is s with its first letter a capital, for a sentence that starts with a pronoun.
func capital(s string) string {
	if s == "" || strings.Contains(s, "/") {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// runRemove removes what the host owns outright, clients, which are gone for good.
func runRemove(ctx context.Context, args []string) error {
	fs := newFlags(verbRemove)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := cli.Parse(fs, args)
	refs, err := removable(words)
	if err != nil {
		return err
	}
	removed := []string{}
	// --json says how it ended, however it did, with what was removed before it failed.
	failed := func(err error) error {
		if *asJSON {
			_ = printJSON(std.out, refsJSON{OK: false, Error: err.Error(), Refs: removed})
		}
		return err
	}
	t := target.transport()
	if err := onRecord(ctx, t, refs); err != nil {
		return failed(err)
	}
	for _, n := range refs {
		resp, err := api.Call(ctx, t, api.Request{Type: api.TypeClientRemove, Client: n.parts[0]})
		if err == nil && resp.Error != "" {
			err = errors.New(resp.Error)
		}
		if err != nil {
			return failed(err)
		}
		removed = append(removed, n.ref)
		if !*asJSON {
			std.out.Done("%s", removedMessage(n, resp.Cancelled))
		}
	}
	if *asJSON {
		return printJSON(std.out, refsJSON{OK: true, Refs: removed})
	}
	return nil
}

// refsJSON is what forget and remove print under --json: whether they did it, and why not, the refs
// they forgot or removed, even if they then failed, and for forget what became of each device.
type refsJSON struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Warning   string          `json:"warning,omitempty"`
	Refs      []string        `json:"refs"`
	Forgotten []api.Forgotten `json:"forgotten,omitempty"`
}

// removable is what remove's arguments name: clients, each by its ref. A device is forgotten, so
// remove given one names forget, and a bare word could be either, so it needs its kind.
func removable(args []string) ([]named, error) {
	if len(args) == 0 {
		return nil, cli.UsageErrorf("oikade remove takes the clients to remove, such as client/old-laptop")
	}
	out := make([]named, 0, len(args))
	for _, arg := range args {
		if !strings.Contains(arg, "/") {
			return nil, cli.UsageErrorf("oikade remove takes client/NAME, not %s", arg).
				WithHint("oikade remove client/%s removes a client, and oikade forget %s forgets a device", arg, arg)
		}
		if strings.HasPrefix(arg, "secret/") {
			return nil, cli.UsageErrorf("oikade remove takes client/NAME, and a secret is one of your values now").
				WithHint("%s", valuesHint)
		}
		kind, parts, err := ident.ParseRef(arg)
		switch {
		case err != nil && strings.HasPrefix(arg, string(ident.Client)+"/"): // What's wrong is its name.
			return nil, cli.UsageErrorf("%s", err)
		case err == nil && kind == ident.Device && len(parts) == 1:
			return nil, cli.UsageErrorf("oikade remove doesn't take devices: a device is forgotten, as its plugin may "+
				"report it again").WithHint("oikade forget %s forgets it", arg)
		case err != nil || kind != ident.Client || len(parts) != 1:
			return nil, cli.UsageErrorf("oikade remove takes client/NAME, not %s", arg)
		}
		n := named{ref: ident.NewRef(kind, parts...).String(), kind: kind, parts: parts}
		if !slices.ContainsFunc(out, func(o named) bool { return o.ref == n.ref }) {
			out = append(out, n)
		}
	}
	return out, nil
}

// onRecord checks that the host has every client refs name, before remove changes anything.
func onRecord(ctx context.Context, t api.Transport, refs []named) error {
	resp, err := call(ctx, t, api.Request{Type: api.TypeClients})
	if err != nil {
		return err
	}
	var missing []string
	for _, n := range refs {
		if !slices.ContainsFunc(resp.Clients, func(c api.Client) bool { return c.Ref == n.ref }) {
			missing = append(missing, n.ref)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return cli.Errorf("there's no %s, so nothing was removed", andList(missing)).
		WithHint("oikade list clients lists what there is")
}

// removedMessage says what removing n did. cancelled says it was a client waiting for its login.
func removedMessage(n named, cancelled bool) string {
	if cancelled {
		return fmt.Sprintf("Cancelled %s's login: its code doesn't work now", n.ref)
	}
	return fmt.Sprintf("Removed %s: its token stopped working, and its streams ended. The context on its machine "+
		"stays, and fails until it logs in again", n.ref)
}
