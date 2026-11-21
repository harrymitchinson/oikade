package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/registry"
)

func TestUnimportedDevicesSayHowToRemoveThemAndCallOutSafety(t *testing.T) {
	var out bytes.Buffer
	printUnimported(&out, []api.Unimported{
		{Device: "awning", Plugin: "shades"},
		{Device: "boiler", Plugin: "relays", Safety: true},
		{Device: "heater", Plugin: "relays"},
	})
	want := `These devices stay on record, offline, because your home doesn't import their plugin:
  relays: boiler, heater
    A Safety claim holds boiler, and forgetting it ends that claim
    Forget them with oikade forget --plugin relays
  shades: awning
    Forget them with oikade forget --plugin shades
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
	out.Reset()
	printUnimported(&out, nil)
	if out.Len() != 0 {
		t.Errorf("got %q", out.String())
	}
}

func TestDevicesMarkThoseWhosePluginIsntImported(t *testing.T) {
	var out bytes.Buffer
	printDevices(cli.Plain(&out, 0), registry.Snapshot{Devices: []registry.Device{
		{Name: "boiler", ID: "b", Driver: "plugin/relays", Endpoint: 2},
		{Name: "lamp", ID: "l", Driver: "plugin/shades", Endpoint: 3},
	}}, deviceMarks{unimported: map[string]bool{"boiler": true}}, false)
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[1], "plugin not imported") || strings.Contains(lines[2], "plugin not imported") {
		t.Errorf("got\n%s", out.String())
	}
}

func TestDevicesMarkThoseHiddenFromMatter(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{Name: "bedroom-shutter", ID: "s1", Driver: "plugin/tahoma", Endpoint: 2},
		{Name: "garage-door", ID: "s2", Driver: "plugin/tahoma", Endpoint: 3},
		{Name: "attic-shutter", ID: "s3", Driver: "plugin/tahoma", Endpoint: 4},
	}}
	marks := deviceMarks{hidden: map[string]bool{"bedroom-shutter": true, "attic-shutter": true}}

	var out bytes.Buffer
	printDevices(cli.Plain(&out, 0), list, marks, false)
	lines := strings.Split(out.String(), "\n")
	// Sorted: attic-shutter, bedroom-shutter, garage-door.
	if !strings.Contains(lines[1], "  hidden from Matter") ||
		!strings.Contains(lines[2], "  hidden from Matter") || strings.Contains(lines[3], "hidden") {
		t.Errorf("got\n%s", out.String())
	}

	raw, err := json.Marshal(listed(list, marks))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"matter":false`) != 2 || strings.Contains(string(raw), `"matter":true`) {
		t.Errorf("only a hidden device says matter, as false: %s", raw)
	}
}

func TestStatusSaysWhatsWrongWithADevice(t *testing.T) {
	var out bytes.Buffer
	st := &api.Status{Devices: []api.Device{
		{
			Name: "porch-light", Problem: "no LAN key: oikade setup ewelink-lan",
			Capabilities: []api.Capability{{Name: "on"}},
		},
		{Name: "shed-relay", Problem: "not seen on the LAN"},
	}}
	refs, err := statusRefs([]string{"porch-light", "shed-relay"})
	if err != nil {
		t.Fatal(err)
	}
	printNamed(cli.Plain(&out, 0), cli.Plain(io.Discard, 0), st, refs, noEvents, false, time.Now())
	for _, want := range []string{
		"porch-light: no LAN key: oikade setup ewelink-lan\n",
		"shed-relay: not seen on the LAN\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
}

// An offline device says so in status, with the driver's problem if it has one.
func TestStatusSaysADeviceIsOffline(t *testing.T) {
	offline, online := false, true
	for _, tc := range []struct {
		d    api.Device
		want string
	}{
		{api.Device{Name: "pool-pump", Online: &offline}, "offline"},
		{api.Device{Name: "shed-relay", Online: &offline, Problem: "not seen on the LAN"}, "offline: not seen on the LAN"},
		{api.Device{Name: "porch-light", Online: &online, Problem: "no key"}, "no key"},
		{api.Device{Name: "desk-lamp", Online: &online}, ""},
		{api.Device{Name: "attic-fan", Online: &offline, PluginNotImported: true}, notImported},
	} {
		if got := deviceState(tc.d); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.d.Name, got, tc.want)
		}
	}
}

func TestDevicesNotInYourCodeSayHowToDeclareThem(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{Name: "attic-fan", ID: "0x1", Driver: "plugin/zigbee", Endpoint: 2},
		{Name: "porch-light", ID: "0x2", Driver: "plugin/zigbee", Endpoint: 3, Declared: true},
		{Name: "kettle", ID: "k", Driver: "simulated/simulator", Endpoint: 4},
	}}
	var out bytes.Buffer
	printDevices(cli.Plain(&out, 0), list, deviceMarks{}, false)
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[1], "attic-fan") || !strings.Contains(lines[1], notInYourCode) ||
		strings.Contains(lines[2], notInYourCode) || strings.Contains(lines[3], notInYourCode) {
		t.Errorf("got\n%s", out.String())
	}
	out.Reset()
	printUndeclared(&out, []string{`home.Device("attic-fan").Hardware("zigbee", "0x1")`})
	if want := "To name, place or type a device that's not in your code, declare it in your home:\n" +
		`  home.Device("attic-fan").Hardware("zigbee", "0x1")` + "\n"; out.String() != want {
		t.Errorf("got\n%s", out.String())
	}
	raw, err := json.Marshal(listed(list, deviceMarks{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"name":"attic-fan"`) || strings.Count(string(raw), `"declared":false`) != 2 ||
		strings.Count(string(raw), `"declared":true`) != 1 {
		t.Errorf("got %s", raw)
	}
}

func TestTypesNamesDevicesAsYourHomeDeclaresThem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := registry.Snapshot{Next: 3, Devices: []registry.Device{
		{Name: "lamp-2", AppName: "lamp-2", ID: "0x2", Driver: "plugin/zigbee", Endpoint: 2, Capabilities: []string{"on"}},
	}}
	declares := func(context.Context) (wire.Manifest, error) {
		return wire.Manifest{Inventory: []wire.DeviceDeclaration{{Name: "porch-light", Plugin: "zigbee", ID: "0x2"}}}, nil
	}
	var stderr bytes.Buffer
	got := declaredIn(t.Context(), &stderr, dir, list, declares)
	if len(got.Devices) != 1 || got.Devices[0].Name != "porch-light" || list.Devices[0].Name != "lamp-2" {
		t.Errorf("got %+v, from %+v", got.Devices, list.Devices)
	}
	// Code that already names the new handle doesn't build until oikade types writes it, so the
	// declarations come from the source.
	broken := func(context.Context) (wire.Manifest, error) {
		return wire.Manifest{}, errors.New("undefined: PorchLight")
	}
	code := "package main\n\nfunc main() {\n\thome.Device(\"porch-light\").\n" +
		"\t\tHardware(\"zigbee\", \"0x2\").Room(\"porch\")\n\t_ = PorchLight.On\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	got = declaredIn(t.Context(), &stderr, dir, list, broken)
	if got.Devices[0].Name != "porch-light" || got.Devices[0].Room != "porch" ||
		!strings.Contains(stderr.String(), "its 1 device declarations are read from its source") {
		t.Errorf("got %+v, and said %q", got.Devices, stderr.String())
	}
	// Outside a module there's nothing to build, and it says so.
	stderr.Reset()
	outside := declaredIn(t.Context(), &stderr, t.TempDir(), list, broken)
	if outside.Devices[0].Name != "lamp-2" || !strings.Contains(stderr.String(), "--module") {
		t.Errorf("got %+v, and said %q", outside.Devices, stderr.String())
	}
}

func TestDeclarationsAreReadFromTheSource(t *testing.T) {
	dir := t.TempDir()
	code := `package main

import "oikade.dev"

func declare(home *oikade.Home, name string) {
	home.Device("study-blind").Hardware("overkiz", "io://1").Room("study").Is(oikade.WindowCovering)
	home.Device("shed-fan")
	home.Device(name).Room("hall") // not a literal, so not read
}
`
	if err := os.WriteFile(filepath.Join(dir, "inventory.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := declarationsInSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.DeviceDeclaration{
		{Name: "study-blind", Plugin: "overkiz", ID: "io://1", Room: "study", DeviceType: "WindowCovering"},
		{Name: "shed-fan"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestStatusMarksADeviceNotInYourCode(t *testing.T) {
	var out, stderr bytes.Buffer
	printStatus(cli.Plain(&out, 0), cli.Plain(&stderr, 0), &api.Status{Devices: []api.Device{
		{
			Name: "attic-fan", Declare: `home.Device("attic-fan").Hardware("zigbee", "0x1")`,
			Capabilities: []api.Capability{{Name: "on", Value: true}, {Name: "power"}},
		},
		{Name: "new-plug", Declare: `home.Device("new-plug").Hardware("zigbee", "0x2")`},
	}}, false, time.Now())
	if strings.Count(out.String(), notInYourCode) != 2 ||
		!strings.Contains(out.String(), "attic-fan        on true  "+notInYourCode) {
		t.Errorf("want each device marked once:\n%s", out.String())
	}
	// How to declare them is oikade list devices' to say.
	const once = "\n2 devices aren't in your code: oikade list devices shows how to declare them\n"
	if !strings.HasSuffix(stderr.String(), once) ||
		strings.Contains(stderr.String(), "home.Device") {
		t.Errorf("got:\n%s", stderr.String())
	}
}

func TestDevicesShowTheirHardwareByItsRef(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{Name: "study-blind", ID: "io://0812-4471-2093/5", Driver: "plugin/overkiz", Endpoint: 2},
		{Name: "kettle", ID: "k", Driver: "simulated/simulator", Endpoint: 3},
	}}
	const ref = "hardware/overkiz/io:%2F%2F0812-4471-2093%2F5"
	var out bytes.Buffer
	printDevices(cli.Plain(&out, 0), list, deviceMarks{}, false)
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[0], "HARDWARE") || !strings.Contains(lines[2], ref) ||
		!strings.Contains(lines[1], "simulated/simulator's k") {
		t.Errorf("got\n%s", out.String())
	}

	raw, err := json.Marshal(listed(list, deviceMarks{}))
	if err != nil {
		t.Fatal(err)
	}
	var listing []struct {
		Name     string `json:"name"`
		Hardware string `json:"hardware"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatal(err)
	}
	hardware := map[string]string{}
	for _, d := range listing {
		hardware[d.Name] = d.Hardware
	}
	if hardware["study-blind"] != ref || hardware["kettle"] != "" {
		t.Errorf("got %s", raw)
	}

	// Pasted back, it's the plugin and the ID the plugin gave.
	k, parts, err := ident.ParseRef(hardware["study-blind"])
	if err != nil || k != ident.Hardware || !slices.Equal(parts, []string{"overkiz", "io://0812-4471-2093/5"}) {
		t.Errorf("ParseRef(%s) = %s %q %v", ref, k, parts, err)
	}
}
