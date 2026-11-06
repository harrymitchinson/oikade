package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/registry"
)

func (r *rig) devices() registry.Snapshot {
	r.t.Helper()
	var list registry.Snapshot
	out := r.stdout(nil, "devices", "--json", "--all")
	if err := json.Unmarshal(out, &list); err != nil {
		r.t.Fatalf("reading devices: %v: %s", err, out)
	}
	return list
}

func deviceOf(t *testing.T, list registry.Snapshot, name string) registry.Device {
	t.Helper()
	for _, d := range list.Devices {
		if d.Name == name && !d.Removed {
			return d
		}
	}
	t.Fatalf("no %s in %+v", name, list)
	return registry.Device{}
}

// pair simulates pairing hardware, and waits until the host has it.
func (r *rig) pair(id, app string) {
	r.t.Helper()
	raw, err := json.Marshal(map[string]any{"pair": map[string]any{
		"name": id, "capabilities": []string{"switch.on"}, "values": map[string]any{"switch.on": false},
	}})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.stdin.Write(append(raw, '\n')); err != nil {
		r.t.Fatal(err)
	}
	r.waitFor(func(l line) bool { return l.is("Changed capability", "device", app) })
}

func TestAPairedDeviceIsUsableAtOnce(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.pair("hall-lamp", "hall-lamp")

	d := deviceOf(t, r.devices(), "hall-lamp")
	if d.ID != "hall-lamp" || d.Endpoint < registry.FirstEndpoint {
		t.Errorf("got %+v", d)
	}
	r.stdout(nil, "set", "hall-lamp", "switch.on", "on")
	r.wrote("hall-lamp", true)
	if !strings.Contains(string(r.stdout(nil, "devices")), "hall-lamp") {
		t.Error("the table left it out")
	}
}

func TestRenamingKeepsADeviceWorkingAndSurvivesARestart(t *testing.T) {
	t.Parallel()
	r := prepare(t)
	r.launch()
	r.waitFor(func(l line) bool { return l.is(attached) })
	endpoint := deviceOf(t, r.devices(), "fan").Endpoint

	r.stdout(nil, "devices", "name", "fan", "ceiling-fan")
	r.stdout(nil, "devices", "room", "ceiling-fan", "Bathroom")
	r.stdout(nil, "set", "ceiling-fan", "switch.on", "on")
	r.wrote("ceiling-fan", true)
	if c := onOf(t, r.status("ceiling-fan"), "ceiling-fan"); c.Value != true {
		t.Errorf("got %+v", c)
	}
	if _, err := r.oikade(nil, "status", "fan"); err == nil {
		t.Error("the old name stayed")
	}

	r.stop()
	r.launch()
	r.waitFor(func(l line) bool { return l.is(attached) })
	d := deviceOf(t, r.devices(), "ceiling-fan")
	if d.Endpoint != endpoint || d.Room != "Bathroom" {
		t.Errorf("after a restart: %+v, want endpoint %d", d, endpoint)
	}
	if st := r.status("ceiling-fan"); st.Devices[0].Room != "Bathroom" {
		t.Errorf("status: %+v", st.Devices[0])
	}
}

func TestReplacingHardwareKeepsTheNameEndpointAndClaims(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("leak-sensor", "sensor.contact-open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	r.sync()
	before := deviceOf(t, r.devices(), "valve")

	r.pair("new-valve", "new-valve")
	if stderr, err := r.oikade(nil, "devices", "name", "valve", "main-valve"); err == nil ||
		!strings.Contains(stderr, "Rule/leak-shutoff") {
		t.Errorf("renamed a device under a claim: %v: %s", err, stderr)
	}
	if stderr, err := r.oikade(nil, "devices", "remove", "valve"); err == nil ||
		!strings.Contains(stderr, "--allow-safety") {
		t.Errorf("removed a device under a Safety claim: %v: %s", err, stderr)
	}
	r.stdout(nil, "devices", "replace", "valve", "new-valve")
	after := deviceOf(t, r.devices(), "valve")
	if after.ID != "new-valve" || after.Endpoint != before.Endpoint {
		t.Errorf("got %+v, was %+v", after, before)
	}
	if c := onOf(t, r.status("valve"), "valve"); c.Level != "Safety" || winner(c) != "Rule/leak-shutoff" {
		t.Errorf("the claim didn't stay on the name: %+v", c)
	}
	if _, err := r.oikade(nil, "status", "new-valve"); err == nil {
		t.Error("the replacement's name stayed")
	}
}

func TestAReservedNameTakesItsHardwareLater(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.stdout(nil, "devices", "reserve", "porch-light", "--room", "Porch")
	if d := deviceOf(t, r.devices(), "porch-light"); !d.Reserved() || d.Room != "Porch" {
		t.Errorf("got %+v", d)
	}
	if _, err := r.oikade(nil, "set", "porch-light", "switch.on", "on"); err == nil {
		t.Error("set a device with no hardware")
	}
	endpoint := deviceOf(t, r.devices(), "porch-light").Endpoint
	r.pair("0xabc", "device-0xabc")
	r.stdout(nil, "devices", "replace", "porch-light", "device-0xabc")
	d := deviceOf(t, r.devices(), "porch-light")
	if d.Reserved() || d.ID != "0xabc" || d.Endpoint != endpoint || d.Room != "Porch" {
		t.Errorf("got %+v, want endpoint %d", d, endpoint)
	}
	r.stdout(nil, "set", "porch-light", "switch.on", "on")
	r.wrote("porch-light", true)
}

func TestARemovedDeviceComesBackOnItsOwnEndpoint(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.pair("shed-lamp", "shed-lamp")
	endpoint := deviceOf(t, r.devices(), "shed-lamp").Endpoint
	r.stdout(nil, "devices", "remove", "shed-lamp")
	if _, err := r.oikade(nil, "set", "shed-lamp", "switch.on", "on"); err == nil {
		t.Error("set a removed device")
	}
	if strings.Contains(string(r.stdout(nil, "devices")), "shed-lamp") {
		t.Error("the table still lists it")
	}
	r.pair("other-lamp", "other-lamp")
	if got := deviceOf(t, r.devices(), "other-lamp").Endpoint; got == endpoint {
		t.Errorf("another device took endpoint %d", got)
	}
	r.pair("shed-lamp", "shed-lamp")
	if got := deviceOf(t, r.devices(), "shed-lamp").Endpoint; got != endpoint {
		t.Errorf("came back on %d, want %d", got, endpoint)
	}
}

// goBuild builds a module of main.go and the generated devices.go, with nothing else to fetch.
func goBuild(t *testing.T, dir, code string) error {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module home\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	return run(dir, nil, "build", "-o", filepath.Join(dir, "home"), ".")
}

func TestTypesAreDeterministicAndARemovedDeviceStopsCompiling(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.stdout(nil, "devices", "reserve", "porch-light")
	dir := t.TempDir()
	out := filepath.Join(dir, "devices.go")
	r.stdout(nil, "types", "--out", out)
	first, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if again := r.stdout(nil, "types", "--out", "-"); string(again) != string(first) {
		t.Errorf("two runs differ:\n%s\n%s", first, again)
	}
	const code = "package main\n\nfunc main() { println(Fan, PorchLight) }\n"
	if err := goBuild(t, dir, code); err != nil {
		t.Fatalf("code naming its devices didn't compile: %v\n%s", err, first)
	}

	r.stdout(nil, "devices", "remove", "fan")
	r.stdout(nil, "types", "--out", out)
	if err := goBuild(t, dir, code); err == nil {
		t.Error("code naming a removed device compiled")
	}
}

func TestRestoringFromTheCommittedFile(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.pair("hall-lamp", "hall-lamp")
	r.stdout(nil, "devices", "name", "hall-lamp", "lamp")
	r.stdout(nil, "devices", "room", "lamp", "Hall")
	r.stdout(nil, "devices", "reserve", "porch-light", "--room", "Porch")
	want := r.devices()
	file := filepath.Join(t.TempDir(), "devices.go")
	r.stdout(nil, "types", "--out", file)
	r.stop()

	lost := start(t)
	lost.stdout(nil, "devices", "restore", file)
	got := lost.devices()
	if got.Next != want.Next {
		t.Errorf("next is %d, want %d", got.Next, want.Next)
	}
	for _, name := range []string{"fan", "lamp", "porch-light"} {
		w, g := deviceOf(t, want, name), deviceOf(t, got, name)
		if g.Endpoint != w.Endpoint || g.Room != w.Room || g.ID != w.ID {
			t.Errorf("%s: got %+v, want %+v", name, g, w)
		}
	}
	lost.stdout(nil, "set", "fan", "switch.on", "on")
	lost.wrote("fan", true)
}

func TestASafetyDeviceCanBeRemovedOnPurpose(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("leak-sensor", "sensor.contact-open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	r.sync()
	r.stdout(nil, "devices", "remove", "valve", "--allow-safety")
	if _, err := r.oikade(nil, "status", "valve"); err == nil {
		t.Error("the valve stayed")
	}
}
