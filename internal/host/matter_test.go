package host

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/node"
	"github.com/harrymitchinson/oikade/internal/matterbridge"
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

func (w *writes) Set(context.Context, ident.ID, string, any) error {
	w.calls++
	return w.err
}

// TestASavedSafetyClaimRefusesAControllerBeforeItsDeviceReports is a claim that's only on disk: the
// device has no arbiter yet, and a valve must not open for it.
func TestASavedSafetyClaimRefusesAControllerBeforeItsDeviceReports(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()}
	eng := &engine.Engine{Log: logging.Discard(), Controls: controls}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	dev := key("valve")
	saved := engine.ControlState{capability.On: control.State{
		Claims: []control.Claim{{Source: "Rule/leak-shutoff", Level: control.Safety, Value: 0}},
	}}
	if err := controls.Put(context.Background(), dev, saved); err != nil {
		t.Fatal(err)
	}

	setter := &writes{}
	var restored []any
	w := &matterWrites{
		engine: eng, devices: devices, setter: setter,
		restore: func(_ ident.ID, _ string, otherwise any) { restored = append(restored, otherwise) },
	}
	if err := w.Set(context.Background(), dev, capability.On, true); err == nil {
		t.Error("the write was let through")
	}
	// The device hasn't reported, so the claim is all a controller can be told.
	if setter.calls != 0 || len(restored) != 1 || restored[0] != false {
		t.Errorf("writes %d, restored %v", setter.calls, restored)
	}
	// What the claim wants anyway is no trouble.
	if err := w.Set(context.Background(), dev, capability.On, false); err != nil {
		t.Error(err)
	}
	if setter.calls != 1 {
		t.Errorf("writes %d", setter.calls)
	}
	// Nor is a capability it doesn't hold.
	if err := w.Set(context.Background(), dev, capability.Brightness, 50.0); err != nil {
		t.Error(err)
	}
}

func TestAFailedWriteTakesTheControllerBack(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	eng := &engine.Engine{
		Log:      logging.Discard(),
		Controls: &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()},
	}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	restored := 0
	w := &matterWrites{
		engine: eng, devices: devices, setter: &writes{err: errors.New("no answer")},
		restore: func(ident.ID, string, any) { restored++ },
	}
	if err := w.Set(context.Background(), key("fan"), capability.On, true); err == nil {
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
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()}
	eng := &engine.Engine{Log: logging.Discard(), Controls: controls}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	dev := key("valve")
	devices.SetDevices(simInstance, map[ident.ID]hub.Meta{dev: {Instance: simInstance}})
	saved := engine.ControlState{capability.On: control.State{
		Claims: []control.Claim{{Source: "Rule/gone", Level: control.Safety}},
	}}
	if err := controls.Put(context.Background(), dev, saved); err != nil {
		t.Fatal(err)
	}
	if holder, _ := heldAtSafety(eng, devices, dev, capability.On, true); holder != "" {
		t.Errorf("held by %s", holder)
	}
}

func TestAStoppingHostWritesNothing(t *testing.T) {
	setter := &writes{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &matterWrites{setter: setter}
	if err := w.Set(ctx, key("fan"), capability.On, true); !errors.Is(err, context.Canceled) {
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
		Log:      logging.Discard(),
		Controls: &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()},
	}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, eng, logging.Discard())
	known.attach(simInstance)
	for _, d := range []driver.AppDevice{
		{ID: "lamp", Name: "lamp", Capabilities: []string{capability.On}},
		{ID: "gone", Name: "gone", Capabilities: []string{capability.On}},
		{ID: "meter", Name: "meter", Capabilities: []string{capability.Battery}},
	} {
		if _, err := known.paired(simInstance, d, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := reg.Report(registry.Report{
		ID: "away", Driver: "other/plugin", Name: "away", Capabilities: []string{capability.On},
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

	m := &matterBridge{log: logging.Discard(), devices: devices, known: known, id: identity{UniqueID: "BRIDGE"}}
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
	// The bridge's own ID is part of it, or two bridges would give the same endpoint the same ID.
	if got, wantID := want[lamp.Endpoint].Unique, matterbridge.UniqueID("BRIDGE/"+strconv.Itoa(int(lamp.Endpoint))); got != wantID {
		t.Errorf("lamp's unique ID = %q, want %q", got, wantID)
	}
}

func TestABridgeTriesAgainUntilItsPortIsFree(t *testing.T) {
	taken, err := net.ListenPacket("udp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	dir := tempDir(t)
	h := &Host{DataDir: dir, Log: logging.Discard(), Matter: &Matter{
		Port: uint16(taken.LocalAddr().(*net.UDPAddr).Port), Retry: 20 * time.Millisecond,
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	matter := func() *api.Matter {
		resp, err := api.Call(ctx, api.Local{DataDir: dir}, api.Request{Type: api.TypeMatter})
		if err != nil {
			return &api.Matter{}
		}
		return resp.Matter
	}
	wait := func(what string, cond func(*api.Matter) bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond(matter()) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, matter())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("it to say why it's down", func(m *api.Matter) bool { return strings.Contains(m.Problem, "in use") })
	if err := taken.Close(); err != nil {
		t.Fatal(err)
	}
	wait("it to take the port", func(m *api.Matter) bool { return m.Serving && m.Problem == "" })
}

func TestClaimsThatCantBeReadRefuseEveryWrite(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	values := data.Namespace(engine.ControlStatesNamespace)
	eng := &engine.Engine{Log: logging.Discard(), Controls: &engine.Stored[engine.ControlState]{Values: values, Log: logging.Discard()}}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	inner := &writes{}
	setter := claimedSetter{engine: eng, setter: inner}
	if err := setter.Set(context.Background(), key("fan"), capability.On, true); err != nil || inner.calls != 1 {
		t.Fatalf("a device with no record: %v, %d writes", err, inner.calls)
	}
	if err := values.Put(key("fan").String(), []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if err := setter.Set(context.Background(), key("fan"), capability.On, true); err == nil || inner.calls != 1 {
		t.Errorf("an unreadable record: %v, %d writes", err, inner.calls)
	}
	if holder, _ := heldAtSafety(eng, devices, key("fan"), capability.On, true); holder != engine.UnreadableClaims {
		t.Errorf("held by %q", holder)
	}
	if err := eng.ForgetDevice(context.Background(), key("fan")); err != nil {
		t.Fatal(err)
	}
	if holder, _ := heldAtSafety(eng, devices, key("fan"), capability.On, true); holder != "" {
		t.Errorf("cleared, and still held by %q", holder)
	}
}
