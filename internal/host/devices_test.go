package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
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
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()}
	eng := &engine.Engine{Log: logging.Discard(), Controls: controls}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store, eng.Setter = devices, driver.SetterFunc(func(context.Context, ident.ID, string, any) error { return nil })
	eng.Init()
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan struct{})
	go func() { defer close(running); eng.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-running })

	var entries []control.Entry
	raw := `[{"capability":"on","default":true,"resolve":"PreferOff","external":"Override","overrideHold":"1h"}]`
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, eng, logging.Discard())
	known.attach(simInstance)
	dev, err := known.paired(simInstance, driver.AppDevice{ID: "v", Name: "valve", Capabilities: []string{capability.On}}, entries)
	if err != nil {
		t.Fatal(err)
	}
	devices.Discovered(dev, driver.Info{Found: true, Capabilities: []string{capability.On}})
	claim := control.Claim{Source: "Rule/leak", Level: control.Safety, Expires: time.Now().Add(time.Hour)}
	if _, err := eng.Claim(ctx, dev, capability.On, claim); err != nil {
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
	controls := &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()}
	eng := &engine.Engine{Log: logging.Discard(), Controls: controls}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, eng, logging.Discard())
	// Its driver hasn't started, so nothing has loaded its arbiters.
	if _, _, err := reg.Report(registry.Report{ID: "v", Driver: "later/x", Name: "valve"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{
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
		devices := hub.NewStore(logging.Discard())
		known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, nil, logging.Discard())
		if err := known.loadHome(); err != nil {
			t.Fatal(err)
		}
		known.attach(simInstance)
		return known, devices
	}
	known, devices := open()
	recorded := control.Entry{Capability: capability.On, Default: json.RawMessage("true"), Resolve: control.NamePreferOff, External: control.NameRevert}
	dev, err := known.paired(simInstance, driver.AppDevice{ID: "v", Name: "valve", Capabilities: []string{capability.On}}, []control.Entry{recorded})
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
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), hub.NewStore(logging.Discard()), nil, logging.Discard())
	setting := func(device, resolve string) wire.DeviceControl {
		return wire.DeviceControl{Device: device, Entry: control.Entry{Capability: capability.On, Resolve: resolve}}
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

func TestMovingARoomsLastDeviceWarnsOfTheHoldsThatReadIt(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	eng := &engine.Engine{Log: logging.Discard()}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, eng, logging.Discard())
	known.attach(simInstance)
	for _, d := range []driver.AppDevice{
		{ID: "s", Name: "sensor", Room: "hall", Capabilities: []string{capability.Occupied}},
		{ID: "m", Name: "motion", Room: "landing", Capabilities: []string{capability.Occupied}},
		{ID: "p", Name: "plug", Room: "landing", Capabilities: []string{capability.On}},
	} {
		if _, err := known.paired(simInstance, d, nil); err != nil {
			t.Fatal(err)
		}
	}
	// As the process's RoomReads said: hall-lamp reads the hall, and landing-light the landing's
	// sensors.
	reads := map[string][]engine.RoomRead{
		"hall-lamp":     {{Room: "hall"}},
		"landing-light": {{Room: "landing", Types: []devicetype.Type{devicetype.OccupancySensor}}},
	}
	s := &apiServer{devices: known, srv: &server{session: &session{reads: reads}}}

	resp := s.move("sensor", "attic")
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if !strings.Contains(resp.Warning, "hall-lamp") || !strings.Contains(resp.Warning, "hall") {
		t.Errorf("got warning %q, want one naming hall-lamp and the hall", resp.Warning)
	}
	if d, _ := reg.Get("sensor"); d.Room != "attic" {
		t.Errorf("sensor is in %q: the move should still happen", d.Room)
	}
	// The plug left on the landing isn't one of the types landing-light reads.
	if resp := s.move("motion", "attic"); !strings.Contains(resp.Warning, "landing-light") {
		t.Errorf("got warning %q, want one naming landing-light", resp.Warning)
	}
	// The plug isn't one of the types landing-light reads, so its move empties nothing.
	if resp := s.move("plug", "attic"); resp.Warning != "" {
		t.Errorf("got warning %q for a device no hold reads", resp.Warning)
	}
}

func TestTheAPINamesAPluginAsTheRegistryRecordsIt(t *testing.T) {
	name, ok := api.PluginOf(registry.Device{Driver: instanceName(pluginKey("relays"))})
	if !ok || name != "relays" {
		t.Errorf("got %q, %v", name, ok)
	}
}

func TestRemovingAPluginsDevicesRemovesThemAll(t *testing.T) {
	data, err := store.Open(t.TempDir())
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
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), devices, eng, logging.Discard())
	relays := instanceName(pluginKey("relays"))
	for _, r := range []registry.Report{
		{ID: "a", Driver: relays, Name: "awning"},
		{ID: "b", Driver: relays, Name: "boiler"},
		{ID: "c", Driver: instanceName(pluginKey("shades")), Name: "curtain"},
	} {
		if _, _, err := reg.Report(r); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/frost", Level: control.Safety}}}}
	if err := controls.Put(ctx, key("boiler"), state); err != nil {
		t.Fatal(err)
	}

	resp := known.removePlugin(ctx, "relays", false)
	if !resp.NeedsAllowSafety || !strings.Contains(resp.Error, "boiler") || len(resp.Removed) != 0 {
		t.Fatalf("got %+v", resp)
	}
	resp = known.removePlugin(ctx, "relays", true)
	if resp.Error != "" || strings.Join(resp.Removed, ",") != "awning,boiler" {
		t.Fatalf("got %+v", resp)
	}
	for _, d := range reg.List().Devices {
		if d.Removed != (d.Name != "curtain") {
			t.Errorf("got %+v", d)
		}
	}
	if resp := known.removePlugin(ctx, "relays", false); resp.Error == "" {
		t.Errorf("removed them twice: %+v", resp)
	}
}
