package host

import (
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
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

var readingLamp = []wire.DeviceDeclaration{{Hardware: "x/l1", Room: "study"}}

func TestDeclaredDevicesAreSavedFirstAndSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	known, _ := declaredHome(t, dir)
	if err := known.takeDeclared(readingLamp); err != nil {
		t.Fatal(err)
	}
	if snap, ok := known.store.Get(key("x/l1")); !ok || snap.Room != "study" {
		t.Errorf("the hub has %+v, %v, want the lamp by its hardware, in the study", snap, ok)
	}
	if _, err := known.registry.Declare([]registry.Declaration{{Hardware: "x/l1", Room: "hall"}}); err != nil {
		t.Fatal(err)
	}
	// The registry was changed behind the declaration's back, as a crash before applying would leave
	// it.
	if err := known.loadDeclared(); err != nil {
		t.Fatal(err)
	}
	if d, ok := known.registry.ByKey("x/l1"); !ok || d.Room != "study" {
		t.Errorf("loading the saved declarations gave %+v", known.registry.List())
	}
}

// A line a home saved before devices were their hardware names none, so loading it skips that line,
// and takes the rest.
func TestSavedDeclarationsWithNoHardwareAreSkipped(t *testing.T) {
	known, _ := declaredHome(t, t.TempDir())
	saved := `{"inventory":[{"name":"shed-fan"},{"name":"lamp","plugin":"x","id":"l1"},{"hardware":"x/l1","room":"study"}]}`
	if err := known.declarations.Put(declaredKey, []byte(saved)); err != nil {
		t.Fatal(err)
	}
	if err := known.loadDeclared(); err != nil {
		t.Fatal(err)
	}
	if d, ok := known.registry.ByKey("x/l1"); !ok || d.Room != "study" || !known.isDeclared("x/l1") {
		t.Errorf("got %+v", known.registry.List())
	}
}

// What the host keeps for a device is by its hardware, so a move leaves its claims where they are,
// and its rules' keys with them.
func TestAMoveMovesNoClaims(t *testing.T) {
	known, controls := declaredHome(t, t.TempDir())
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{
		{Source: "Rule/x/l1 on night", Level: control.Safety}, {Source: "Rule/evening", Level: control.Normal},
	}}}
	if err := controls.Put(t.Context(), key("x/l1"), state); err != nil {
		t.Fatal(err)
	}
	if err := known.takeDeclared(readingLamp); err != nil {
		t.Fatal(err)
	}
	got, ok := controls.Get(key("x/l1"))
	if !ok || len(got[capability.On].Claims) != 2 || got[capability.On].Claims[0].Source != "Rule/x/l1 on night" {
		t.Errorf("the lamp's claims are %+v, %v, want them as they were", got, ok)
	}
	if holders, safety := known.claimed(key("x/l1")); !slices.Equal(holders, []string{"case/x/l1/on/night", "flow/evening"}) || !safety {
		t.Errorf("claimed says %v, %v, by the lamp's hardware", holders, safety)
	}
}

// A device your home's code declares can't be forgotten, as the next deploy would bring it back.
func TestADeclaredDeviceIsntForgotten(t *testing.T) {
	known, _ := declaredHome(t, t.TempDir())
	if err := known.takeDeclared(readingLamp); err != nil {
		t.Fatal(err)
	}
	if resp, _ := known.forget(t.Context(), []string{"x/l1"}, true, nil); !strings.Contains(resp.Error, "declared in your home's code: delete its declaration and deploy, then forget it") {
		t.Errorf("removed a declared device: %+v", resp)
	}
}

// pairedHome is an inventory with devices paired from plugin x, and a server whose process's holds
// have read reads, as a deploy sees them before the process stops.
func pairedHome(t *testing.T, reads map[string][]engine.RoomRead, devices ...driver.AppDevice) *deployer {
	t.Helper()
	known, _ := declaredHome(t, t.TempDir())
	for _, d := range devices {
		if _, err := known.paired(pluginKey("x"), d, nil); err != nil {
			t.Fatal(err)
		}
	}
	srv := &server{session: &session{reads: reads}, declaring: known.declaring}
	return &deployer{srv: srv, log: logging.Discard()}
}

// A deploy warns of a room its declarations leave with none of the devices a hold reads there, by
// the hold's ref, and not of one another device still covers, one that was empty already, or a hold
// the next version drops.
func TestADeployWarnsOfARoomItsDeclarationsEmpty(t *testing.T) {
	plugs := []devicetype.Type{devicetype.OnOffPlugInUnit}
	d := pairedHome(t, map[string][]engine.RoomRead{
		"x/h on hall-plugs": {{Room: "hall", Types: plugs}},
		"x/d on occupied":   {{Room: "study"}},
		"dropped on hall":   {{Room: "hall"}},
		"attic-fan":         {{Room: "attic"}},
	},
		driver.AppDevice{ID: "h", Name: "heater", Room: "hall", Capabilities: []string{capability.On}, DeviceType: devicetype.OnOffPlugInUnit},
		driver.AppDevice{ID: "s", Name: "sensor", Room: "study", Capabilities: []string{capability.Occupied}},
		driver.AppDevice{ID: "d", Name: "desk-lamp", Room: "study", Capabilities: []string{capability.On}},
	)
	// The heater, declared a light, leaves the hall with no plug; the sensor moves to the loft, but
	// the desk lamp is still in the study.
	next := wire.Manifest{
		Inventory: []wire.DeviceDeclaration{
			{Hardware: "x/h", Room: "hall", DeviceType: devicetype.OnOffLight},
			{Hardware: "x/s", Room: "loft"},
		},
		Rules: []wire.RuleInfo{{Name: "x/h on hall-plugs"}, {Name: "x/d on occupied"}, {Name: "attic-fan"}},
	}
	got := d.declaredWarning(next, wire.Manifest{})
	want := "hall has no devices now that case/x/h/on/hall-plugs read, so they keep their state until one is back in hall"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Declaring the devices as they are warns of nothing.
	same := wire.Manifest{Inventory: []wire.DeviceDeclaration{{Hardware: "x/h", Room: "hall"}}, Rules: next.Rules}
	if got := d.declaredWarning(same, wire.Manifest{}); got != "" {
		t.Errorf("declaring the heater where it is warns %q", got)
	}
}

// A device a declaration moves into a room that decides a capability it has its own decision on is
// named, unless its decision hands over to the room's with Then, or it was there already.
func TestADeployWarnsOfADeviceThatKeepsItsOwnDecisionInItsNewRoom(t *testing.T) {
	d := pairedHome(t, nil,
		driver.AppDevice{ID: "p", Name: "pendant", Room: "hall", Capabilities: []string{capability.On}, DeviceType: devicetype.OnOffLight},
	)
	decides := func(lines ...string) []wire.DeviceControl {
		return []wire.DeviceControl{
			{Room: "kitchen", Entry: control.Entry{Capability: capability.On, Lines: []string{"Rule/kitchen on dark"}}},
			{Device: "x/p", Entry: control.Entry{Capability: capability.On, Lines: lines}},
		}
	}
	into := func(room string, settings []wire.DeviceControl) wire.Manifest {
		return wire.Manifest{Inventory: []wire.DeviceDeclaration{{Hardware: "x/p", Room: room}}, Control: settings}
	}
	own := decides("Rule/x/p on dinner")
	want := "hardware/x/p/on keeps its own decision rather than kitchen's, unless the home hands it over with Then"
	if got := d.declaredWarning(into("kitchen", own), wire.Manifest{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := d.declaredWarning(into("kitchen", decides("Rule/x/p on dinner", "Rule/kitchen on dark")), wire.Manifest{}); got != "" {
		t.Errorf("a decision that hands over with Then warns %q", got)
	}
	if got := d.declaredWarning(into("hall", own), wire.Manifest{}); got != "" {
		t.Errorf("staying in the hall warns %q", got)
	}
	// A room's decision with no case, such as flows and a default, hands over all of its lines.
	flows := []wire.DeviceControl{
		{Room: "kitchen", Entry: control.Entry{Capability: capability.On, Lines: []string{control.LineOverride, control.LineFlows}}},
		{Device: "x/p", Entry: control.Entry{Capability: capability.On, Lines: []string{"Rule/x/p on dinner", control.LineFlows}}},
	}
	if got := d.declaredWarning(into("kitchen", flows), wire.Manifest{}); got != "" {
		t.Errorf("a decision that has the room's lines warns %q", got)
	}
	flows[1].Entry.Lines = []string{"Rule/x/p on dinner"}
	if got := d.declaredWarning(into("kitchen", flows), wire.Manifest{}); got == "" {
		t.Error("a decision without the room's Flows line doesn't warn")
	}
}
