package host

import (
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/registry"
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

func TestARenameInADeployKeepsASafetyClaimHolding(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.change("leak-sensor", opened, true)
	r.untilValue("valve", false)
	r.until("the leak shutoff", func() bool {
		return strings.Contains(string(r.stdout(nil, "status", "valve")), "leak-shutoff wants false")
	})

	stderr, err := r.deploy("plugins-mains")
	if err != nil {
		t.Fatalf("a rename was refused: %v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
	if !strings.Contains(stderr, "Renamed valve to mains-valve") {
		t.Errorf("got:\n%s", stderr)
	}
	if status := string(r.stdout(nil, "status", "mains-valve")); !strings.Contains(status, "leak-shutoff wants false") {
		t.Errorf("the claim didn't come with it:\n%s", status)
	}
	// The fixture's plugin opens its valve whenever it starts, as a deploy restarts it, and the
	// Safety claim, under the new name, closes it again.
	r.untilValue("mains-valve", false)
	r.stop()
	if got := r.ruleClaims("mains-valve"); !holdsClaim(got, "Rule/mains-valve on leak-shutoff", control.Safety) {
		t.Errorf("saved claims %+v, want the case's at Safety under the new name", got)
	}
}

func TestADeviceNotInYourCodeSaysHowToDeclareIt(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.untilValue("valve", true)
	want := `home.Device("valve").Hardware("relays", "valve")`
	if stderr, err := r.oikade(nil, "devices"); err != nil || !strings.Contains(stderr, want) {
		t.Errorf("oikade devices said %v:\n%s", err, stderr)
	}
	if out := string(r.stdout(nil, "devices")); !strings.Contains(out, "not in your code") {
		t.Errorf("got:\n%s", out)
	}
	if out := string(r.stdout(nil, "status", "--json")); !strings.Contains(out, `"declare":"home.Device(\"valve\")`) {
		t.Errorf("oikade status --json has no line to declare valve:\n%s", out)
	}

	r.mustDeploy("plugins-mains")
	if out := string(r.stdout(nil, "devices", "--json")); !strings.Contains(out, `"name":"mains-valve"`) ||
		!strings.Contains(out, `"declared":true`) {
		t.Errorf("got:\n%s", out)
	}
	if stderr, _ := r.oikade(nil, "devices"); strings.Contains(stderr, `home.Device("mains-valve")`) {
		t.Errorf("a declared device has a line to declare it:\n%s", stderr)
	}
}

func TestADeclaredSwapOfHardwareKeepsTheNameEndpointAndClaims(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.until("the spare relay", func() bool {
		return slices.ContainsFunc(r.devices().Devices, func(d registry.Device) bool { return d.ID == "spare-relay" })
	})
	r.mustDeploy("plugins-valve")
	before := deviceOf(t, r.devices(), "valve")
	r.change("leak-sensor", opened, true)
	r.untilValue("valve", false)

	// The valve failed: its declaration's ID moves onto the spare.
	stderr, err := r.deploy("plugins-swapped")
	if err != nil {
		t.Fatalf("%v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
	after := deviceOf(t, r.devices(), "valve")
	if after.ID != "spare-relay" || after.Endpoint != before.Endpoint {
		t.Errorf("got %+v, was %+v", after, before)
	}
	r.untilValue("valve", false)
	r.stop()
	if got := r.ruleClaims("valve"); !holdsClaim(got, "Rule/valve on leak-shutoff", control.Safety) {
		t.Errorf("saved claims %+v, want the Safety claim still on the name", got)
	}
}

func TestADeclaredTypeThatTakesADeviceOutOfASafetyRoomWaitsForAllowSafetyRemoval(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.until("the relay", func() bool {
		return slices.ContainsFunc(r.devices().Devices, func(d registry.Device) bool { return d.ID == "relay" })
	})
	r.mustDeploy("plugins-porch-plug")
	stderr, err := r.deploy("plugins-porch-light")
	if err == nil || !strings.Contains(stderr, "relay would leave the devices porch on porch-leak writes at Safety") {
		t.Fatalf("declared the porch's plug a light: %v: %s", err, stderr)
	}
	if d := deviceOf(t, r.devices(), "relay"); d.DeviceType != devicetype.OnOffPlugInUnit {
		t.Errorf("the refused deploy left the relay a %q", d.DeviceType)
	}
	r.mustDeploy("plugins-porch-light", "--allow-safety-removal")
	if d := deviceOf(t, r.devices(), "relay"); d.DeviceType != devicetype.OnOffLight {
		t.Errorf("with --allow-safety-removal, the relay is a %q", d.DeviceType)
	}
}
