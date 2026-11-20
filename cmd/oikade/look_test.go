package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/registry"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// printers are the ways a command prints: plain, as to a pipe, and styled, as to a terminal.
var printers = map[string]func(io.Writer, int) *cli.Printer{"plain": cli.Plain, "colour": cli.Styled}

// aHome is a home's devices as the host has them, of several plugins and states.
func aHome() (registry.Snapshot, deviceMarks) {
	return registry.Snapshot{Devices: []registry.Device{
		{Name: "hall-lamp", ID: "1000aabbcc", Driver: "plugin/ewelink-lan", Endpoint: 2, Room: "hall", Declared: true,
			DeviceType: devicetype.OnOffLight, Capabilities: []string{"on"}, Manufacturer: "SONOFF", Model: "MINIR4",
			SerialNumber: "1000aabbcc", FirmwareVersion: "3.6.0"},
		{Name: "office-ac", ID: "5f1c0c2e-77b3-4f8e-9d4a-1b2c3d4e5f60", Driver: "plugin/toshiba", Endpoint: 3,
			Manufacturer: "Toshiba", Capabilities: []string{"mode", "on", "setpoint", "temperature"}},
		{Name: "study-blind", ID: "io://0812-4471-2093/5", Driver: "plugin/overkiz", Endpoint: 4, Room: "study",
			Declared: true, Capabilities: []string{"position", "target-position", "moving"},
			Manufacturer: "Somfy", Model: "Somfy RS100 io"},
		{Name: "porch-light", ID: "1000ddeeff", Driver: "plugin/ewelink-lan", Endpoint: 5, Declared: true,
			Capabilities: []string{"on"}},
	}}, deviceMarks{
		hidden:   map[string]bool{"study-blind": true},
		problems: map[string]string{"porch-light": "no LAN key: oikade secret set 1000ddeeff"},
	}
}

func TestDevicesFitTheTerminal(t *testing.T) {
	for _, width := range []int{80, 120} {
		for name, printer := range printers {
			list, marks := aHome()
			var b bytes.Buffer
			_, dropped := printDevices(printer(&b, width), list, marks, false)
			_, _ = fmt.Fprintf(&b, "dropped: %v\n", dropped)
			testkit.Golden(t, fmt.Sprintf("devices-%s-%d", name, width), b.Bytes())
			// At 80, a device's problem, which never shortens as it names the fix, can push past it.
			if width < 120 {
				continue
			}
			for i, line := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
				if n := len([]rune(stripStyles(line))); n > width {
					t.Errorf("%s at %d: line %d is %d wide: %q", name, width, i, n, line)
				}
			}
		}
	}
	list, marks := aHome()
	var b bytes.Buffer
	if _, dropped := printDevices(cli.Plain(&b, 80), list, marks, true); len(dropped) != 0 ||
		!strings.Contains(b.String(), "hardware/toshiba/5f1c0c2e-77b3-4f8e-9d4a-1b2c3d4e5f60") {
		t.Errorf("--wide dropped %v:\n%s", dropped, b.String())
	}
	for _, want := range []string{"SONOFF MINIR4", "Toshiba ", " Somfy RS100 io ", "1000aabbcc  3.6.0"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("--wide has no %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "Somfy Somfy") {
		t.Errorf("a model that starts with its manufacturer names it twice:\n%s", b.String())
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

// oikade devices | cat writes no escape codes, as stdout isn't a terminal.
func TestDevicesToAPipeAreUnstyled(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	list, marks := aHome()
	p := cli.NewPrinter(w)
	printDevices(p, list, marks, false)
	p.Hint("--wide shows HARDWARE")
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.Styled || bytes.Contains(out, []byte("\x1b")) || !bytes.Contains(out, []byte("hall-lamp")) {
		t.Errorf("styled %v:\n%q", p.Styled, out)
	}
}

func TestClientsAndSecretsInTheLook(t *testing.T) {
	now := time.Date(2026, 11, 18, 14, 30, 0, 0, time.Local)
	clients := []api.Client{
		{Name: "laptop", Created: now.AddDate(0, 0, -8), LastUsed: now.Add(-8 * time.Second)},
		{Name: "ci", Created: time.Date(2026, 11, 18, 8, 45, 53, 0, time.Local)},
	}
	secrets := []api.Secret{{Name: "ewelink-1000aabbcc"}, {Name: "toshiba-password"}}
	for name, printer := range printers {
		var b bytes.Buffer
		if err := printClients(printer(&b, 80), cli.Plain(io.Discard, 80), clients, false, now); err != nil {
			t.Fatal(err)
		}
		if err := printSecrets(printer(&b, 80), cli.Plain(io.Discard, 80), secrets, false); err != nil {
			t.Fatal(err)
		}
		testkit.Golden(t, "clients-secrets-"+name, b.Bytes())
	}
}

// aStatus is a home's status, with each kind of device a line can show.
func aStatus(now time.Time) *api.Status {
	since := now.Add(-8 * time.Minute)
	until := now.Add(4 * time.Minute)
	gone := now.Add(-95 * time.Minute)
	on, off := true, false
	return &api.Status{
		Home: api.Home{Version: "f113018fb81c06e4d5fa3500102bfbc4", Process: true, Since: &since},
		Devices: []api.Device{
			{Name: "hall-lamp", Room: "hall", Online: &on, Capabilities: []api.Capability{{Name: "on", Value: false}}},
			{
				Name: "office-ac", Online: &on, Declare: `home.Device("office-ac").Hardware("toshiba", "5f1c0c2e")`,
				Capabilities: []api.Capability{
					{Name: "mode", Value: "cool"},
					{
						Name: "on", Value: true, Level: "Normal", Wanted: true, Case: "too-warm",
						Claims: []api.Claim{{
							Source: "case/office-ac/on/too-warm", Case: "too-warm", Level: "Normal", Value: true, Winner: true,
						}},
						Limit: "MinimumOn", LimitUntil: &until,
					},
					{Name: "setpoint", Value: 21.5},
					{Name: "temperature", Value: 24.25},
				},
			},
			{
				Name: "porch-light", Problem: "no LAN key: oikade secret set 1000ddeeff", Plugin: "ewelink-lan",
				Capabilities: []api.Capability{{Name: "on"}},
			},
			{
				Name: "study-blind", Room: "study", Online: &off, OfflineSince: &gone, Plugin: "overkiz",
				Capabilities: []api.Capability{
					{Name: "position", Value: 40.0}, {Name: "target-position", Value: 40.0}, {Name: "moving", Value: false},
				},
			},
		},
		Rules: []api.Rule{
			{
				Name: "office-ac on too-warm", Ref: "case/office-ac/on/too-warm", Kind: "case", Device: "office-ac",
				Capability: "on", Case: "too-warm", Active: &on, Since: since,
			},
			{Name: "morning", Ref: "flow/morning", Kind: "flow"},
		},
		Plugins: []api.Plugin{
			{Name: "ewelink-lan", Ref: "plugin/ewelink-lan", Devices: []string{"porch-light"}},
			{
				Name: "overkiz", Ref: "plugin/overkiz", Problem: "login refused: check secret/overkiz",
				Devices: []string{"study-blind"},
			},
		},
		Matter: &api.Matter{Serving: true, Port: 5540, Fabrics: []api.Fabric{{Index: 1, Label: "Home"}}},
	}
}

// officeACEvents are office-ac's last events, as the host's log has them.
func officeACEvents(now time.Time) []api.Event {
	at := func(ago time.Duration) time.Time { return now.Add(-ago) }
	return []api.Event{
		{Type: api.EventAvailability, Ref: "device/office-ac", Time: at(26 * time.Hour), Message: api.AvailabilityOnline},
		{
			Type: api.EventWrite, Ref: "device/office-ac/on", Time: at(8 * time.Minute), Capability: "on", Value: true,
			By: "case/office-ac/on/too-warm",
		},
		{
			Type: api.EventChange, Ref: "device/office-ac/on", Time: at(8*time.Minute - time.Second), Capability: "on",
			Value: true, Previous: false,
		},
		{
			Type: api.EventChange, Ref: "device/office-ac/temperature", Time: at(time.Minute), Capability: "temperature",
			Value: 24.25,
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
			out.WriteString("--- stderr\n")
			out.Write(stderr.Bytes())
			testkit.Golden(t, fmt.Sprintf("status-%s-%d", name, width), out.Bytes())
			for i, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
				if n := len([]rune(stripStyles(line))); n > width {
					t.Errorf("%s at %d: line %d is %d wide: %q", name, width, i, n, line)
				}
			}
		}
	}
}

// Each kind of ref status takes has its own view, in the order they're given: a device in full, with
// its last events, a capability the same, a room's devices, a case and the capability it decides, a
// flow, a plugin and its devices, and the Matter bridge. A device's view is in full, so its WHY and
// the bridge's sentences may run past the terminal's width, but the tables fit it.
func TestStatusOfEachKindFitsTheTerminal(t *testing.T) {
	now := time.Date(2026, 11, 18, 14, 0, 0, 0, time.Local)
	refs, err := statusRefs([]string{
		"office-ac", "device/study-blind/position", "room/hall", "case/office-ac/on/too-warm", "flow/morning",
		"plugin/overkiz", "matter",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := func(ref string) ([]api.Event, error) {
		if ref == "device/office-ac" {
			return officeACEvents(now), nil
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
		{Type: api.EventChange, Ref: "device/desk-plug/on", Capability: "on", Value: true, Previous: false},
		{Type: api.EventChange, Ref: "device/office-ac/setpoint", Capability: "setpoint", Value: 21.5},
		{Type: api.EventWrite, Ref: "device/desk-plug/on", Capability: "on", Value: false},
		{Type: api.EventWrite, Ref: "device/desk-plug/on", Capability: "on", Value: true, By: "case/desk-plug/on/occupied"},
		{Type: api.EventWrite, Ref: "device/office-ac/setpoint", Capability: "setpoint", Value: 20.0, By: "matter"},
		{Type: api.EventWrite, Ref: "device/hall-blind/position", Capability: "position", Value: 40.0, By: "command"},
		{
			Type: api.EventRefused, Ref: "device/desk-plug/on", Capability: "on", Value: true,
			Message: "case/desk-plug/on/hall-too-humid holds it at Safety",
		},
		{Type: api.EventRule, Ref: "flow/morning", Result: "succeeded"},
		{Type: api.EventRule, Ref: "case/towel-rail/on/after-shower", Result: "condition_error"},
		{Type: api.EventPause, Ref: "flow/morning", Message: "paused"},
		{Type: api.EventDeploy, Message: "f113018fb81c06e4d5fa3500102bfbc4"},
		{Type: api.EventProcess, Message: "connected"},
		{Type: api.EventAvailability, Ref: "device/porch-light", Message: api.AvailabilityOffline},
		{Type: api.EventAvailability, Ref: "device/garage-relay", Message: api.AvailabilityOffline, Result: "no LAN key"},
		{Type: api.EventAvailability, Ref: "device/porch-light", Message: api.AvailabilityOnline},
		{Type: api.EventPlugin, Ref: "plugin/overkiz", Message: "the TaHoma box refused the login"},
		{Type: api.EventPlugin, Ref: "plugin/overkiz"},
		{Type: api.EventMatter, Ref: "matter", Message: api.MatterWindowOpened},
		{Type: api.EventMatter, Ref: "matter", Message: api.MatterControllerPaired, Value: 2.0},
		{Type: api.EventMatter, Ref: "matter", Message: api.MatterWindowClosed},
		{Type: api.EventMatter, Ref: "matter", Message: api.MatterControllerLeft, Value: 1.0},
		{Type: api.EventForget, Ref: "device/shed-lamp"},
		{Type: api.EventRename, Ref: "device/mains-valve", Previous: "valve"},
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
		if err := report(p, api.Response{Version: "f113018fb81c06e4d5fa3500102bfbc4"}, "Deployed version"); err != nil {
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
		if err := finished(api.Response{}, false, "Paused flow/morning", tc.changed); err != nil || b.String() != tc.want {
			t.Errorf("changed %v: %q, %v", tc.changed, b.String(), err)
		}
	}
	if stuck("on", nil, true) || !stuck("brightness", 33.1, 33.0) || stuck("brightness", 30.0, 33.0) {
		t.Error("stuck")
	}
}

func TestAModelNamesItsManufacturerOnce(t *testing.T) {
	for _, c := range []struct{ manufacturer, model, want string }{
		{"Somfy", "Somfy RS100 io", "Somfy RS100 io"},
		{"somfy", "Somfy", "Somfy"},
		{"Aqara", "AqaraHub-M2", "Aqara AqaraHub-M2"},
		{"Toshiba", "", "Toshiba"},
		{"", "MINIR4", "MINIR4"},
		{"Bosch", "Bo", "Bosch Bo"},
	} {
		if got := modelOf(registry.Device{Manufacturer: c.manufacturer, Model: c.model}); got != c.want {
			t.Errorf("%q and %q gave %q, want %q", c.manufacturer, c.model, got, c.want)
		}
	}
}
