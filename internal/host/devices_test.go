package host

import (
	"context"
	"encoding/json"
	"slices"
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
	raw := `[{"capability":"on","default":true,"external":"Override","overrideHold":"1h"}]`
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), devices, eng, logging.Discard())
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

	if resp, _ := known.remove(ctx, "valve", false); resp.Error == "" || !resp.NeedsAllowSafety {
		t.Fatalf("removed a device under a Safety claim: %+v", resp)
	}
	if resp, _ := known.remove(ctx, "valve", true); resp.Error != "" {
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
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), devices, eng, logging.Discard())
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
	if resp, _ := known.remove(ctx, "valve", false); !resp.NeedsAllowSafety {
		t.Errorf("got %+v", resp)
	}
	if err := known.rename("valve", "valve-two"); err == nil {
		t.Error("renamed a device with a claim on record")
	}
	if err := known.replace("valve", "valve"); err == nil {
		t.Error("moved a device with a claim on record")
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
		known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), devices, nil, logging.Discard())
		if err := known.loadHome(); err != nil {
			t.Fatal(err)
		}
		known.attach(simInstance)
		return known, devices
	}
	known, devices := open()
	recorded := control.Entry{Capability: capability.On, Default: json.RawMessage("true"), External: control.NameRevert}
	dev, err := known.paired(simInstance, driver.AppDevice{ID: "v", Name: "valve", Capabilities: []string{capability.On}}, []control.Entry{recorded})
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(devices *hub.Store) string {
		snap, ok := devices.Get(dev)
		if !ok || len(snap.Control) != 1 {
			t.Fatalf("control %+v", snap.Control)
		}
		return strings.Join(snap.Control[0].Lines, " ")
	}
	if got := resolve(devices); got != "" {
		t.Fatalf("lines %q before the home says", got)
	}

	declared := recorded
	declared.Lines = []string{"Rule/valve on leak"}
	if err := known.setHome([]wire.DeviceControl{{Device: "valve", Entry: declared}}); err != nil {
		t.Fatal(err)
	}
	if got := resolve(devices); got != "Rule/valve on leak" {
		t.Fatalf("lines %q, want the home's", got)
	}

	// A host that restarts arbitrates this before any process has attached.
	_, again := open()
	if got := resolve(again); got != "Rule/valve on leak" {
		t.Fatalf("lines %q after a restart, want the home's", got)
	}

	// A home that stops saying so leaves what's on record.
	if err := known.setHome(nil); err != nil {
		t.Fatal(err)
	}
	if got := resolve(devices); got != "" {
		t.Fatalf("lines %q, want what's on record", got)
	}
}

// A room's setting is each device's there with the capability and no setting of its own, and one
// that moves in with a setting of its own keeps it, which the move warns of.
func TestARoomsSettingReachesTheDevicesThereNow(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	devices := hub.NewStore(logging.Discard())
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), devices, nil, logging.Discard())
	known.attach(simInstance)
	ids := map[string]ident.ID{}
	for _, d := range []driver.AppDevice{
		{ID: "c", Name: "ceiling", Room: "hall", Capabilities: []string{capability.On}},
		{ID: "l", Name: "lamp", Room: "attic", Capabilities: []string{capability.On}},
		{ID: "s", Name: "sensor", Room: "hall", Capabilities: []string{capability.Occupied}},
	} {
		id, err := known.paired(simInstance, d, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids[d.Name] = id
	}
	entry := func(line string) control.Entry {
		return control.Entry{Capability: capability.On, Lines: []string{line}, External: control.NameRevert}
	}
	if err := known.setHome([]wire.DeviceControl{
		{Room: "hall", Entry: entry("Rule/hall on evening")},
		{Device: "lamp", Entry: entry("Rule/lamp on reading")},
	}); err != nil {
		t.Fatal(err)
	}
	lines := func(name string) string {
		snap, _ := devices.Get(ids[name])
		var out []string
		for _, c := range snap.Control {
			out = append(out, c.Lines...)
		}
		return strings.Join(out, " ")
	}
	if got := lines("ceiling"); got != "Rule/hall on evening" {
		t.Errorf("ceiling's lines are %q, want the hall's", got)
	}
	if got := lines("sensor"); got != "" {
		t.Errorf("sensor, which has no on, has lines %q", got)
	}

	s := &apiServer{devices: known, srv: &server{}}
	resp := s.move("lamp", "hall")
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if got := lines("lamp"); got != "Rule/lamp on reading" {
		t.Errorf("lamp's lines are %q, want its own", got)
	}
	if want := "lamp keeps its own decision on on rather than hall's"; !strings.Contains(resp.Warning, want) {
		t.Errorf("got warning %q, want %q", resp.Warning, want)
	}
	if resp := s.move("ceiling", "attic"); resp.Warning != "" || lines("ceiling") != "" {
		t.Errorf("ceiling, out of the hall, warns %q and has lines %q", resp.Warning, lines("ceiling"))
	}

	// A lamp that hands over to the hall's decision with Then has the hall's lines below its own.
	handed := entry("Rule/lamp on reading")
	handed.Lines = append(handed.Lines, "Rule/hall on evening")
	if err := known.setHome([]wire.DeviceControl{
		{Room: "hall", Entry: entry("Rule/hall on evening")}, {Device: "lamp", Entry: handed},
	}); err != nil {
		t.Fatal(err)
	}
	if resp := s.move("lamp", "attic"); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if resp := s.move("lamp", "hall"); resp.Warning != "" {
		t.Errorf("a lamp that hands over warns %q", resp.Warning)
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
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), hub.NewStore(logging.Discard()), nil, logging.Discard())
	setting := func(device, line string) wire.DeviceControl {
		return wire.DeviceControl{Device: device, Entry: control.Entry{Capability: capability.On, Lines: []string{line}}}
	}
	if err := known.takeHome([]wire.DeviceControl{setting("a", "Rule/first"), setting("b", "Rule/first")}); err != nil {
		t.Fatal(err)
	}
	if err := known.takeHome([]wire.DeviceControl{setting("a", "Rule/second")}); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range known.home {
		got[c.Device] = c.Entry.Lines[0]
	}
	if got["a"] != "Rule/second" || got["b"] != "Rule/first" {
		t.Fatalf("after taking: %v", got)
	}
	if err := known.setHome([]wire.DeviceControl{setting("a", "Rule/second")}); err != nil {
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
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), devices, eng, logging.Discard())
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

func TestRetypingOrRemovingARoomsLastDeviceWarnsOfTheHoldsThatReadIt(t *testing.T) {
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
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), devices, eng, logging.Discard())
	known.attach(simInstance)
	for _, d := range []driver.AppDevice{
		{ID: "k", Name: "kettle", Room: "landing", Capabilities: []string{capability.On}},
		{ID: "p", Name: "plug", Room: "landing", Capabilities: []string{capability.On}},
		{ID: "s", Name: "sensor", Room: "hall", Capabilities: []string{capability.Occupied}},
	} {
		if _, err := known.paired(simInstance, d, nil); err != nil {
			t.Fatal(err)
		}
	}
	relays := pluginKey("relays")
	known.attach(relays)
	if _, err := known.paired(relays, driver.AppDevice{ID: "b", Name: "bulb", Room: "study", Capabilities: []string{capability.On}}, nil); err != nil {
		t.Fatal(err)
	}
	reads := map[string][]engine.RoomRead{
		"hall-lamp":     {{Room: "hall"}},
		"landing-plugs": {{Room: "landing", Types: []devicetype.Type{devicetype.OnOffPlugInUnit}}},
		"study-fan":     {{Room: "study"}},
	}
	s := &apiServer{ctx: context.Background(), devices: known, srv: &server{session: &session{reads: reads}}}

	// The kettle is still a plug on the landing, so retyping the plug as a light empties nothing.
	if resp := s.retype("plug", devicetype.OnOffLight); resp.Error != "" || resp.Warning != "" {
		t.Errorf("got %+v, want no warning while the kettle is left", resp)
	}
	resp := s.retype("kettle", devicetype.OnOffLight)
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if !strings.Contains(resp.Warning, "landing-plugs") || !strings.Contains(resp.Warning, "landing") {
		t.Errorf("got warning %q, want one naming landing-plugs and the landing", resp.Warning)
	}
	if d, _ := reg.Get("kettle"); d.DeviceType != devicetype.OnOffLight {
		t.Errorf("kettle is %q: the type should still change", d.DeviceType)
	}
	// The landing's read was empty already, so going back to what the driver says only fills it.
	if resp := s.retype("plug", ""); resp.Warning != "" {
		t.Errorf("got warning %q for a retype into the read", resp.Warning)
	}

	resp = s.remove(api.Request{Type: api.TypeDeviceRemove, Device: "sensor"})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if !strings.Contains(resp.Warning, "hall-lamp") || !strings.Contains(resp.Warning, "hall") {
		t.Errorf("got warning %q, want one naming hall-lamp and the hall", resp.Warning)
	}
	if _, ok := reg.Get("sensor"); ok {
		t.Error("sensor is still on record: the remove should still happen")
	}

	// A refused remove empties nothing.
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/study-fan", Level: control.Safety}}}}
	if err := controls.Put(context.Background(), key("bulb"), state); err != nil {
		t.Fatal(err)
	}
	if resp := s.remove(api.Request{Type: api.TypeDeviceRemove, Device: "bulb"}); !resp.NeedsAllowSafety || resp.Warning != "" {
		t.Errorf("got %+v, want a refusal with no warning", resp)
	}
	resp = s.remove(api.Request{Type: api.TypeDeviceRemove, Plugin: "relays", AllowSafety: true})
	if !strings.Contains(resp.Warning, "study-fan") || strings.Contains(resp.Warning, "hall-lamp") {
		t.Errorf("got %+v, want a warning naming study-fan, and not the hall emptied before", resp)
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
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), devices, eng, logging.Discard())
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

	resp, _ := known.removePlugin(ctx, "relays", false)
	if !resp.NeedsAllowSafety || !strings.Contains(resp.Error, "boiler") || len(resp.Removed) != 0 {
		t.Fatalf("got %+v", resp)
	}
	resp, _ = known.removePlugin(ctx, "relays", true)
	if resp.Error != "" || strings.Join(resp.Removed, ",") != "awning,boiler" {
		t.Fatalf("got %+v", resp)
	}
	for _, d := range reg.List().Devices {
		if d.Removed != (d.Name != "curtain") {
			t.Errorf("got %+v", d)
		}
	}
	if resp, _ := known.removePlugin(ctx, "relays", false); resp.Error == "" {
		t.Errorf("removed them twice: %+v", resp)
	}
}

// A retype that takes a device out of a room's Safety case is named, and one the case leaves out,
// as it has a decision of its own, isn't.
func TestRetypingOutOfARoomsSafetyCaseNamesIt(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	known := newInventory(reg, data.Namespace(homeSettingsNamespace), data.Namespace(declaredNamespace), hub.NewStore(logging.Discard()), nil, logging.Discard())
	known.attach(simInstance)
	for _, d := range []driver.AppDevice{
		{ID: "c", Name: "ceiling", Room: "hall", Capabilities: []string{capability.On}, DeviceType: devicetype.OnOffLight},
		{ID: "l", Name: "lamp", Room: "hall", Capabilities: []string{capability.On}, DeviceType: devicetype.OnOffLight},
	} {
		if _, err := known.paired(simInstance, d, nil); err != nil {
			t.Fatal(err)
		}
	}
	rules := []wire.RuleInfo{{Name: "hall on smoke", Kind: wire.KindCase, Safety: true, Rooms: []wire.RoomOfTypes{{
		Room: "hall", Capability: capability.On, DeviceTypes: []devicetype.Type{devicetype.OnOffLight}, Except: []string{"lamp"},
	}}}}
	s := &apiServer{devices: known, srv: &server{session: &session{rules: rules}}}
	if out, _ := s.typedOut("ceiling", devicetype.OnOffPlugInUnit); !slices.Equal(out, []string{"hall on smoke"}) {
		t.Errorf("retyping ceiling names %v", out)
	}
	if out, _ := s.typedOut("lamp", devicetype.OnOffPlugInUnit); len(out) != 0 {
		t.Errorf("retyping lamp, which the case leaves out, names %v", out)
	}
}
