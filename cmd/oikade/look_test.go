package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/testkit"
)

// printers are the ways a command prints: plain, as to a pipe, and styled, as to a terminal.
var printers = map[string]func(io.Writer, int) *cli.Printer{"plain": cli.Plain, "colour": cli.Styled}

// aHome is a home's devices as the host has them, of several plugins and states.
func aHome() (registry.Snapshot, deviceMarks) {
	return registry.Snapshot{Devices: []registry.Device{
		{AppName: "Hall lamp", ID: "1000aabbcc", Driver: "plugin/ewelink-lan", Endpoint: 2, Room: "hall",
			DeviceType: devicetype.OnOffLight, Capabilities: []string{"on"}, Manufacturer: "SONOFF",
			Model: "MINIR4", SerialNumber: "1000aabbcc", FirmwareVersion: "3.6.0"},
		{AppName: "Bureau", ID: "5f1c0c2e-77b3-4f8e-9d4a-1b2c3d4e5f60", Driver: "plugin/toshiba",
			Endpoint: 3, Manufacturer: "Toshiba", Capabilities: []string{"mode", "on", "setpoint", "temperature"}},
		{AppName: "Volet bureau", ID: "io://0812-4471-2093/5", Driver: "plugin/overkiz", Endpoint: 4,
			Room: "study", Capabilities: []string{"position", "target-position", "moving"},
			Manufacturer: "Somfy", Model: "Somfy RS100 io"},
		{AppName: "Porch", ID: "1000ddeeff", Driver: "plugin/ewelink-lan", Endpoint: 5, Capabilities: []string{"on"}},
	}}, deviceMarks{hidden: map[string]bool{"overkiz/io:%2F%2F0812-4471-2093%2F5": true}, declared: map[string]bool{
		"ewelink-lan/1000aabbcc": true, "overkiz/io:%2F%2F0812-4471-2093%2F5": true, "ewelink-lan/1000ddeeff": true,
	}}
}

// --wide shows a device's endpoint, which goes first on a narrow terminal, and its ref in full.
func TestDevicesWideShowsEveryColumn(t *testing.T) {
	list, marks := aHome()
	var b bytes.Buffer
	if _, dropped := printDevices(cli.Plain(&b, 80), list, marks, false); !slices.Contains(dropped, "ENDPOINT") {
		t.Errorf("at 80 columns, dropped %v:\n%s", dropped, b.String())
	}
	b.Reset()
	if _, dropped := printDevices(cli.Plain(&b, 80), list, marks, true); len(dropped) != 0 ||
		!strings.Contains(b.String(), "hardware/toshiba/5f1c0c2e-77b3-4f8e-9d4a-1b2c3d4e5f60") ||
		!strings.Contains(strings.Split(b.String(), "\n")[0], "ENDPOINT") {
		t.Errorf("--wide dropped %v:\n%s", dropped, b.String())
	}
}

// stripStyles is line without its SGR escape codes.
func stripStyles(line string) string {
	for {
		start := strings.Index(line, "\x1b[")
		if start < 0 {
			return line
		}
		end := strings.IndexByte(line[start:], 'm')
		line = line[:start] + line[start+end+1:]
	}
}

// oikade list devices | cat writes no escape codes, as stdout isn't a terminal.
func TestDevicesToAPipeAreUnstyled(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	list, marks := aHome()
	p := cli.NewPrinter(w)
	printDevices(p, list, marks, false)
	p.Hint("--wide shows ENDPOINT")
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.Styled || bytes.Contains(out, []byte("\x1b")) || !bytes.Contains(out, []byte("hardware/ewelink-lan/1000aabbcc")) {
		t.Errorf("styled %v:\n%q", p.Styled, out)
	}
}

// aStatus is a home's status, with each kind of device a line can show.
func aStatus(now time.Time) *api.Status {
	since := now.Add(-8 * time.Minute)
	until := now.Add(4 * time.Minute)
	gone := now.Add(-95 * time.Minute)
	lingers := now.Add(30 * time.Hour)
	on, off := true, false
	return &api.Status{
		Home: api.Home{Version: "f113018fb81c06e4d5fa3500102bfbc4", Process: true, Since: &since},
		Devices: []api.Device{
			{Hardware: "ewelink-lan/1000aabbcc", Ref: "hardware/ewelink-lan/1000aabbcc", Room: "hall", Online: &on,
				Capabilities: []api.Capability{{Name: "on", Value: false}}},
			{
				Hardware: "toshiba/5f1c0c2e", Ref: "hardware/toshiba/5f1c0c2e", Online: &on,
				Undeclared: true,
				Capabilities: []api.Capability{
					{Name: "mode", Value: "cool"},
					{
						Name: "on", Value: true, Level: "Normal", Wanted: true, Case: "too-warm",
						Claims: []api.Claim{{
							Source: "case/toshiba/5f1c0c2e/on/too-warm", Case: "too-warm", Level: "Normal", Value: true, Winner: true,
						}},
						Limit: "MinimumOn", LimitUntil: &until,
					},
					{Name: "setpoint", Value: 21.5},
					{Name: "temperature", Value: 24.25},
				},
			},
			{
				Hardware: "ewelink-lan/1000ddeeff", Ref: "hardware/ewelink-lan/1000ddeeff",
				Problem: "no LAN key: oikade setup ewelink-lan", Plugin: "ewelink-lan",
				Capabilities: []api.Capability{{Name: "on"}},
			},
			{
				Hardware: "overkiz/io:%2F%2F0812-4471-2093%2F5", Ref: "hardware/overkiz/io:%2F%2F0812-4471-2093%2F5",
				Room: "study", Online: &off, OfflineSince: &gone, Plugin: "overkiz",
				Capabilities: []api.Capability{
					{Name: "position", Value: 40.0}, {Name: "target-position", Value: 40.0}, {Name: "moving", Value: false},
				},
			},
		},
		Rules: []api.Rule{
			{
				Name: "toshiba/5f1c0c2e on too-warm", Ref: "case/toshiba/5f1c0c2e/on/too-warm", Kind: "case",
				Device: "toshiba/5f1c0c2e", Capability: "on", Case: "too-warm", Active: &on, Since: since,
			},
			{Name: "morning", Ref: "flow/morning", Kind: "flow"},
		},
		Signals: []api.Signal{
			{
				Name: "anyone-home", Ref: "signal/anyone-home", Value: &on, Since: &gone, OffAt: &lingers,
				Why:  "its conditions are false: group/presence/occupied isn't true on any of them",
				Dead: []string{"hardware/hue/0x9"},
			},
			{Name: "upstairs", Ref: "signal/upstairs", Value: &off, Since: &since},
		},
		Plugins: []api.Plugin{
			{Name: "ewelink-lan", Ref: "plugin/ewelink-lan", Devices: []string{"ewelink-lan/1000ddeeff"}},
			{
				Name: "overkiz", Ref: "plugin/overkiz", Problem: "login refused: check overkiz.Local's Token",
				Devices: []string{"overkiz/io:%2F%2F0812-4471-2093%2F5"},
			},
		},
		Matter: &api.Matter{Serving: true, Port: 5540, Fabrics: []api.Fabric{{Index: 1, Label: "Home"}}},
	}
}

// officeACEvents are office-ac's last events, as the host's log has them.
func officeACEvents(now time.Time) []api.Event {
	at := func(ago time.Duration) time.Time { return now.Add(-ago) }
	return []api.Event{
		{Type: api.EventAvailability, Ref: "hardware/toshiba/5f1c0c2e", Time: at(26 * time.Hour),
			Message: api.AvailabilityOnline},
		{
			Type: api.EventWrite, Ref: "hardware/toshiba/5f1c0c2e/on", Time: at(8 * time.Minute), Capability: "on", Value: true,
			By: "case/toshiba/5f1c0c2e/on/too-warm",
		},
		{
			Type: api.EventChange, Ref: "hardware/toshiba/5f1c0c2e/on", Time: at(8*time.Minute - time.Second), Capability: "on",
			Value: true, Previous: false,
		},
		{
			Type: api.EventChange, Ref: "hardware/toshiba/5f1c0c2e/temperature", Time: at(time.Minute),
			Capability: "temperature", Value: 24.25,
		},
	}
}

// noEvents reads no events of anything.
func noEvents(string) ([]api.Event, error) { return nil, nil }

func TestStatusFitsTheTerminal(t *testing.T) {
	now := time.Date(2026, 11, 18, 14, 0, 0, 0, time.Local)
	for _, width := range []int{80, 120} {
		for name, printer := range printers {
			var out, stderr bytes.Buffer
			printStatus(printer(&out, width), printer(&stderr, width), aStatus(now), false, now)
			// The tables fit, where stderr's sentences wrap.
			for i, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
				if n := len([]rune(stripStyles(line))); n > width {
					t.Errorf("%s at %d: line %d is %d wide: %q", name, width, i, n, line)
				}
			}
			out.WriteString("--- stderr\n")
			out.Write(stderr.Bytes())
			testkit.Golden(t, fmt.Sprintf("status-%s-%d", name, width), out.Bytes())
		}
	}
}

// Each kind of ref status takes has its own view, in the order they're given: a device in full, with
// its last events, a capability the same, a room's devices, a case and the capability it decides, a
// flow, a signal with why and its last events, a plugin and its devices, and the Matter bridge. A
// device's view is in full, so its WHY and the bridge's sentences may run past the terminal's width,
// but the tables fit it.
func TestStatusOfEachKindFitsTheTerminal(t *testing.T) {
	now := time.Date(2026, 11, 18, 14, 0, 0, 0, time.Local)
	refs, err := statusRefs([]string{
		"hardware/toshiba/5f1c0c2e", "hardware/overkiz/io:%2F%2F0812-4471-2093%2F5/position", "room/hall",
		"case/toshiba/5f1c0c2e/on/too-warm", "flow/morning",
		"signal/anyone-home", "plugin/overkiz", "matter",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := func(ref string) ([]api.Event, error) {
		switch ref {
		case "hardware/toshiba/5f1c0c2e":
			return officeACEvents(now), nil
		case "signal/anyone-home":
			return []api.Event{
				{Type: api.EventSignal, Ref: ref, Time: now.Add(-50 * time.Hour), Value: false, Previous: true},
				{Type: api.EventSignal, Ref: ref, Time: now.Add(-95 * time.Minute), Value: true, Previous: false},
			}, nil
		}
		return nil, nil
	}
	for _, width := range []int{80, 120} {
		for name, printer := range printers {
			var out, stderr bytes.Buffer
			printNamed(printer(&out, width), printer(&stderr, width), aStatus(now), refs, events, false, now)
			out.WriteString("--- stderr\n")
			out.Write(stderr.Bytes())
			testkit.Golden(t, fmt.Sprintf("status-refs-%s-%d", name, width), out.Bytes())
		}
	}
}

// The rest of what oikade prints, in the same look: matter's controllers, tail's events, version's
// host and a command's ✓ line.
func TestTheRestLookTheSame(t *testing.T) {
	at := time.Date(2026, 11, 18, 14, 2, 7, 0, time.Local)
	events := []api.Event{
		{Type: api.EventChange, Ref: "hardware/kasa/desk1/on", Capability: "on", Value: true, Previous: false},
		{Type: api.EventChange, Ref: "hardware/toshiba/5f1c0c2e/setpoint", Capability: "setpoint", Value: 21.5},
		{Type: api.EventWrite, Ref: "hardware/kasa/desk1/on", Capability: "on", Value: false},
		{Type: api.EventWrite, Ref: "hardware/kasa/desk1/on", Capability: "on", Value: true,
			By: "case/kasa/desk1/on/occupied"},
		{Type: api.EventWrite, Ref: "hardware/toshiba/5f1c0c2e/setpoint", Capability: "setpoint", Value: 20.0, By: "matter"},
		{Type: api.EventWrite, Ref: "hardware/overkiz/io:%2F%2F0812-4471-2093%2F7/position", Capability: "position",
			Value: 40.0, By: "command"},
		{
			Type: api.EventRefused, Ref: "hardware/kasa/desk1/on", Capability: "on", Value: true,
			Message: "case/kasa/desk1/on/hall-too-humid holds it at Safety",
		},
		{Type: api.EventRule, Ref: "flow/morning", Result: "succeeded"},
		{Type: api.EventRule, Ref: "case/ewelink-lan/1000a1b2c3/on/after-shower", Result: "condition_error"},
		{Type: api.EventPause, Ref: "flow/morning", Message: "paused"},
		{Type: api.EventDeploy, Message: "f113018fb81c06e4d5fa3500102bfbc4"},
		{Type: api.EventProcess, Message: "connected"},
		{Type: api.EventAvailability, Ref: "hardware/ewelink-lan/1000ddeeff", Message: api.AvailabilityOffline},
		{Type: api.EventAvailability, Ref: "hardware/ewelink-lan/1000c0ffee", Message: api.AvailabilityOffline,
			Result: "no LAN key"},
		{Type: api.EventAvailability, Ref: "hardware/ewelink-lan/1000ddeeff", Message: api.AvailabilityOnline},
		{Type: api.EventPlugin, Ref: "plugin/overkiz", Message: "the TaHoma box refused the login"},
		{Type: api.EventPlugin, Ref: "plugin/overkiz"},
		{Type: api.EventMatter, Ref: "matter", Message: api.MatterWindowOpened},
		{Type: api.EventMatter, Ref: "matter", Message: api.MatterControllerPaired, Value: 2.0},
		{Type: api.EventMatter, Ref: "matter", Message: api.MatterWindowClosed},
		{Type: api.EventMatter, Ref: "matter", Message: api.MatterControllerLeft, Value: 1.0},
		{Type: api.EventForget, Ref: "hardware/zigbee2mqtt/0x00158d0001a2b3c4"},
		// A type a host kept from before, which this oikade doesn't know.
		{Type: "rename", Ref: "hardware/relays/valve", Message: "mains-valve"},
		{Type: api.EventCall, Ref: "flow/morning", Message: "forecast", Value: true},
		{Type: api.EventCall, Ref: "flow/morning", Message: "tariff", Value: map[string]any{"cheap": true, "until": "07:00"}},
		{Type: api.EventCall, Ref: "flow/morning", Message: "notify", Result: "the push service timed out"},
	}
	for name, printer := range printers {
		var b bytes.Buffer
		p := printer(&b, 80)
		printMatter(p, &api.Matter{Serving: true, Port: 5540, Fabrics: []api.Fabric{
			{Index: 1, VendorID: 0x1349, Label: "Home"}, {Index: 2, VendorID: 0x1384},
		}}, false)
		p.Println("---")
		for _, e := range events {
			e.Time = at
			p.Println(eventLine(p, e))
		}
		p.Println("---")
		p.Println(hostVersionLine(api.Response{HostVersion: "v0.2.0-alpha.2", GOOS: "linux", GOARCH: "arm64"}, thisMachine))
		p.Println(hostVersionLine(api.Response{GOOS: "linux", GOARCH: "amd64"}, "europa"))
		p.Println("---")
		resp := api.Response{Version: "f113018fb81c06e4d5fa3500102bfbc4"}
		if err := report(p, resp, "Deployed version", ", with 6 values from .env", ""); err != nil {
			t.Fatal(err)
		}
		testkit.Golden(t, "rest-"+name, b.Bytes())
		for i, line := range strings.Split(b.String(), "\n") {
			if strings.HasSuffix(stripStyles(line), " ") {
				t.Errorf("%s: line %d ends in a blank: %q", name, i, line)
			}
		}
	}
}

// version says its own, then why it has no host's. A context that isn't there is an error.
func TestVersionSaysWhyItHasNoHosts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	out, errs, code := runOikade(t, cli.Plain, "version", "--data-dir", dir)
	if code != 0 || out != "oikade "+cli.Version(version)+"\n" ||
		errs != "Couldn't ask the host for its version: no host is running on this machine with its data in "+dir+"\n" {
		t.Errorf("no host: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if _, errs, code := runOikade(t, cli.Plain, "version", "--context", "nowhere"); code != 1 ||
		!strings.HasPrefix(errs, "Error: ") {
		t.Errorf("no such context: exit %d, stderr %q", code, errs)
	}
}

// Only a change gets a ✓: a value that hasn't answered or was held back doesn't.
func TestOnlyAChangeGetsATick(t *testing.T) {
	saved := std
	t.Cleanup(func() { std = saved })
	for _, tc := range []struct {
		changed bool
		want    string
	}{{true, "\x1b[32m✓\x1b[0m Paused flow/morning\n"}, {false, "Paused flow/morning\n"}} {
		var b bytes.Buffer
		std.out = cli.Styled(&b, 80)
		if err := finished(api.Response{}, "flow/morning", false, "Paused flow/morning", tc.changed); err != nil ||
			b.String() != tc.want {
			t.Errorf("changed %v: %q, %v", tc.changed, b.String(), err)
		}
	}
	if stuck("on", nil, true) || !stuck("brightness", 33.1, 33.0) || stuck("brightness", 30.0, 33.0) {
		t.Error("stuck")
	}
}
