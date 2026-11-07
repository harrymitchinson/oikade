package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/host/api"
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
		"name": id, "capabilities": []string{"on"}, "values": map[string]any{"on": false},
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
	r.stdout(nil, "set", "hall-lamp", "on", "on")
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

	r.stdout(nil, "devices", "name", "fan", "ceiling-fan", "--allow-safety")
	r.stdout(nil, "devices", "room", "ceiling-fan", "Bathroom")
	r.stdout(nil, "set", "ceiling-fan", "on", "on")
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
	r.change("leak-sensor", "open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	r.sync()
	before := deviceOf(t, r.devices(), "valve")

	r.pair("new-valve", "new-valve")
	if stderr, err := r.oikade(nil, "devices", "name", "valve", "main-valve", "--allow-safety"); err == nil ||
		!strings.Contains(stderr, "Rule/leak-shutoff") {
		t.Errorf("renamed a device under a claim: %v: %s", err, stderr)
	}
	if stderr, err := r.oikade(nil, "devices", "remove", "valve"); err == nil ||
		!strings.Contains(stderr, "--allow-safety") {
		t.Errorf("removed a device under a Safety claim: %v: %s", err, stderr)
	}
	r.stdout(nil, "devices", "replace", "valve", "new-valve", "--allow-safety")
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
	if _, err := r.oikade(nil, "set", "porch-light", "on", "on"); err == nil {
		t.Error("set a device with no hardware")
	}
	endpoint := deviceOf(t, r.devices(), "porch-light").Endpoint
	r.pair("0xabc", "device-0xabc")
	r.stdout(nil, "devices", "replace", "porch-light", "device-0xabc", "--allow-safety")
	d := deviceOf(t, r.devices(), "porch-light")
	if d.Reserved() || d.ID != "0xabc" || d.Endpoint != endpoint || d.Room != "Porch" {
		t.Errorf("got %+v, want endpoint %d", d, endpoint)
	}
	r.stdout(nil, "set", "porch-light", "on", "on")
	r.wrote("porch-light", true)
}

func TestARemovedDeviceComesBackOnItsOwnEndpoint(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.pair("shed-lamp", "shed-lamp")
	endpoint := deviceOf(t, r.devices(), "shed-lamp").Endpoint
	r.stdout(nil, "devices", "remove", "shed-lamp")
	if _, err := r.oikade(nil, "set", "shed-lamp", "on", "on"); err == nil {
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

// goBuild builds a module of main.go and the generated devices.go, on this checkout's framework,
// with nothing else to fetch.
func goBuild(t *testing.T, dir, code string) error {
	t.Helper()
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeModule(repo, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	return run(dir, []string{"GOFLAGS=-mod=mod", "GOPROXY=off"}, "build", "-o", filepath.Join(dir, "home"), ".")
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
	const code = "package main\n\nfunc main() { println(Fan.On.Device, PorchLight.Name) }\n"
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
	r.stdout(nil, "devices", "name", "hall-lamp", "lamp", "--allow-safety")
	r.stdout(nil, "devices", "room", "lamp", "Hall")
	r.stdout(nil, "devices", "type", "lamp", "OnOffLight")
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
		if g.Endpoint != w.Endpoint || g.Room != w.Room || g.ID != w.ID || g.DeviceType != w.DeviceType {
			t.Errorf("%s: got %+v, want %+v", name, g, w)
		}
	}
	if lamp := deviceOf(t, got, "lamp"); lamp.DeviceType != "OnOffLight" {
		t.Errorf("the lamp lost its device type: %+v", lamp)
	}
	lost.stdout(nil, "set", "fan", "on", "on")
	lost.wrote("fan", true)
}

func TestASafetyDeviceCanBeRemovedOnPurpose(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("leak-sensor", "open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	r.sync()
	r.stdout(nil, "devices", "remove", "valve", "--allow-safety")
	if _, err := r.oikade(nil, "status", "valve"); err == nil {
		t.Error("the valve stayed")
	}
}

func TestRestoreRefusesToMoveAClaimedName(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("leak-sensor", "open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	r.sync()
	list := r.devices()
	for n, d := range list.Devices {
		if d.Name == "valve" {
			list.Devices[n].Name = "main-valve"
		}
	}
	file, err := registry.Render(list, "main")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "devices.go")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	if stderr, err := r.oikade(nil, "devices", "restore", path, "--force"); err == nil ||
		!strings.Contains(stderr, "Rule/leak-shutoff") {
		t.Errorf("moved a claimed name: %v: %s", err, stderr)
	}
	deviceOf(t, r.devices(), "valve")
}

func TestRemovalAfterARestartSeesASavedSafetyClaim(t *testing.T) {
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.change("leak-sensor", opened, true)
	r.wrote("valve", false)
	r.sync()
	r.stop()
	if got := r.ruleClaims("valve"); !holdsClaim(got, "Rule/leak-shutoff", 3) {
		t.Fatalf("no Safety claim: %+v", got)
	}
	file, _ := filepath.Abs("testdata/devices-novalve.json")
	r.launchWithout("--home", homeBin, "--simulate", file, settle)
	r.awaitLoaded()
	r.sync()
	r.stop()
	if got := r.ruleClaims("valve"); !holdsClaim(got, "Rule/leak-shutoff", 3) {
		t.Fatalf("restart lost the claim: %+v", got)
	}
	r.launchWithout("--home", homeBin, "--simulate", file, settle)
	r.awaitLoaded()
	r.sync()
	stderr, err := r.oikade(nil, "devices", "remove", "valve")
	if !strings.Contains(stderr, "--allow-safety") {
		t.Errorf("got %q", stderr)
	}
	r.stop()
	s := r.openStore()
	defer func() { _ = s.Close() }()
	_, ok, _ := s.Namespace("controls").Get("home/valve")
	if err == nil {
		t.Fatalf("a Safety claim's device was removed without --allow-safety")
	}
	if !ok {
		t.Error("the Safety claim was deleted")
	}
}

func TestASafetyClaimIsEnforcedBeforeItsDeviceReports(t *testing.T) {
	t.Parallel()
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.change("leak-sensor", opened, true)
	r.wrote("valve", false)
	r.sync()
	r.stop()
	file, err := filepath.Abs("testdata/devices-novalve.json")
	if err != nil {
		t.Fatal(err)
	}
	r.launchWithout("--home", homeBin, "--simulate", file, settle)
	r.awaitLoaded()
	r.sync()

	st := r.status("valve")
	c := api.Capability{}
	if len(st.Devices) == 1 && len(st.Devices[0].Capabilities) > 0 {
		c = st.Devices[0].Capabilities[0]
	}
	if c.Level != "Safety" || winner(c) != "Rule/leak-shutoff" {
		t.Errorf("the arbiter didn't load before the device reported: %+v", st.Devices)
	}
	stderr, err := r.deploy("renamed")
	if err == nil || !strings.Contains(stderr, `"leak-shutoff"`) {
		t.Errorf("the deploy check missed the claim: %v: %s", err, stderr)
	}
}

func TestAFileThatDisagreesNeedsForce(t *testing.T) {
	t.Parallel()
	r := start(t)
	list := r.devices()
	for n, d := range list.Devices {
		if d.Name == "fan" {
			list.Devices[n].Name = "old-fan"
		}
	}
	file, err := registry.Render(list, "main")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "devices.go")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	if stderr, err := r.oikade(nil, "devices", "restore", path); err == nil || !strings.Contains(stderr, "--force") {
		t.Errorf("restored a file that disagrees: %v: %s", err, stderr)
	}
	deviceOf(t, r.devices(), "fan")
	r.stdout(nil, "devices", "restore", path, "--force")
	deviceOf(t, r.devices(), "old-fan")
}

func TestChangingANameWaitsForAllowSafetyWhileASafetyRuleRuns(t *testing.T) {
	t.Parallel()
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.sync()
	r.pair("spare-valve", "spare-valve")
	for _, args := range [][]string{
		{"devices", "name", "valve", "valve-two"},
		{"devices", "name", "leak-sensor", "leak-two"},
		{"devices", "replace", "valve", "spare-valve"},
	} {
		stderr, err := r.oikade(nil, args...)
		if err == nil || !strings.Contains(stderr, "--allow-safety") || !strings.Contains(stderr, "deploy") {
			t.Errorf("%v: %v: %s", args, err, stderr)
		}
	}
	deviceOf(t, r.devices(), "valve")
	r.stdout(nil, "devices", "name", "fan", "ceiling-fan", "--allow-safety")
	deviceOf(t, r.devices(), "ceiling-fan")
}
