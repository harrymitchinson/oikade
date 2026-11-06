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
	known := newInventory(reg, devices, eng, logr.Discard())
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
	known := newInventory(reg, devices, eng, logr.Discard())
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
