package host

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
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

var readingLamp = []wire.DeviceDeclaration{{Name: "reading-lamp", Plugin: "x", ID: "l1", Room: "study"}}

func TestDeclaredDevicesAreSavedFirstAndSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	known, _ := declaredHome(t, dir)
	if err := known.takeDeclared(readingLamp, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := known.store.Get(key("reading-lamp")); !ok {
		t.Error("the hub doesn't have the new name")
	}
	if _, err := known.registry.Declare([]registry.Declaration{{Name: "desk-lamp", Driver: "plugin/x", ID: "l1"}}); err != nil {
		t.Fatal(err)
	}
	// The registry was changed behind the declaration's back, as a crash before applying would leave it.
	if err := known.loadDeclared(); err != nil {
		t.Fatal(err)
	}
	if d, ok := known.registry.Get("reading-lamp"); !ok || d.Room != "study" {
		t.Errorf("loading the saved declarations gave %+v", known.registry.List())
	}
}

func TestARenamedDeviceTakesItsClaims(t *testing.T) {
	known, controls := declaredHome(t, t.TempDir())
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{
		{Source: "Rule/lamp on night", Level: control.Safety}, {Source: "Flow/evening", Level: control.Normal},
		// A room's case keyed by the same name, which isn't the device's own.
		{Source: "Rule/lamp on dusk", Level: control.Normal},
	}}}
	if err := controls.Put(t.Context(), key("lamp"), state); err != nil {
		t.Fatal(err)
	}
	var renamed func(string) string
	known.renamed = func(r func(string) string) error { renamed = r; return nil }
	if err := known.takeDeclared(readingLamp, []string{"reading-lamp on night"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := controls.Get(key("lamp")); ok {
		t.Error("the claims stayed under the old name")
	}
	got, _ := controls.Get(key("reading-lamp"))
	sources := make([]string, 0, len(got[capability.On].Claims))
	for _, c := range got[capability.On].Claims {
		sources = append(sources, c.Source)
	}
	if want := []string{"Rule/reading-lamp on night", "Flow/evening", "Rule/lamp on dusk"}; !slices.Equal(sources, want) {
		t.Errorf("got claims from %v, want %v", sources, want)
	}
	if got := renamed("lamp on night"); got != "reading-lamp on night" {
		t.Errorf("the rules' state was told lamp on night is now %q", got)
	}
}

func TestARenameWhoseRuleStateCantMoveKeepsItsClaims(t *testing.T) {
	known, controls := declaredHome(t, t.TempDir())
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/lamp on night", Level: control.Safety}}}}
	if err := controls.Put(t.Context(), key("lamp"), state); err != nil {
		t.Fatal(err)
	}
	calls := 0
	known.renamed = func(func(string) string) error {
		calls++
		if calls == 1 {
			return errors.New("disk full")
		}
		return nil
	}
	if err := known.takeDeclared(readingLamp, nil); err == nil {
		t.Fatal("took it")
	}
	if _, ok := known.registry.Get("lamp"); !ok {
		t.Error("the registry renamed it")
	}
	got, _ := controls.Get(key("lamp"))
	if len(got[capability.On].Claims) != 1 || got[capability.On].Claims[0].Source != "Rule/lamp on night" {
		t.Errorf("lamp's claims are %+v, want them back as they were", got)
	}
	if _, ok := controls.Get(key("reading-lamp")); ok {
		t.Error("claims were left under the new name")
	}
}

func TestRuleStateFollowsARename(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	holds := &engine.Stored[engine.HoldState]{Values: data.Namespace(engine.HoldStatesNamespace), Log: logging.Discard()}
	ctx := t.Context()
	for _, name := range []string{"a on night", "b on night", "hall on motion"} {
		if err := holds.Put(ctx, key(name), engine.HoldState{Active: name == "a on night"}); err != nil {
			t.Fatal(err)
		}
	}
	// a and b swap names.
	swap := map[string]string{"a": "b", "b": "a"}
	if err := renameKeys(holds, func(k string) string { return control.RenamedKey(k, swap) }); err != nil {
		t.Fatal(err)
	}
	if st, _ := holds.Get(key("b on night")); !st.Active {
		t.Error("a's hold didn't move to b")
	}
	if st, ok := holds.Get(key("a on night")); !ok || st.Active {
		t.Errorf("b's hold didn't move to a: %+v, %v", st, ok)
	}
	if _, ok := holds.Get(key("hall on motion")); !ok {
		t.Error("another device's hold went")
	}
}

func TestAClaimedReservationStaysDeclaredAndADeclaredDeviceIsntRemoved(t *testing.T) {
	known, controls := declaredHome(t, t.TempDir())
	fan := []wire.DeviceDeclaration{{Name: "shed-fan"}, {Name: "lamp", Plugin: "x", ID: "l1"}}
	if err := known.takeDeclared(fan, nil); err != nil {
		t.Fatal(err)
	}
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/frost", Level: control.Safety}}}}
	if err := controls.Put(t.Context(), key("shed-fan"), state); err != nil {
		t.Fatal(err)
	}
	if err := known.takeDeclared(fan[1:], nil); err == nil || !strings.Contains(err.Error(), "flow/frost holds shed-fan") {
		t.Errorf("dropped a claimed reservation: %v", err)
	}
	if resp, _ := known.forget(t.Context(), []string{"lamp"}, true, nil); !strings.Contains(resp.Error, "declared in your home's code: delete its declaration and deploy, then forget it") {
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
// the name the hold has in the next version, and not of one another device still covers, one that
// was empty already, or a hold the next version drops.
func TestADeployWarnsOfARoomItsDeclarationsEmpty(t *testing.T) {
	plugs := []devicetype.Type{devicetype.OnOffPlugInUnit}
	d := pairedHome(t, map[string][]engine.RoomRead{
		"heater on hall-plugs":  {{Room: "hall", Types: plugs}},
		"desk-lamp on occupied": {{Room: "study"}},
		"dropped on hall":       {{Room: "hall"}},
		"attic-fan":             {{Room: "attic"}},
	},
		driver.AppDevice{ID: "h", Name: "heater", Room: "hall", Capabilities: []string{capability.On}, DeviceType: devicetype.OnOffPlugInUnit},
		driver.AppDevice{ID: "s", Name: "sensor", Room: "study", Capabilities: []string{capability.Occupied}},
		driver.AppDevice{ID: "d", Name: "desk-lamp", Room: "study", Capabilities: []string{capability.On}},
	)
	// The heater, renamed and declared a light, leaves the hall with no plug; the sensor moves to the
	// loft, but the desk lamp is still in the study.
	next := wire.Manifest{
		Inventory: []wire.DeviceDeclaration{
			{Name: "radiator", Plugin: "x", ID: "h", Room: "hall", DeviceType: devicetype.OnOffLight},
			{Name: "sensor", Plugin: "x", ID: "s", Room: "loft"},
		},
		Rules: []wire.RuleInfo{{Name: "radiator on hall-plugs"}, {Name: "desk-lamp on occupied"}, {Name: "attic-fan"}},
	}
	got := d.declaredWarning(next, wire.Manifest{}, map[string]string{"heater": "radiator"})
	want := "hall has no devices now that case/radiator/on/hall-plugs read, so they keep their state until one is back in hall"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Declaring the devices as they are warns of nothing.
	same := wire.Manifest{Inventory: []wire.DeviceDeclaration{{Name: "heater", Plugin: "x", ID: "h", Room: "hall"}}, Rules: next.Rules}
	if got := d.declaredWarning(same, wire.Manifest{}, nil); got != "" {
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
			{Device: "pendant", Entry: control.Entry{Capability: capability.On, Lines: lines}},
		}
	}
	into := func(room string, settings []wire.DeviceControl) wire.Manifest {
		return wire.Manifest{Inventory: []wire.DeviceDeclaration{{Name: "pendant", Plugin: "x", ID: "p", Room: room}}, Control: settings}
	}
	own := decides("Rule/pendant on dinner")
	want := "device/pendant/on keeps its own decision rather than kitchen's, unless the home hands it over with Then"
	if got := d.declaredWarning(into("kitchen", own), wire.Manifest{}, nil); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := d.declaredWarning(into("kitchen", decides("Rule/pendant on dinner", "Rule/kitchen on dark")), wire.Manifest{}, nil); got != "" {
		t.Errorf("a decision that hands over with Then warns %q", got)
	}
	if got := d.declaredWarning(into("hall", own), wire.Manifest{}, nil); got != "" {
		t.Errorf("staying in the hall warns %q", got)
	}
	// A room's decision with no case, such as flows and a default, hands over all of its lines.
	flows := []wire.DeviceControl{
		{Room: "kitchen", Entry: control.Entry{Capability: capability.On, Lines: []string{control.LineOverride, control.LineFlows}}},
		{Device: "pendant", Entry: control.Entry{Capability: capability.On, Lines: []string{"Rule/pendant on dinner", control.LineFlows}}},
	}
	if got := d.declaredWarning(into("kitchen", flows), wire.Manifest{}, nil); got != "" {
		t.Errorf("a decision that has the room's lines warns %q", got)
	}
	flows[1].Entry.Lines = []string{"Rule/pendant on dinner"}
	if got := d.declaredWarning(into("kitchen", flows), wire.Manifest{}, nil); got == "" {
		t.Error("a decision without the room's Flows line doesn't warn")
	}
}
