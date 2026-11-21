package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
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
	ctx, cancel := context.WithCancel(t.Context())
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

	if resp, _ := known.forget(ctx, []string{"valve"}, false, nil); resp.Error == "" || !resp.NeedsAllowSafety {
		t.Fatalf("removed a device under a Safety claim: %+v", resp)
	}
	if resp, _ := known.forget(ctx, []string{"valve"}, true, nil); resp.Error != "" {
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
	ctx := t.Context()
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{
		{Source: "Rule/leak", Level: control.Safety},
		{Source: "Rule/old", Level: control.Normal, Expires: time.Now().Add(-time.Hour)},
	}}}
	if err := controls.Put(ctx, key("valve"), state); err != nil {
		t.Fatal(err)
	}
	if holders, safety := known.claimed("valve"); !safety || len(holders) != 1 || holders[0] != "flow/leak" {
		t.Errorf("got %v, %v", holders, safety)
	}
	if resp, _ := known.forget(ctx, []string{"valve"}, false, nil); !resp.NeedsAllowSafety {
		t.Errorf("got %+v", resp)
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
// that moves in with a setting of its own keeps it.
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
	known.attach(pluginKey("x"))
	ids := map[string]ident.ID{}
	for _, d := range []driver.AppDevice{
		{ID: "c", Name: "ceiling", Room: "hall", Capabilities: []string{capability.On}},
		{ID: "l", Name: "lamp", Room: "attic", Capabilities: []string{capability.On}},
		{ID: "s", Name: "sensor", Room: "hall", Capabilities: []string{capability.Occupied}},
	} {
		id, err := known.paired(pluginKey("x"), d, nil)
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

	// Declared rooms move devices, as a deploy does.
	declare := func(rooms map[string]string) {
		t.Helper()
		decls := make([]wire.DeviceDeclaration, 0, len(rooms))
		for name, room := range rooms {
			decls = append(decls, wire.DeviceDeclaration{Name: name, Plugin: "x", ID: name[:1], Room: room})
		}
		if err := known.takeDeclared(decls, nil); err != nil {
			t.Fatal(err)
		}
	}
	declare(map[string]string{"lamp": "hall"})
	if got := lines("lamp"); got != "Rule/lamp on reading" {
		t.Errorf("lamp's lines are %q, want its own", got)
	}
	declare(map[string]string{"lamp": "hall", "ceiling": "attic"})
	if got := lines("ceiling"); got != "" {
		t.Errorf("ceiling, out of the hall, has lines %q", got)
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

func TestRemovingARoomsLastDeviceWarnsOfTheHoldsThatReadIt(t *testing.T) {
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
	s := &apiServer{ctx: t.Context(), devices: known, srv: &server{session: &session{reads: reads}}}

	resp := s.forget(api.Request{Type: api.TypeForget, Devices: []string{"sensor"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if !strings.Contains(resp.Warning, "hall-lamp") || !strings.Contains(resp.Warning, "hall") {
		t.Errorf("got warning %q, want one naming hall-lamp and the hall", resp.Warning)
	}
	if _, ok := reg.Get("sensor"); ok {
		t.Error("sensor is still on record: the forget should still happen")
	}

	// A refused forget empties nothing.
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/study-fan", Level: control.Safety}}}}
	if err := controls.Put(t.Context(), key("bulb"), state); err != nil {
		t.Fatal(err)
	}
	if resp := s.forget(api.Request{Type: api.TypeForget, Devices: []string{"bulb"}}); !resp.NeedsAllowSafety || resp.Warning != "" {
		t.Errorf("got %+v, want a refusal with no warning", resp)
	}
	resp = s.forget(api.Request{Type: api.TypeForget, Plugin: "relays", AllowSafety: true})
	if !strings.Contains(resp.Warning, "study-fan") || strings.Contains(resp.Warning, "hall-lamp") {
		t.Errorf("got %+v, want a warning naming study-fan, and not the hall emptied before", resp)
	}
}

func TestTheAPINamesAPluginAsTheRegistryRecordsIt(t *testing.T) {
	name, ok := api.PluginOf(registry.Device{Driver: instanceName(pluginKey("relays"))})
	if !ok || name != "relays" {
		t.Errorf("got %q, %v", name, ok)
	}
	// A device no plugin reports, such as a simulated one, has none, not its driver's name.
	if name, ok := api.PluginOf(registry.Device{Driver: instanceName(simInstance)}); ok || name != "" {
		t.Errorf("a simulated device's plugin: %q, %v", name, ok)
	}
}

func TestForgettingAPluginsDevicesForgetsThemAll(t *testing.T) {
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
	ctx := t.Context()
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/frost", Level: control.Safety}}}}
	if err := controls.Put(ctx, key("boiler"), state); err != nil {
		t.Fatal(err)
	}

	resp, _ := known.forgetPlugin(ctx, "relays", false, nil)
	if !resp.NeedsAllowSafety || !strings.Contains(resp.Error, "boiler") || len(resp.Forgotten) != 0 {
		t.Fatalf("got %+v", resp)
	}
	resp, _ = known.forgetPlugin(ctx, "relays", true, nil)
	// What held the boiler is what its forget ended.
	if resp.Error != "" || len(resp.Forgotten) != 2 || resp.Forgotten[0].Device != "awning" ||
		resp.Forgotten[1].Device != "boiler" || len(resp.Forgotten[1].Ended) != 1 ||
		!strings.HasSuffix(resp.Forgotten[1].Ended[0], "/frost") {
		t.Fatalf("got %+v", resp)
	}
	for _, d := range reg.List().Devices {
		if d.Removed != (d.Name != "curtain") {
			t.Errorf("got %+v", d)
		}
	}
	if resp, _ := known.forgetPlugin(ctx, "relays", false, nil); resp.Error == "" {
		t.Errorf("removed them twice: %+v", resp)
	}
}

// A declared room or type that takes a device out of a room's Safety case is named, and one the
// case leaves out, as it has a decision of its own, or that the next version's case still covers,
// isn't.
func TestADeclaredTypeOutOfARoomsSafetyCaseNamesIt(t *testing.T) {
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
	known.attach(pluginKey("x"))
	for _, d := range []driver.AppDevice{
		{ID: "c", Name: "ceiling", Room: "hall", Capabilities: []string{capability.On}, DeviceType: devicetype.OnOffLight},
		{ID: "l", Name: "lamp", Room: "hall", Capabilities: []string{capability.On}, DeviceType: devicetype.OnOffLight},
	} {
		if _, err := known.paired(pluginKey("x"), d, nil); err != nil {
			t.Fatal(err)
		}
	}
	rules := []wire.RuleInfo{{Name: "hall on smoke", Kind: wire.KindCase, Safety: true, Rooms: []wire.RoomOfTypes{{
		Room: "hall", Capability: capability.On, DeviceTypes: []devicetype.Type{devicetype.OnOffLight}, Except: []string{"lamp"},
	}}}}
	as := func(name, room string, t devicetype.Type) []wire.DeviceDeclaration {
		return []wire.DeviceDeclaration{{Name: name, Plugin: "x", ID: name[:1], Room: room, DeviceType: t}}
	}
	if out := known.leavingSafety(as("ceiling", "hall", devicetype.OnOffPlugInUnit), rules, rules); len(out) != 1 ||
		!strings.Contains(out[0], "ceiling would leave the devices hall on smoke writes at Safety") {
		t.Errorf("declaring ceiling a plug names %q", out)
	}
	if out := known.leavingSafety(as("ceiling", "attic", ""), rules, rules); len(out) != 1 {
		t.Errorf("declaring ceiling in the attic names %q", out)
	}
	if out := known.leavingSafety(as("lamp", "hall", devicetype.OnOffPlugInUnit), rules, rules); len(out) != 0 {
		t.Errorf("declaring lamp a plug, which the case leaves out, names %v", out)
	}
	if out := known.leavingSafety(as("ceiling", "hall", devicetype.OnOffLight), rules, rules); len(out) != 0 {
		t.Errorf("declaring ceiling what it is names %v", out)
	}
	// The next version's case takes plugs too, so a plug is still covered.
	widened := []wire.RuleInfo{rules[0]}
	widened[0].Rooms = []wire.RoomOfTypes{{Room: "hall", Capability: capability.On,
		DeviceTypes: []devicetype.Type{devicetype.OnOffLight, devicetype.OnOffPlugInUnit}, Except: []string{"lamp"}}}
	if out := known.leavingSafety(as("ceiling", "hall", devicetype.OnOffPlugInUnit), rules, widened); len(out) != 0 {
		t.Errorf("declaring ceiling a plug, which the next case covers, names %v", out)
	}
	// One the next version drops is its own finding.
	if out := known.leavingSafety(as("ceiling", "attic", ""), rules, nil); len(out) != 0 {
		t.Errorf("a dropped case names %v", out)
	}
	// A room write of any type covers a move.
	anyType := []wire.RuleInfo{{Name: "hall off", Kind: wire.KindCase, Safety: true, Rooms: []wire.RoomOfTypes{{
		Room: "hall", Capability: capability.On,
	}}}}
	if out := known.leavingSafety(as("lamp", "attic", ""), anyType, anyType); len(out) != 1 {
		t.Errorf("moving lamp out of a room written at Safety names %v", out)
	}
}

// A forget checks every device before it changes anything, and says what became of each: the claims
// it ended, and whether your home imports its plugin. A plugin's device doesn't come back at its next
// report, as a simulated one does.
func TestAForgetChecksEveryDeviceAndSaysWhatBecameOfIt(t *testing.T) {
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
	for _, r := range []registry.Report{
		{ID: "a", Driver: instanceName(pluginKey("relays")), Name: "awning"},
		{ID: "b", Driver: instanceName(pluginKey("relays")), Name: "boiler"},
		{ID: "c", Driver: instanceName(pluginKey("shades")), Name: "curtain"},
	} {
		if _, _, err := reg.Report(r); err != nil {
			t.Fatal(err)
		}
	}
	state := engine.ControlState{capability.On: control.State{Claims: []control.Claim{{Source: "Rule/frost", Level: control.Safety}}}}
	if err := controls.Put(t.Context(), key("boiler"), state); err != nil {
		t.Fatal(err)
	}
	imports := &plugins{host: &Host{}, known: known, imported: []string{"relays"}, importsKnown: true}
	s := &apiServer{ctx: t.Context(), devices: known, srv: &server{plugins: imports}}

	resp := s.forget(api.Request{Type: api.TypeForget, Devices: []string{"awning", "nope"}})
	if !strings.Contains(resp.Error, "device/nope") || len(resp.Forgotten) != 0 {
		t.Errorf("forgetting awning and nope: %+v", resp)
	}
	if d, _ := reg.Get("awning"); d.Removed {
		t.Error("awning was forgotten, though nope stopped the forget")
	}
	if resp := s.forget(api.Request{Type: api.TypeForget, Devices: []string{"awning", "awning"}}); resp.Error != "" ||
		len(resp.Forgotten) != 1 {
		t.Errorf("forgetting awning twice over: %+v", resp)
	}

	resp = s.forget(api.Request{Type: api.TypeForget, Devices: []string{"boiler", "curtain"}, AllowSafety: true})
	want := []api.Forgotten{
		{Device: "boiler", Ref: "device/boiler", Plugin: "relays", Ended: []string{control.RuleRef("frost").String()}},
		{Device: "curtain", Ref: "device/curtain", Plugin: "shades", Unimported: true},
	}
	if resp.Error != "" || fmt.Sprintf("%+v", resp.Forgotten) != fmt.Sprintf("%+v", want) {
		t.Errorf("got %+v, %q\nwant %+v", resp.Forgotten, resp.Error, want)
	}

	if resp := s.forget(api.Request{Type: api.TypeForget, Devices: []string{"boiler"}}); resp.Error != "device/boiler is forgotten already" {
		t.Errorf("forgetting boiler again: %+v", resp)
	}
	if resp := s.forget(api.Request{Type: api.TypeForget}); resp.Error == "" {
		t.Error("a forget of nothing went ahead")
	}
}
