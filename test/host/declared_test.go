package host

import (
	"slices"
	"strings"
	"testing"

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
	for _, want := range []string{"Renamed relay to porch-relay", "Moved porch-relay to Porch",
		"Set porch-relay's device type to OnOffLight", "Reserved shed-fan"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("no %q in:\n%s", want, stderr)
		}
	}
	d := deviceOf(t, r.devices(), "porch-relay")
	if !d.Declared || d.Room != "Porch" || d.DeviceType != devicetype.OnOffLight || d.Endpoint != before.Endpoint {
		t.Errorf("got %+v, want it declared, in the porch, a light, on endpoint %d", d, before.Endpoint)
	}
	if !deviceOf(t, r.devices(), "shed-fan").Reserved() {
		t.Error("shed-fan isn't reserved")
	}
	if stderr, err := r.oikade(nil, "devices", "name", "porch-relay", "garden-relay"); err == nil ||
		!strings.Contains(stderr, "declared in your home's code") {
		t.Errorf("renamed a declared device: %v: %s", err, stderr)
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
