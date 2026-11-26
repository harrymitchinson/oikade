package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		{AppName: "Boiler", ID: "b", Driver: "plugin/relays", Endpoint: 2},
		{AppName: "Lamp", ID: "l", Driver: "plugin/shades", Endpoint: 3},
	}}, deviceMarks{unimported: map[string]bool{"relays/b": true}}, false)
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[1], "plugin not imported") || strings.Contains(lines[2], "plugin not imported") {
		t.Errorf("got\n%s", out.String())
	}
}

func TestDevicesMarkThoseHiddenFromMatter(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{ID: "s1", Driver: "plugin/tahoma", Endpoint: 2},
		{ID: "s2", Driver: "plugin/tahoma", Endpoint: 3},
		{ID: "s3", Driver: "plugin/tahoma", Endpoint: 4},
	}}
	marks := deviceMarks{hidden: map[string]bool{"tahoma/s1": true, "tahoma/s3": true}}

	var out bytes.Buffer
	printDevices(cli.Plain(&out, 0), list, marks, false)
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[1], "  hidden from Matter") ||
		strings.Contains(lines[2], "hidden") || !strings.Contains(lines[3], "  hidden from Matter") {
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
			Hardware: "ewelink-lan/1000ddeeff", Ref: "hardware/ewelink-lan/1000ddeeff",
			Problem: "no LAN key: oikade setup ewelink-lan", Capabilities: []api.Capability{{Name: "on"}},
		},
		{Hardware: "ewelink-lan/1000aabbcc", Ref: "hardware/ewelink-lan/1000aabbcc", Problem: "not seen on the LAN"},
	}}
	refs, err := statusRefs([]string{"hardware/ewelink-lan/1000ddeeff", "hardware/ewelink-lan/1000aabbcc"})
	if err != nil {
		t.Fatal(err)
	}
	printNamed(cli.Plain(&out, 0), cli.Plain(io.Discard, 0), st, refs, noEvents, false, time.Now())
	for _, want := range []string{
		"hardware/ewelink-lan/1000ddeeff: no LAN key: oikade setup ewelink-lan\n",
		"hardware/ewelink-lan/1000aabbcc: not seen on the LAN\n",
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
		{api.Device{Hardware: "a/1", Online: &offline}, "offline"},
		{api.Device{Hardware: "a/2", Online: &offline, Problem: "not seen on the LAN"}, "offline: not seen on the LAN"},
		{api.Device{Hardware: "a/3", Online: &online, Problem: "no key"}, "no key"},
		{api.Device{Hardware: "a/4", Online: &online}, ""},
		{api.Device{Hardware: "a/5", Online: &offline, PluginNotImported: true}, notImported},
	} {
		if got := deviceState(tc.d); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.d.Hardware, got, tc.want)
		}
	}
}

func TestDevicesNotInYourCodeAreMarked(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{ID: "0x1", Driver: "plugin/zigbee", Endpoint: 2},
		{ID: "0x2", Driver: "plugin/zigbee", Endpoint: 3},
		{ID: "k", Driver: "simulated/simulator", Endpoint: 4},
	}}
	var out bytes.Buffer
	marks := deviceMarks{declared: map[string]bool{"zigbee/0x2": true}}
	undeclared, _ := printDevices(cli.Plain(&out, 0), list, marks, false)
	lines := strings.Split(out.String(), "\n")
	// By ref: the simulator's kettle, then the attic fan and the porch light.
	if !strings.Contains(lines[1], notInYourCode) || !strings.Contains(lines[2], notInYourCode) ||
		strings.Contains(lines[3], notInYourCode) {
		t.Errorf("got\n%s", out.String())
	}
	if !slices.Equal(undeclared, []string{"hardware/simulator/k", "hardware/zigbee/0x1"}) {
		t.Errorf("got %q", undeclared)
	}
}

// oikade types leaves out the devices your code declares, read from its build, or from its source
// when it doesn't build.
func TestTypesLeavesOutTheDevicesYourCodeDeclares(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := registry.Snapshot{Next: 4, Devices: []registry.Device{
		{AppName: "Lampe", ID: "0x2", Driver: "plugin/zigbee", Endpoint: 2, Capabilities: []string{"on"}},
		{AppName: "Prise", ID: "0x3", Driver: "plugin/zigbee", Endpoint: 3, Capabilities: []string{"on"}},
	}}
	declares := func(context.Context) (wire.Manifest, error) {
		return wire.Manifest{Inventory: []wire.DeviceDeclaration{{Hardware: "zigbee/0x2", Room: "porch"}}}, nil
	}
	var stderr bytes.Buffer
	got := undeclaredIn(t.Context(), &stderr, dir, list, declares)
	if len(got.Devices) != 1 || got.Devices[0].Key() != "zigbee/0x3" || len(list.Devices) != 2 {
		t.Errorf("got %+v, from %+v", got.Devices, list.Devices)
	}
	broken := func(context.Context) (wire.Manifest, error) {
		return wire.Manifest{}, errors.New("undefined: Prise")
	}
	code := "package main\n\nvar Lampe = oikade.Device[oikade.OnOffLight](\"zigbee/0x2\")\n\n" +
		"func main() { _ = Prise.On }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	got = undeclaredIn(t.Context(), &stderr, dir, list, broken)
	if len(got.Devices) != 1 || got.Devices[0].Key() != "zigbee/0x3" ||
		!strings.Contains(stderr.String(), "its 1 devices are read from its source") {
		t.Errorf("got %+v, and said %q", got.Devices, stderr.String())
	}
	// Outside a module there's nothing to build, and it says so.
	stderr.Reset()
	if outside := undeclaredIn(t.Context(), &stderr, t.TempDir(), list, broken); len(outside.Devices) != 2 ||
		!strings.Contains(stderr.String(), "--module") {
		t.Errorf("got %+v, and said %q", outside.Devices, stderr.String())
	}
}

func TestDeclarationsAreReadFromTheSource(t *testing.T) {
	dir := t.TempDir()
	code := `package main

import "oikade.dev"

func lamp(hardware string) oikade.OnOffLight {
	return oikade.Device[oikade.OnOffLight](hardware) // not a literal, so not read
}

var (
	Salon   = oikade.Room("salon")
	SalonTV = oikade.Device[oikade.OnOffLight]("hue/0x2", Salon)
	Capteur = oikade.Device[struct {
		oikade.OccupancySensor
		Temperature oikade.Number
	}]("hue/0x3")
)
`
	if err := os.WriteFile(filepath.Join(dir, "inventory.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := declarationsInSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.DeviceDeclaration{{Hardware: "hue/0x2"}, {Hardware: "hue/0x3"}}
	g, _ := json.Marshal(got)
	if w, _ := json.Marshal(want); string(g) != string(w) {
		t.Errorf("got %s, want %s", g, w)
	}
}

func TestStatusMarksADeviceNotInYourCode(t *testing.T) {
	var out, stderr bytes.Buffer
	printStatus(cli.Plain(&out, 0), cli.Plain(&stderr, 0), &api.Status{Devices: []api.Device{
		{
			Hardware: "zigbee/0x1", Ref: "hardware/zigbee/0x1", Undeclared: true,
			Capabilities: []api.Capability{{Name: "on", Value: true}, {Name: "power"}},
		},
		{Hardware: "zigbee/0x2", Ref: "hardware/zigbee/0x2", Undeclared: true},
	}}, false, time.Now())
	if strings.Count(out.String(), notInYourCode) != 2 ||
		!strings.Contains(out.String(), "hardware/zigbee/0x1        on true  "+notInYourCode) {
		t.Errorf("want each device marked once:\n%s", out.String())
	}
	if once := "\n2 devices aren't in your code: oikade types adds them\n"; !strings.HasSuffix(stderr.String(), once) {
		t.Errorf("got:\n%s", stderr.String())
	}
}

func TestDevicesShowTheirHardwareByItsRef(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{ID: "io://0812-4471-2093/5", Driver: "plugin/overkiz", Endpoint: 2},
		{ID: "k", Driver: "simulated/simulator", Endpoint: 3},
	}}
	const ref = "hardware/overkiz/io:%2F%2F0812-4471-2093%2F5"
	var out bytes.Buffer
	printDevices(cli.Plain(&out, 0), list, deviceMarks{}, false)
	lines := strings.Split(out.String(), "\n")
	if !strings.HasPrefix(lines[0], "DEVICE ") || !strings.HasPrefix(lines[1], ref+" ") ||
		!strings.HasPrefix(lines[2], "hardware/simulator/k ") {
		t.Errorf("got\n%s", out.String())
	}

	raw, err := json.Marshal(listed(list, deviceMarks{}))
	if err != nil {
		t.Fatal(err)
	}
	var listing []struct {
		Ref      string `json:"ref"`
		Hardware string `json:"hardware"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatal(err)
	}
	want := `[{hardware/overkiz/io:%2F%2F0812-4471-2093%2F5 overkiz/io:%2F%2F0812-4471-2093%2F5} ` +
		`{hardware/simulator/k simulator/k}]`
	if fmt.Sprint(listing) != want {
		t.Errorf("got %s", raw)
	}

	// Pasted back, it's the plugin and the ID the plugin gave.
	k, parts, err := ident.ParseRef(listing[0].Ref)
	if err != nil || k != ident.Hardware || !slices.Equal(parts, []string{"overkiz", "io://0812-4471-2093/5"}) {
		t.Errorf("ParseRef(%s) = %s %q %v", ref, k, parts, err)
	}
}
