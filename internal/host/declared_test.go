package host

import (
	"errors"
	"slices"
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
	if err := known.takeDeclared(fan[1:], nil); err == nil || !strings.Contains(err.Error(), "Rule/frost holds shed-fan") {
		t.Errorf("dropped a claimed reservation: %v", err)
	}
	if resp, _ := known.remove(t.Context(), "lamp", true); !strings.Contains(resp.Error, "declared in your home's code") {
		t.Errorf("removed a declared device: %+v", resp)
	}
}
