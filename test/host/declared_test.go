package host

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/control"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/registry"
)

func TestADeployAppliesTheHomesDeviceDeclarations(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	var before registry.Device
	r.until("the relay", func() bool {
		i := slices.IndexFunc(r.devices().Devices, func(d registry.Device) bool { return d.Name == "relay" })
		if i >= 0 {
			before = r.devices().Devices[i]
		}
		return i >= 0
	})
	stderr, err := r.deploy("plugins-declared")
	if err != nil {
		t.Fatalf("%v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
	for _, want := range []string{"Renamed relay to porch-relay", "Moved porch-relay to porch",
		"Set porch-relay's device type to OnOffLight", "Reserved shed-fan"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("no %q in:\n%s", want, stderr)
		}
	}
	d := deviceOf(t, r.devices(), "porch-relay")
	if !d.Declared || d.Room != "porch" || d.DeviceType != devicetype.OnOffLight || d.Endpoint != before.Endpoint {
		t.Errorf("got %+v, want it declared, in the porch, a light, on endpoint %d", d, before.Endpoint)
	}
	if !deviceOf(t, r.devices(), "shed-fan").Reserved() {
		t.Error("shed-fan isn't reserved")
	}

	// A version that declares nothing gives it back its app's name.
	stderr, err = r.deploy("plugins")
	if err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	r.awaitDeployed()
	if !strings.Contains(stderr, "Gave porch-relay back its app's name, relay") {
		t.Errorf("got:\n%s", stderr)
	}
	if d := deviceOf(t, r.devices(), "relay"); d.Declared || d.Room != before.Room || d.Endpoint != before.Endpoint {
		t.Errorf("got %+v, want it as before", d)
	}
}

// A device is its hardware, so renaming it moves nothing: its claims and a held latch stay where they
// are, under its hardware's key, and both survive a restart of the host.
func TestARenameKeepsClaimsAndAHeldLatchWithNothingMoved(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.change("leak-sensor", opened, true)
	r.untilValue("relays/valve", false)
	// The leak is gone, and the latch holds the valve shut until a reset.
	r.change("leak-sensor", opened, false)
	r.sync()
	r.until("the leak shutoff", func() bool {
		return strings.Contains(string(r.stdout(nil, "status", "hardware/relays/valve")), "leak-shutoff wants false")
	})

	stderr, err := r.deploy("plugins-mains")
	if err != nil {
		t.Fatalf("a rename was refused: %v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
	if !strings.Contains(stderr, "Renamed valve to mains-valve") {
		t.Errorf("got:\n%s", stderr)
	}
	for _, launch := range []func(){func() {}, func() { r.launchedPlugins() }} {
		launch()
		if status := string(r.stdout(nil, "status", "hardware/relays/valve")); !strings.Contains(status,
			"leak-shutoff wants false") {
			t.Errorf("the latch didn't hold:\n%s", status)
		}
		// The fixture's plugin opens its valve whenever it starts, as a deploy restarts it, and the
		// latched Safety claim closes it again.
		r.untilValue("relays/valve", false)
		r.stop()
		r.expectLatched()
	}
}

// expectLatched checks that a stopped host keeps the relays' valve shut by its leak case, latched,
// at Safety, under the valve's hardware.
func (r *rig) expectLatched() {
	r.t.Helper()
	const rule = "relays/valve on leak-shutoff"
	claims := r.ruleClaims("relays/valve")
	s := r.openStore()
	defer func() { _ = s.Close() }()
	raw, ok, err := s.Namespace(engine.HoldStatesNamespace).Get(rule)
	if err != nil || !ok {
		r.t.Fatalf("no hold state for %s: %v", rule, err)
	}
	var hold engine.HoldState
	if err := json.Unmarshal(raw, &hold); err != nil {
		r.t.Fatal(err)
	}
	if !hold.Latched || !holdsClaim(claims, control.SourceRule+rule, control.Safety) {
		r.t.Fatalf("the case isn't latched with its Safety claim: %+v, %+v. The log:\n%s", hold, claims, r.dump())
	}
}

func TestADeviceNotInYourCodeSaysHowToDeclareIt(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.untilValue("relays/valve", true)
	want := `home.Device("valve").Hardware("relays", "valve")`
	if stderr, err := r.oikade(nil, "list", "devices"); err != nil || !strings.Contains(stderr, want) {
		t.Errorf("oikade devices said %v:\n%s", err, stderr)
	}
	if out := string(r.stdout(nil, "list", "devices")); !strings.Contains(out, "not in your code") {
		t.Errorf("got:\n%s", out)
	}
	if out := string(r.stdout(nil, "status", "--json")); !strings.Contains(out, `"declare":"home.Device(\"valve\")`) {
		t.Errorf("oikade status --json has no line to declare valve:\n%s", out)
	}

	r.mustDeploy("plugins-mains")
	if out := string(r.stdout(nil, "list", "devices", "--json")); !strings.Contains(out, `"name":"mains-valve"`) ||
		!strings.Contains(out, `"declared":true`) {
		t.Errorf("got:\n%s", out)
	}
	if stderr, _ := r.oikade(nil, "list", "devices"); strings.Contains(stderr, `home.Device("mains-valve")`) {
		t.Errorf("a declared device has a line to declare it:\n%s", stderr)
	}
}

// Swapped hardware is a new device to what the host keeps: the Safety case on the old hardware goes,
// which a deploy says, and the spare's own claims it afresh. The name keeps its endpoint.
func TestADeclaredSwapOfHardwareStartsItsClaimsAgain(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.until("the spare relay", func() bool {
		return slices.ContainsFunc(r.devices().Devices, func(d registry.Device) bool { return d.ID == "spare-relay" })
	})
	r.mustDeploy("plugins-valve")
	before := deviceOf(t, r.devices(), "valve")
	r.change("leak-sensor", opened, true)
	r.untilValue("relays/valve", false)

	// The valve failed: its declaration's ID moves onto the spare.
	stderr, err := r.deploy("plugins-swapped")
	if err == nil || !strings.Contains(stderr, "Safety case case/relays/valve/on/leak-shutoff is gone") {
		t.Fatalf("a swap that ends the old hardware's Safety case went ahead: %v: %s", err, stderr)
	}
	r.mustDeploy("plugins-swapped", "--allow-safety")
	after := deviceOf(t, r.devices(), "valve")
	if after.ID != "spare-relay" || after.Endpoint != before.Endpoint {
		t.Errorf("got %+v, was %+v", after, before)
	}
	r.untilValue("relays/spare-relay", false)
	r.stop()
	got := r.ruleClaims("relays/spare-relay")
	if !holdsClaim(got, "Rule/relays/spare-relay on leak-shutoff", control.Safety) {
		t.Errorf("saved claims %+v, want the spare's case at Safety", got)
	}
	s := r.openStore()
	defer func() { _ = s.Close() }()
	raw, _, _ := s.Namespace(engine.ControlStatesNamespace).Get("relays/valve")
	if strings.Contains(string(raw), "leak-shutoff") {
		t.Errorf("the old hardware kept the case's claim: %s", raw)
	}
}

func TestADeclaredTypeThatTakesADeviceOutOfASafetyRoomWaitsForAllowSafety(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.until("the relay", func() bool {
		return slices.ContainsFunc(r.devices().Devices, func(d registry.Device) bool { return d.ID == "relay" })
	})
	r.mustDeploy("plugins-porch-plug")
	stderr, err := r.deploy("plugins-porch-light")
	if err == nil || !strings.Contains(stderr,
		"hardware/relays/relay would leave the devices case/porch/on/porch-leak writes at Safety") {
		t.Fatalf("declared the porch's plug a light: %v: %s", err, stderr)
	}
	if d := deviceOf(t, r.devices(), "relay"); d.DeviceType != devicetype.OnOffPlugInUnit {
		t.Errorf("the refused deploy left the relay a %q", d.DeviceType)
	}
	r.mustDeploy("plugins-porch-light", "--allow-safety")
	if d := deviceOf(t, r.devices(), "relay"); d.DeviceType != devicetype.OnOffLight {
		t.Errorf("with --allow-safety, the relay is a %q", d.DeviceType)
	}
}

// A host whose store kept claims and latches under devices' names, as hosts did before a device was
// its hardware, starts them again and says so: nothing finds them by hardware.
func TestAStoreKeptByNameStartsItsClaimsAndLatchesAgain(t *testing.T) {
	t.Parallel()
	r := preparePlugins(t)
	s := r.openStore()
	// As the last host left it, with the valve on record.
	devices, err := registry.Open(s.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	valve := registry.Report{ID: "valve", Driver: "plugin/relays", Name: "valve", Capabilities: []string{"on"}}
	if _, _, err := devices.Report(valve); err != nil {
		t.Fatal(err)
	}
	put := func(namespace, key string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Namespace(namespace).Put(key, raw); err != nil {
			t.Fatal(err)
		}
	}
	put(engine.ControlStatesNamespace, "valve", engine.ControlState{"on": control.State{Claims: []control.Claim{
		{Source: "Rule/valve on leak-shutoff", Level: control.Safety},
	}}})
	put(engine.HoldStatesNamespace, "valve on leak-shutoff", engine.HoldState{Active: true, Latched: true})
	_ = s.Close()

	r.launchedPlugins()
	const startedAgain = "Started claims, latches and flow runs again, as the store kept them by device name"
	if got := r.count(startedAgain, "states", 2.0); got != 1 {
		t.Errorf("logged that the two started again %d times. The log:\n%s", got, r.dump())
	}
	// Nothing holds the valve shut, so it stays open, as its plugin starts it.
	r.untilValue("relays/valve", true)
	if status := string(r.stdout(nil, "status", "hardware/relays/valve")); strings.Contains(status, "leak-shutoff") {
		t.Errorf("the old latch holds:\n%s", status)
	}
	r.stop()
	s = r.openStore()
	defer func() { _ = s.Close() }()
	if raw, ok, _ := s.Namespace(engine.ControlStatesNamespace).Get("valve"); ok {
		t.Errorf("the claims stayed: %s", raw)
	}
	if raw, ok, _ := s.Namespace(engine.HoldStatesNamespace).Get("valve on leak-shutoff"); ok {
		t.Errorf("the latch stayed: %s", raw)
	}
	_ = s.Close()

	// Once is enough: what it keeps from now on is by hardware.
	r.launchedPlugins()
	if got := r.count(startedAgain); got != 1 {
		t.Errorf("logged it %d times over two starts", got)
	}
}

// A line in your code gives its hardware the capabilities of its type and any others it lists, so a
// deploy refuses one that the hardware doesn't report, naming both.
func TestADeployRefusesHardwareThatLacksACapabilityItsLineGivesIt(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.until("the valve", func() bool {
		return slices.ContainsFunc(r.devices().Devices, func(d registry.Device) bool {
			return d.ID == "valve" && len(d.Capabilities) > 0
		})
	})
	stderr, err := r.deploy("plugins-warm-valve")
	if err == nil || !strings.Contains(stderr, "relays/valve has no temperature, which its line in your code gives it") {
		t.Fatalf("gave the valve a temperature: %v: %s", err, stderr)
	}
}

// A room in your code is the devices placed in it, which the host puts there, so a decision on it
// follows the sensor placed in it.
func TestARoomFollowsTheSensorPlacedInIt(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.mustDeploy("cuisine")
	motion, light := deviceOf(t, r.devices(), "cuisine-motion"), deviceOf(t, r.devices(), "cuisine-light")
	if !motion.Declared || motion.Room != "cuisine" || light.Room != "cuisine" ||
		light.DeviceType != devicetype.OnOffLight || light.Label != "Plafond" {
		t.Errorf("the sensor is %+v, and the light %+v", motion, light)
	}
	// Neither has a room in its app, so room/cuisine is oikade.Room's.
	if out := string(r.stdout(nil, "status", "room/cuisine")); !strings.Contains(out,
		"hardware/simulator/cuisine-motion ") ||
		!strings.Contains(out, "hardware/simulator/cuisine-light ") {
		t.Errorf("status room/cuisine:\n%s", out)
	}
	r.change("cuisine-motion", "occupied", true)
	r.untilValue("simulator/cuisine-light", true)
	r.change("cuisine-motion", "occupied", false)
	r.untilValue("simulator/cuisine-light", false)
}
