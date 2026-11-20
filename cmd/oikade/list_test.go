package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/registry"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// listed is what each kind oikade list takes prints: of a home with some of it, and of one with none.
type listing struct {
	kind  string
	print func(out, stderr *cli.Printer, empty, asJSON bool) error
}

// listings are each kind's, as of now.
func listings(now time.Time) []listing {
	none := api.Status{Home: api.Home{Version: "v3", Process: true}}
	st := func(empty bool) *api.Status {
		if empty {
			return &none
		}
		s := aStatus(now)
		on := true
		s.Rules = append(s.Rules, api.Rule{
			Name: "leak-shutoff", Ref: "case/mains-valve/on/leak-shutoff", Kind: "case", Device: "mains-valve",
			Capability: "on", Case: "leak-shutoff", Safety: true, Paused: true, Active: &on,
		})
		s.Plugins[0].NeedsSetup = true
		s.Plugins = append(s.Plugins, api.Plugin{
			Name: "zigbee2mqtt", Ref: "plugin/zigbee2mqtt", Devices: []string{"shed-relay"},
		})
		s.Devices = append(s.Devices, api.Device{Name: "shed-relay", Plugin: "zigbee2mqtt", PluginNotImported: true})
		return s
	}
	return []listing{
		{kindDevices, func(out, stderr *cli.Printer, empty, asJSON bool) error {
			list, marks := aHome()
			if empty {
				list.Devices = []registry.Device{{Name: "old-plug", Removed: true}}
			}
			resp := api.Response{Registry: &list, Unimported: []api.Unimported{{Device: "porch-light"}}}
			for name := range marks.hidden {
				resp.HiddenFromMatter = append(resp.HiddenFromMatter, name)
			}
			return printDeviceList(out, stderr, resp, listOptions{json: asJSON})
		}},
		{kindRules, func(out, stderr *cli.Printer, empty, asJSON bool) error {
			return printRules(out, stderr, st(empty), listOptions{json: asJSON})
		}},
		{kindPlugins, func(out, stderr *cli.Printer, empty, asJSON bool) error {
			return printPlugins(out, stderr, st(empty), listOptions{json: asJSON})
		}},
		{kindSecrets, func(out, stderr *cli.Printer, empty, asJSON bool) error {
			if empty {
				return printSecrets(out, stderr, nil, nil, asJSON, now)
			}
			secrets := []api.Secret{
				{Name: "toshiba-password", Ref: "secret/toshiba-password", Changed: now.AddDate(0, 0, -3)},
				{Name: "ewelink-1000aabbcc", Ref: "secret/ewelink-1000aabbcc", Changed: now.Add(-40 * time.Minute)},
			}
			return printSecrets(out, stderr, secrets, []string{"overkiz", "toshiba-password"}, asJSON, now)
		}},
		{kindClients, func(out, stderr *cli.Printer, empty, asJSON bool) error {
			if empty {
				return printClients(out, stderr, nil, asJSON, now)
			}
			clients := []api.Client{
				{Name: "laptop", Ref: "client/laptop", Created: now.AddDate(0, 0, -8), LastUsed: now.Add(-8 * time.Second)},
				{Name: "ci", Ref: "client/ci", Created: now.Add(-5 * time.Hour)},
			}
			return printClients(out, stderr, clients, asJSON, now)
		}},
		{kindContexts, func(out, stderr *cli.Printer, empty, asJSON bool) error {
			cfg := clientConfig{}
			if !empty {
				cfg = clientConfig{Current: "home", Contexts: map[string]hostContext{
					"home": {Address: "europa.local:8443", Pin: "sha256:9f2c41d0a7b3e8f6", Token: "oik_x"},
					"ci":   {Address: "10.0.0.4:8443", Pin: "sha256:0b7d5e2a91c4f3e8"},
				}}
			}
			return printContexts(out, stderr, cfg, listOptions{json: asJSON})
		}},
	}
}

// Each kind's table, then what it says with none, plain and styled, at 80 and 120 columns.
func TestListFitsTheTerminal(t *testing.T) {
	now := time.Date(2026, 11, 18, 14, 30, 0, 0, time.Local)
	for _, l := range listings(now) {
		for _, width := range []int{80, 120} {
			for name, printer := range printers {
				var b bytes.Buffer
				for _, empty := range []bool{false, true} {
					if empty {
						b.WriteString("--- none\n")
					}
					var out, stderr bytes.Buffer
					if err := l.print(printer(&out, width), printer(&stderr, width), empty, false); err != nil {
						t.Fatal(err)
					}
					// The table fits, where stderr's sentences wrap.
					for i, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
						if n := len([]rune(stripStyles(line))); n > width {
							t.Errorf("%s %s at %d: line %d is %d wide: %q", l.kind, name, width, i, n, line)
						}
					}
					b.Write(out.Bytes())
					b.WriteString("--- stderr\n")
					b.Write(stderr.Bytes())
				}
				testkit.Golden(t, fmt.Sprintf("list-%s-%s-%d", l.kind, name, width), b.Bytes())
			}
		}
	}
}

// --json is an array, each thing with its ref, and an empty one when there's none, with nothing on
// stderr.
func TestListJSONIsAnArray(t *testing.T) {
	now := time.Now()
	for _, l := range listings(now) {
		var out, stderr bytes.Buffer
		if err := l.print(cli.Plain(&out, 0), cli.Plain(&stderr, 0), false, true); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.String(), "[{") || stderr.Len() != 0 {
			t.Errorf("list %s --json: %s%s", l.kind, out.String(), stderr.String())
		}
		if l.kind != kindContexts && strings.Count(out.String(), `"ref":"`) != strings.Count(out.String(), `"name":"`) {
			t.Errorf("list %s --json has a thing with no ref: %s", l.kind, out.String())
		}
		out.Reset()
		if err := l.print(cli.Plain(&out, 0), cli.Plain(&stderr, 0), true, true); err != nil {
			t.Fatal(err)
		}
		if out.String() != "[]\n" || stderr.Len() != 0 {
			t.Errorf("list %s --json of none: %q%q", l.kind, out.String(), stderr.String())
		}
	}
}

// A kind's word is singular or plural, and anything else is a command typed wrong.
func TestListTakesOneKind(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, word := range []string{"context", "contexts"} {
		if out, errs, code := runOikade(t, cli.Plain, "list", word); code != 0 || out != "" ||
			!strings.HasPrefix(errs, "No contexts") {
			t.Errorf("list %s exits %d:\n%s%s", word, code, out, errs)
		}
	}
	for args, want := range map[string]string{
		"list":                        "Error: oikade list takes one kind\n",
		"list devices rules":          "Error: oikade list takes one kind\n",
		"list device/hall-lamp":       "Error: oikade list doesn't take device/hall-lamp\n",
		"list rules --all":            "Error: --all is only for oikade list devices\n",
		"list contexts --context lab": "Error: --context picks a host, and oikade list contexts asks none\n",
		"list contexts --data-dir /x": "Error: --data-dir picks a host, and oikade list contexts asks none\n",
	} {
		if _, errs, code := runOikade(t, cli.Plain, strings.Fields(args)...); code != 2 || !strings.HasPrefix(errs, want) {
			t.Errorf("oikade %s exits %d:\n%s\nwant 2, and stderr starting %q", args, code, errs, want)
		}
	}
}

// A word from another tool, or from before, names the command to use, and exits 2.
func TestAWordFromElsewhereNamesTheCommand(t *testing.T) {
	var b bytes.Buffer
	for _, args := range []string{
		"devices", "devices --json", "devices --wide", "devices --all", "secret list", "client list", "logs", "events", "ls",
		"get", "show",
	} {
		_, errs, code := runOikade(t, cli.Plain, strings.Fields(args)...)
		_, _ = fmt.Fprintf(&b, "$ oikade %s\n%sexit %d\n\n", args, errs, code)
		if code != 2 {
			t.Errorf("oikade %s exits %d", args, code)
		}
	}
	testkit.Golden(t, "list-instead-plain", b.Bytes())
}

// A forgotten device is listed only with --all, and a list without one says it's there.
func TestListDevicesLeavesForgottenOnesOut(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{Name: "hall-lamp", ID: "l", Driver: "plugin/ewelink-lan", Endpoint: 2},
		{Name: "old-plug", ID: "p", Driver: "plugin/ewelink-lan", Endpoint: 3, Removed: true},
	}}
	var out, stderr bytes.Buffer
	resp := api.Response{Registry: &list}
	if err := printDeviceList(cli.Plain(&out, 0), cli.Plain(io.Discard, 0), resp, listOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "old-plug") || len(list.Devices) != 2 {
		t.Errorf("without --all:\n%s", out.String())
	}
	out.Reset()
	err := printDeviceList(cli.Plain(&out, 0), cli.Plain(&stderr, 0), resp, listOptions{all: true})
	if err != nil || !strings.Contains(out.String(), "old-plug") || !strings.Contains(out.String(), "forgotten") {
		t.Errorf("with --all:\n%s", out.String())
	}
}
