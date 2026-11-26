package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/registry"
)

func (r *rig) devices() registry.Snapshot {
	r.t.Helper()
	var list registry.Snapshot
	out := r.stdout(nil, "list", "devices", "--json", "--all")
	if err := json.Unmarshal(out, &list.Devices); err != nil {
		r.t.Fatalf("reading devices: %v: %s", err, out)
	}
	return list
}

// deviceOf is the device on record with the key, or else the one with the ID.
func deviceOf(t *testing.T, list registry.Snapshot, ref string) registry.Device {
	t.Helper()
	var found []registry.Device
	for _, d := range list.Devices {
		if d.Removed {
			continue
		}
		if d.Key() == ref {
			return d
		}
		if d.ID == ref {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d devices are %s in %+v", len(found), ref, list)
	}
	return found[0]
}

// pair simulates pairing hardware, and waits until the host has it.
func (r *rig) pair(id string) {
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
	r.waitFor(func(l line) bool { return l.is("Changed capability", "device", simRef(id)) })
}

func TestAPairedDeviceIsUsableAtOnce(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.pair("hall-lamp")

	d := deviceOf(t, r.devices(), "hall-lamp")
	if d.ID != "hall-lamp" || d.Endpoint < registry.FirstEndpoint {
		t.Errorf("got %+v", d)
	}
	r.stdout(nil, "set", simRef("hall-lamp")+"/on", "on")
	r.wrote("hall-lamp", true)
	if !strings.Contains(string(r.stdout(nil, "list", "devices")), "hall-lamp") {
		t.Error("the table left it out")
	}
}

// A forgotten device comes back when its driver next reports it, as forget says, on its own endpoint.
func TestAForgottenDeviceComesBackOnItsOwnEndpoint(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.pair("shed-lamp")
	endpoint := deviceOf(t, r.devices(), "shed-lamp").Endpoint
	said := string(r.stdout(nil, "forget", simRef("shed-lamp")))
	if !strings.HasPrefix(said, "Forgot hardware/simulator/shed-lamp") ||
		!strings.Contains(said, "It comes back when its driver next reports it") {
		t.Errorf("forget said:\n%s", said)
	}
	if _, err := r.oikade(nil, "set", simRef("shed-lamp")+"/on", "on"); err == nil {
		t.Error("set a removed device")
	}
	if strings.Contains(string(r.stdout(nil, "list", "devices")), "shed-lamp") {
		t.Error("the table still lists it")
	}
	r.pair("other-lamp")
	if got := deviceOf(t, r.devices(), "other-lamp").Endpoint; got == endpoint {
		t.Errorf("another device took endpoint %d", got)
	}
	r.pair("shed-lamp")
	if got := deviceOf(t, r.devices(), "shed-lamp").Endpoint; got != endpoint {
		t.Errorf("came back on %d, want %d", got, endpoint)
	}
}

// goBuild builds a module of main.go and the generated devices.go, on this checkout's package oikade,
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

// Your devices file is yours: oikade types adds to it, and a device the host forgets keeps its line,
// which a deploy would bring back.
func TestTypesNeverChangesALineThatsThere(t *testing.T) {
	t.Parallel()
	r := start(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "devices.go")
	r.stdout(nil, "types", "--out", out)
	first, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	const code = "package main\n\nfunc main() { println(Fan.On.Device, Valve.Key) }\n"
	if err := goBuild(t, dir, code); err != nil {
		t.Fatalf("code naming its devices didn't compile: %v\n%s", err, first)
	}

	r.stdout(nil, "forget", "hardware/simulator/fan")
	r.stdout(nil, "types", "--out", out)
	if again, _ := os.ReadFile(out); string(again) != string(first) {
		t.Errorf("devices.go changed:\n%s\n%s", first, again)
	}
	if err := goBuild(t, dir, code); err != nil {
		t.Errorf("code naming a forgotten device stopped compiling: %v", err)
	}
}

func TestASafetyDeviceCanBeRemovedOnPurpose(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("leak-sensor", "open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "hardware/simulator/valve", "value", false) })
	r.sync()
	r.stdout(nil, "forget", "hardware/simulator/valve", "--allow-safety")
	if _, err := r.oikade(nil, "status", "hardware/simulator/valve"); err == nil {
		t.Error("the valve stayed")
	}
}

func TestRemovalAfterARestartSeesASavedSafetyClaim(t *testing.T) {
	t.Parallel()
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.change("leak-sensor", opened, true)
	r.wrote("valve", false)
	r.sync()
	r.stop()
	if got := r.ruleClaims("simulator/valve"); !holdsClaim(got, "Rule/simulator/valve on leak-shutoff", 3) {
		t.Fatalf("no Safety claim: %+v", got)
	}
	file, _ := filepath.Abs("testdata/devices-novalve.json")
	r.launchWithout("--home", homeBin, "--simulate", file, settle)
	r.awaitLoaded()
	r.sync()
	r.stop()
	if got := r.ruleClaims("simulator/valve"); !holdsClaim(got, "Rule/simulator/valve on leak-shutoff", 3) {
		t.Fatalf("restart lost the claim: %+v", got)
	}
	r.launchWithout("--home", homeBin, "--simulate", file, settle)
	r.awaitLoaded()
	r.sync()
	stderr, err := r.oikade(nil, "forget", "hardware/simulator/valve")
	if !strings.Contains(stderr, "--allow-safety") {
		t.Errorf("got %q", stderr)
	}
	r.stop()
	s := r.openStore()
	defer func() { _ = s.Close() }()
	_, ok, _ := s.Namespace("controls").Get("simulator/valve")
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

	st := r.status("hardware/simulator/valve")
	c := api.Capability{}
	if len(st.Devices) == 1 && len(st.Devices[0].Capabilities) > 0 {
		c = st.Devices[0].Capabilities[0]
	}
	if c.Level != "Safety" || winner(c) != "case/simulator/valve/on/leak-shutoff" {
		t.Errorf("the arbiter didn't load before the device reported: %+v", st.Devices)
	}
	stderr, err := r.deploy("renamed")
	if err == nil || !strings.Contains(stderr, "Safety case case/simulator/valve/on/leak-shutoff is gone") {
		t.Errorf("the deploy check missed the claim: %v: %s", err, stderr)
	}
}

// inRooms writes the simulated devices with some of them in rooms, and returns the file's path.
func inRooms(t *testing.T, rooms map[string]string) string {
	t.Helper()
	raw, err := os.ReadFile(devicesFile(t))
	if err != nil {
		t.Fatal(err)
	}
	var devices []map[string]any
	if err := json.Unmarshal(raw, &devices); err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if room, ok := rooms[d["name"].(string)]; ok {
			d["room"] = room
		}
	}
	path := filepath.Join(t.TempDir(), "devices.json")
	raw, err = json.Marshal(devices)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRemovingARoomsLastSensorWarnsOfTheHoldsThatReadIt(t *testing.T) {
	t.Parallel()
	r := start(t, "--simulate", inRooms(t, map[string]string{"damp-sensor": "cellar", "dry-sensor": "cellar"}))
	r.sync()
	r.wrote("dehumidifier", true)

	// The dry sensor is left in the cellar, so nothing warns, and the remove happens.
	stderr, err := r.oikade(nil, "forget", "hardware/simulator/damp-sensor")
	if err != nil || strings.Contains(stderr, "Warning") {
		t.Errorf("got %v: %q, want no warning while the cellar has a sensor", err, stderr)
	}
	for _, d := range r.devices().Devices {
		if d.ID == "damp-sensor" && !d.Removed {
			t.Errorf("damp-sensor is still on record: %+v", d)
		}
	}

	// The last one out warns, in --json as warning.
	var removed struct {
		OK        bool            `json:"ok"`
		Warning   string          `json:"warning"`
		Refs      []string        `json:"refs"`
		Forgotten []api.Forgotten `json:"forgotten"`
	}
	if err := json.Unmarshal(r.stdout(nil, "forget", "hardware/simulator/dry-sensor", "--json"), &removed); err != nil {
		t.Fatal(err)
	}
	if !removed.OK || len(removed.Forgotten) != 1 || !slices.Equal(removed.Refs,
		[]string{"hardware/simulator/dry-sensor"}) ||
		!strings.Contains(removed.Warning, "cellar-damp") ||
		!strings.Contains(removed.Warning, "cellar") {
		t.Errorf("got %+v, want a warning naming cellar-damp and the cellar", removed)
	}
	// Out of a room no hold reads, it says nothing.
	if stderr, err := r.oikade(nil, "forget", "hardware/simulator/fan"); err != nil || strings.Contains(stderr,
		"Warning") {
		t.Errorf("got %v: %q from a room no hold reads", err, stderr)
	}
}

// A claim on a mode is saved as the mode's number and comes back from disk as its name: the host
// writes it again, and oikade status shows it by name.
func TestAClaimOnAModeSurvivesARestart(t *testing.T) {
	t.Parallel()
	r := flowRig(t, "")
	r.launch(settle)
	r.awaitLoaded()
	r.change("bedroom-damp", opened, true)
	r.wrote("bedroom-ac", "dry")
	r.sync()
	r.stop()
	claims := r.savedClaims("simulator/bedroom-ac", "mode")
	if !holdsClaim(claims, "Rule/bedroom-dry-out", control.Normal) {
		t.Fatalf("no claim from the flow: %+v", claims)
	}
	if dry, _ := capability.Lookup(capability.Mode); claims[0].Value != float64(slices.Index(dry.Names, "dry")) {
		t.Errorf("the claim holds %v, want dry's number", claims[0].Value)
	}

	// The simulated unit starts in auto again, and its decision's Default is cool, so only the saved
	// claim makes it dry.
	r.launch(settle)
	r.awaitLoaded()
	r.sync()
	if n := r.count(wrote, "device", "hardware/simulator/bedroom-ac", "value", "dry"); n != 2 {
		t.Fatalf("dry was written %d times, want again after the restart. The log:\n%s", n, r.dump())
	}
	c := capabilityOf(t, r.status("hardware/simulator/bedroom-ac"), "simulator/bedroom-ac", "mode")
	if c.Value != "dry" || c.Level != "Normal" || len(c.Claims) == 0 || c.Claims[0].Value != "dry" {
		t.Errorf("status has the mode as %+v", c)
	}
}
