package host

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/matter/node"
	"github.com/harrymitchinson/oikade/internal/registry"
	"github.com/harrymitchinson/oikade/internal/store"
)

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestIdentityIsMadeOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	data := openStore(t, dir)
	first, err := loadIdentity(data.Namespace(matterNamespace))
	if err != nil {
		t.Fatal(err)
	}
	if !(node.Setup{Passcode: first.Passcode, Discriminator: first.Discriminator}).Valid() ||
		len(first.UniqueID) != 32 || len(first.DAC) == 0 || len(first.Key) == 0 {
		t.Errorf("identity %+v", first)
	}
	_ = data.Close()

	data = openStore(t, dir)
	t.Cleanup(func() { _ = data.Close() })
	second, err := loadIdentity(data.Namespace(matterNamespace))
	if err != nil {
		t.Fatal(err)
	}
	if first.Passcode != second.Passcode || first.Discriminator != second.Discriminator ||
		first.UniqueID != second.UniqueID || !bytes.Equal(first.DAC, second.DAC) || !bytes.Equal(first.Key, second.Key) {
		t.Errorf("the identity changed: %+v, then %+v", first, second)
	}
}

func TestAnUnreadableIdentityIsNotReplaced(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	values := data.Namespace(matterNamespace)
	if err := values.Put(identityKey, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIdentity(values); err == nil {
		t.Error("an unreadable identity was accepted")
	}
	if raw, _, _ := values.Get(identityKey); string(raw) != "not json" {
		t.Errorf("it was replaced with %s", raw)
	}
}

func TestFabricsAreKeptInTheStore(t *testing.T) {
	dir := t.TempDir()
	data := openStore(t, dir)
	fabrics := fabricStore{data.Namespace(matterNamespace)}
	if got, err := fabrics.Load(); got != nil || err != nil {
		t.Errorf("before anything was saved: %q, %v", got, err)
	}
	if err := fabrics.Save([]byte("table")); err != nil {
		t.Fatal(err)
	}
	_ = data.Close()

	data = openStore(t, dir)
	t.Cleanup(func() { _ = data.Close() })
	if got, err := (fabricStore{data.Namespace(matterNamespace)}).Load(); string(got) != "table" || err != nil {
		t.Errorf("after reopening: %q, %v", got, err)
	}
}

type writes struct {
	calls int
	err   error
}

func (w *writes) Set(context.Context, types.NamespacedName, string, any) error {
	w.calls++
	return w.err
}

// TestASavedSafetyClaimRefusesAControllerBeforeItsDeviceReports is a claim that's only on disk: the
// device has no arbiter yet, and a valve must not open for it.
func TestASavedSafetyClaimRefusesAControllerBeforeItsDeviceReports(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logr.Discard()}
	eng := &engine.Engine{Log: logr.Discard(), Controls: controls}
	devices := hub.NewStore(logr.Discard(), eng)
	eng.Store = devices
	eng.Init()
	dev := key("valve")
	saved := engine.ControlState{capability.SwitchOn: control.State{
		Claims: []control.Claim{{Source: "Rule/leak-shutoff", Level: control.Safety, Value: 0}},
	}}
	if err := controls.Put(context.Background(), dev, saved); err != nil {
		t.Fatal(err)
	}

	setter := &writes{}
	var restored []any
	w := &matterWrites{
		engine: eng, devices: devices, setter: setter,
		restore: func(_ types.NamespacedName, _ string, otherwise any) { restored = append(restored, otherwise) },
	}
	if err := w.Set(context.Background(), dev, capability.SwitchOn, true); err == nil {
		t.Error("the write was let through")
	}
	// The device hasn't reported, so the claim is all a controller can be told.
	if setter.calls != 0 || len(restored) != 1 || restored[0] != false {
		t.Errorf("writes %d, restored %v", setter.calls, restored)
	}
	// What the claim wants anyway is no trouble.
	if err := w.Set(context.Background(), dev, capability.SwitchOn, false); err != nil {
		t.Error(err)
	}
	if setter.calls != 1 {
		t.Errorf("writes %d", setter.calls)
	}
	// Nor is a capability it doesn't hold.
	if err := w.Set(context.Background(), dev, capability.LightLevel, 50.0); err != nil {
		t.Error(err)
	}
}

func TestAFailedWriteTakesTheControllerBack(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	eng := &engine.Engine{
		Log:      logr.Discard(),
		Controls: &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logr.Discard()},
	}
	devices := hub.NewStore(logr.Discard(), eng)
	eng.Store = devices
	eng.Init()
	restored := 0
	w := &matterWrites{
		engine: eng, devices: devices, setter: &writes{err: errors.New("no answer")},
		restore: func(types.NamespacedName, string, any) { restored++ },
	}
	if err := w.Set(context.Background(), key("fan"), capability.SwitchOn, true); err == nil {
		t.Error("the failure was lost")
	}
	if restored != 1 {
		t.Errorf("restored %d", restored)
	}
}

// TestAClaimOnWhatsNoLongerArbitratedRefusesNothing is a stale record: a device that says it doesn't
// arbitrate a capability has nothing to enforce a claim with, and would refuse writes for good.
func TestAClaimOnWhatsNoLongerArbitratedRefusesNothing(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logr.Discard()}
	eng := &engine.Engine{Log: logr.Discard(), Controls: controls}
	devices := hub.NewStore(logr.Discard(), eng)
	eng.Store = devices
	eng.Init()
	dev := key("valve")
	devices.SetDevices(simInstance, map[types.NamespacedName]hub.Meta{dev: {Instance: simInstance}})
	saved := engine.ControlState{capability.SwitchOn: control.State{
		Claims: []control.Claim{{Source: "Rule/gone", Level: control.Safety}},
	}}
	if err := controls.Put(context.Background(), dev, saved); err != nil {
		t.Fatal(err)
	}
	if holder, _ := heldAtSafety(eng, devices, dev, capability.SwitchOn, true); holder != "" {
		t.Errorf("held by %s", holder)
	}
}

func TestAStoppingHostWritesNothing(t *testing.T) {
	setter := &writes{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &matterWrites{setter: setter}
	if err := w.Set(ctx, key("fan"), capability.SwitchOn, true); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
	if setter.calls != 0 {
		t.Error("it wrote")
	}
}

// TestOnlyHardwareThatMapsIsOnTheBridge says what's left off, and that a device whose driver is away
// stays on, unreachable.
func TestOnlyHardwareThatMapsIsOnTheBridge(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	eng := &engine.Engine{
		Log:      logr.Discard(),
		Controls: &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logr.Discard()},
	}
	devices := hub.NewStore(logr.Discard(), eng)
	eng.Store = devices
	eng.Init()
	known := newInventory(reg, devices, eng, logr.Discard())
	known.attach(simInstance)
	for _, d := range []driver.AppDevice{
		{ID: "lamp", Name: "lamp", Capabilities: []string{capability.SwitchOn}},
		{ID: "gone", Name: "gone", Capabilities: []string{capability.SwitchOn}},
		{ID: "meter", Name: "meter", Capabilities: []string{capability.Battery}},
	} {
		if _, err := known.paired(simInstance, d, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := reg.Report(registry.Report{
		ID: "away", Driver: "other/plugin", Name: "away", Capabilities: []string{capability.SwitchOn},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove("gone"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Reserve("future", ""); err != nil {
		t.Fatal(err)
	}
	devices.Discovered(key("lamp"), driver.Info{Found: true})

	m := &matterBridge{devices: devices, known: known, id: identity{UniqueID: "BRIDGE"}}
	want := m.want()
	if len(want) != 2 {
		t.Fatalf("on the bridge: %+v", want)
	}
	lamp, _ := reg.Get("lamp")
	away, _ := reg.Get("away")
	if d := want[lamp.Endpoint]; d.Key != key("lamp") || d.Absent || d.Unique == "" {
		t.Errorf("lamp: %+v", d)
	}
	if d := want[away.Endpoint]; d.Key != key("away") || !d.Absent {
		t.Errorf("away: %+v", d)
	}
	if want[lamp.Endpoint].Unique == want[away.Endpoint].Unique {
		t.Error("two devices share a unique ID")
	}
}
