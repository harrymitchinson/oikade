package host

import (
	"context"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/registry"
	"github.com/harrymitchinson/oikade/internal/store"
)

// declaredHome is an inventory with one paired lamp, on a store a test can open again.
func declaredHome(t *testing.T, dir string) (*inventory, *engine.Stored[engine.ControlState]) {
	t.Helper()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()}
	eng := &engine.Engine{Log: logging.Discard(), Controls: controls}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), devices, eng, logging.Discard())
	known.attach(pluginKey("x"))
	if _, err := known.paired(pluginKey("x"), driver.AppDevice{ID: "l1", Name: "Lamp", Capabilities: []string{capability.On}}, nil); err != nil {
		t.Fatal(err)
	}
	return known, controls
}

var readingLamp = []wire.DeviceDeclaration{{Name: "reading-lamp", Plugin: "x", ID: "l1", Room: "Study"}}

func TestDeclaredDevicesAreSavedFirstAndSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	known, _ := declaredHome(t, dir)
	if err := known.takeDeclared(readingLamp); err != nil {
		t.Fatal(err)
	}
	if _, ok := known.store.Get(key("reading-lamp")); !ok {
		t.Error("the hub doesn't have the new name")
	}
	if err := known.rename("reading-lamp", "desk-lamp"); err != nil {
		t.Fatal(err) // The inventory itself doesn't refuse: the API does.
	}
	if err := known.declared("desk-lamp"); err == nil || !strings.Contains(err.Error(), "change it there") {
		t.Errorf("a declared device isn't refused: %v", err)
	}
	// The registry was changed behind the declaration's back, as a crash before applying would leave it.
	if err := known.loadDeclared(); err != nil {
		t.Fatal(err)
	}
	if d, ok := known.registry.Get("reading-lamp"); !ok || d.Room != "Study" {
		t.Errorf("loading the saved declarations gave %+v", known.registry.List())
	}
}

func TestARenameOfAClaimedDeviceIsRefused(t *testing.T) {
	known, controls := declaredHome(t, t.TempDir())
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/night", Level: control.Normal}}}}
	if err := controls.Put(context.Background(), key("lamp"), state); err != nil {
		t.Fatal(err)
	}
	if _, err := known.planDeclared(readingLamp); err == nil || !strings.Contains(err.Error(), "Rule/night holds lamp") {
		t.Errorf("got %v", err)
	}
	if err := known.takeDeclared(readingLamp); err == nil {
		t.Error("took it")
	}
	if _, ok := known.registry.Get("lamp"); !ok {
		t.Error("the claimed device was renamed")
	}
	raw, ok, _ := known.declarations.Get(declaredKey)
	if ok {
		t.Errorf("saved refused declarations %s", raw)
	}
	// A room changes no name, so a claim doesn't stop it.
	if err := known.takeDeclared([]wire.DeviceDeclaration{{Name: "lamp", Plugin: "x", ID: "l1", Room: "Hall"}}); err != nil {
		t.Fatal(err)
	}
	if d, _ := known.registry.Get("lamp"); d.Room != "Hall" || !d.Declared {
		t.Errorf("got %+v", d)
	}
}

func TestAClaimedReservationStaysDeclaredAndADeclaredDeviceIsntRemoved(t *testing.T) {
	known, controls := declaredHome(t, t.TempDir())
	fan := []wire.DeviceDeclaration{{Name: "shed-fan"}, {Name: "lamp", Plugin: "x", ID: "l1"}}
	if err := known.takeDeclared(fan); err != nil {
		t.Fatal(err)
	}
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/frost", Level: control.Safety}}}}
	if err := controls.Put(context.Background(), key("shed-fan"), state); err != nil {
		t.Fatal(err)
	}
	if err := known.takeDeclared(fan[1:]); err == nil || !strings.Contains(err.Error(), "Rule/frost holds shed-fan") {
		t.Errorf("dropped a claimed reservation: %v", err)
	}
	if resp, _ := known.remove(context.Background(), "lamp", true); !strings.Contains(resp.Error, "declared in your home's code") {
		t.Errorf("removed a declared device: %+v", resp)
	}
}
