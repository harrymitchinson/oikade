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
			DeviceType: devicetype.OnOffLight, Capabilities: []string{"on"}},
		{Name: "office-ac", ID: "5f1c0c2e-77b3-4f8e-9d4a-1b2c3d4e5f60", Driver: "plugin/toshiba", Endpoint: 3,
			Capabilities: []string{"mode", "on", "setpoint", "temperature"}},
		{Name: "study-blind", ID: "io://0812-4471-2093/5", Driver: "plugin/overkiz", Endpoint: 4, Room: "study",
			Declared: true, Capabilities: []string{"position", "target-position", "moving"}},
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
	on, off := true, false
	return &api.Status{
		Version: "f113018fb81c06e4d5fa3500102bfbc4", Process: true, Since: &since,
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
				Name: "porch-light", Problem: "no LAN key: oikade secret set 1000ddeeff",
				Capabilities: []api.Capability{{Name: "on"}},
			},
			{
				Name: "study-blind", Room: "study", Online: &off,
				Capabilities: []api.Capability{
					{Name: "position", Value: 40.0}, {Name: "target-position", Value: 40.0}, {Name: "moving", Value: false},
				},
			},
		},
		Rules: []api.Rule{
			{Name: "office-ac on too-warm", Ref: "case/office-ac/on/too-warm", Kind: "case", Active: &on, Since: since},
			{Name: "morning", Ref: "flow/morning", Kind: "flow"},
		},
	}
}

func TestStatusFitsTheTerminal(t *testing.T) {
	now := time.Date(2026, 11, 18, 14, 0, 0, 0, time.Local)
	paired := &api.Matter{Serving: true, Fabrics: []api.Fabric{{Index: 1}}}
	for _, width := range []int{80, 120} {
		for name, printer := range printers {
			var out, stderr bytes.Buffer
			printStatus(printer(&out, width), printer(&stderr, width), aStatus(now), paired, true, false, now)
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
	// One device is shown in full, a capability a line with why it has its value.
	for name, printer := range printers {
		st := aStatus(now)
		st.Devices = st.Devices[1:2]
		var out, stderr bytes.Buffer
		printStatus(printer(&out, 80), printer(&stderr, 80), st, nil, false, false, now)
		out.WriteString("--- stderr\n")
		out.Write(stderr.Bytes())
		testkit.Golden(t, "status-device-"+name, out.Bytes())
	}
}
