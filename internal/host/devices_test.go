package host

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/registry"
	"github.com/harrymitchinson/oikade/internal/store"
)

func TestRemovingADeviceEndsItsSavedClaims(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logr.Discard()}
	eng := &engine.Engine{Log: logr.Discard(), Controls: controls}
	devices := hub.NewStore(logr.Discard(), eng)
	eng.Store, eng.Setter = devices, driver.SetterFunc(func(context.Context, types.NamespacedName, string, any) error { return nil })
	eng.Init()
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan struct{})
	go func() { defer close(running); eng.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-running })

	var entries []control.Entry
	raw := `[{"capability":"switch.on","default":true,"resolve":"PreferOff","external":"Override","overrideHold":"1h"}]`
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, eng, logr.Discard())
	known.attach(simInstance)
	dev, err := known.paired(simInstance, driver.AppDevice{ID: "v", Name: "valve", Capabilities: []string{capability.SwitchOn}}, entries)
	if err != nil {
		t.Fatal(err)
	}
	devices.Discovered(dev, driver.Info{Found: true, Capabilities: []string{capability.SwitchOn}})
	claim := control.Claim{Source: "Rule/leak", Level: control.Safety, Expires: time.Now().Add(time.Hour)}
	if _, err := eng.Claim(ctx, dev, capability.SwitchOn, claim); err != nil {
		t.Fatal(err)
	}

	if resp := known.remove(ctx, "valve", false); resp.Error == "" || !resp.NeedsAllowSafety {
		t.Fatalf("removed a device under a Safety claim: %+v", resp)
	}
	if resp := known.remove(ctx, "valve", true); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if _, ok := controls.Get(dev); ok {
		t.Error("its saved claims stayed for the next device of that name")
	}
}

func TestAClaimOnRecordCountsForADeviceWithNoArbiters(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logr.Discard()}
	eng := &engine.Engine{Log: logr.Discard(), Controls: controls}
	devices := hub.NewStore(logr.Discard(), eng)
	eng.Store = devices
	eng.Init()
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, eng, logr.Discard())
	// Its driver hasn't started, so nothing has loaded its arbiters.
	if _, _, err := reg.Report(registry.Report{ID: "v", Driver: "later/x", Name: "valve"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	state := engine.ControlState{capability.SwitchOn: control.State{Claims: []control.Claim{
		{Source: "Rule/leak", Level: control.Safety},
		{Source: "Rule/old", Level: control.Normal, Expires: time.Now().Add(-time.Hour)},
	}}}
	if err := controls.Put(ctx, key("valve"), state); err != nil {
		t.Fatal(err)
	}
	if holders, safety := known.claimed("valve"); !safety || len(holders) != 1 || holders[0] != "Rule/leak" {
		t.Errorf("got %v, %v", holders, safety)
	}
	if resp := known.remove(ctx, "valve", false); !resp.NeedsAllowSafety {
		t.Errorf("got %+v", resp)
	}
	if err := known.rename("valve", "valve-two"); err == nil {
		t.Error("renamed a device with a claim on record")
	}
	if err := known.replace("valve", "valve"); err == nil {
		t.Error("moved a device with a claim on record")
	}
	if _, err := known.restore(registry.Snapshot{Next: 9}, true); err != nil {
		t.Errorf("a file that doesn't name it: %v", err)
	}
	if _, err := known.restore(registry.Snapshot{Next: 9, Devices: []registry.Device{
		{Name: "valve", ID: "other", Driver: "later/x", Endpoint: 8},
	}}, true); err == nil {
		t.Error("gave a claimed name to other hardware")
	}
}

func TestHomeSettingsWinAndSurviveARestart(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	open := func() (*inventory, *hub.Store) {
		devices := hub.NewStore(logr.Discard())
		known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, nil, logr.Discard())
		if err := known.loadHome(); err != nil {
			t.Fatal(err)
		}
		known.attach(simInstance)
		return known, devices
	}
	known, devices := open()
	recorded := control.Entry{Capability: capability.SwitchOn, Default: json.RawMessage("true"), Resolve: control.NamePreferOff, External: control.NameRevert}
	dev, err := known.paired(simInstance, driver.AppDevice{ID: "v", Name: "valve", Capabilities: []string{capability.SwitchOn}}, []control.Entry{recorded})
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(devices *hub.Store) string {
		snap, ok := devices.Get(dev)
		if !ok || len(snap.Control) != 1 {
			t.Fatalf("control %+v", snap.Control)
		}
		return snap.Control[0].Resolve
	}
	if got := resolve(devices); got != control.NamePreferOff {
		t.Fatalf("resolve %s before the home says", got)
	}

	declared := recorded
	declared.Resolve = control.NamePreferOn
	if err := known.setHome([]wire.DeviceControl{{Device: "valve", Entry: declared}}); err != nil {
		t.Fatal(err)
	}
	if got := resolve(devices); got != control.NamePreferOn {
		t.Fatalf("resolve %s, want the home's", got)
	}

	// A host that restarts arbitrates this before any process has attached.
	_, again := open()
	if got := resolve(again); got != control.NamePreferOn {
		t.Fatalf("resolve %s after a restart, want the home's", got)
	}

	// A home that stops saying so leaves what's on record.
	if err := known.setHome(nil); err != nil {
		t.Fatal(err)
	}
	if got := resolve(devices); got != control.NamePreferOff {
		t.Fatalf("resolve %s, want what's on record", got)
	}
}

func TestAProcessCantDropASetting(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), hub.NewStore(logr.Discard()), nil, logr.Discard())
	setting := func(device, resolve string) wire.DeviceControl {
		return wire.DeviceControl{Device: device, Entry: control.Entry{Capability: capability.SwitchOn, Resolve: resolve}}
	}
	if err := known.takeHome([]wire.DeviceControl{setting("a", control.NamePreferOn), setting("b", control.NamePreferOn)}); err != nil {
		t.Fatal(err)
	}
	if err := known.takeHome([]wire.DeviceControl{setting("a", control.NamePreferOff)}); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range known.home {
		got[c.Device] = c.Entry.Resolve
	}
	if got["a"] != control.NamePreferOff || got["b"] != control.NamePreferOn {
		t.Fatalf("after taking: %v", got)
	}
	if err := known.setHome([]wire.DeviceControl{setting("a", control.NamePreferOff)}); err != nil {
		t.Fatal(err)
	}
	if len(known.home) != 1 {
		t.Fatalf("after settling: %v", known.home)
	}
}
