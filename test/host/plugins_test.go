package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"oikade.dev/contract/v1"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/matter/nodetest"
	"oikade.dev/internal/mqtt/mqtttest"
	"oikade.dev/sdk/plugin"
)

const (
	pluginConnected = "Connected plugin"
	pluginStarted   = "Started plugin process"
	pluginRestarted = "Restarting plugin process"
	pluginKilled    = "Killing plugin process; it stopped answering"
	rejectedMessage = "Rejected plugin message"
)

// pluginDevices writes the simulated devices for a home with plugins: the usual ones but the valve,
// which the relays plugin has, and a sensor and two switches the plugins variant's rules use.
func pluginDevices(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "devices-novalve.json"))
	if err != nil {
		t.Fatal(err)
	}
	devices := make([]map[string]any, 0, 3)
	if err := json.Unmarshal(raw, &devices); err != nil {
		t.Fatal(err)
	}
	devices = append(devices, map[string]any{"name": "once-sensor", "capabilities": []string{opened}})
	for _, name := range []string{"quiet-held", "quiet-heard"} {
		devices = append(devices, map[string]any{"name": name, "capabilities": []string{"on"}, "values": map[string]any{"on": false}})
	}
	out, err := json.Marshal(devices)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(file, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// startPlugins starts a host on a home that imports plugins, with both of them connected.
func startPlugins(t *testing.T, args ...string) *rig {
	t.Helper()
	r := preparePlugins(t)
	r.launchedPlugins(args...)
	return r
}

// preparePlugins is prepare, for a host that runs the plugins.
func preparePlugins(t *testing.T) *rig {
	t.Helper()
	r := prepare(t)
	r.env = append(r.env, "OIKADE_TEST_PLUGIN_DOWN="+filepath.Join(r.dir, "plugin-down"))
	return r
}

// launchedPlugins launches a host that runs the plugins, and waits for its process and both plugins
// to connect.
func (r *rig) launchedPlugins(args ...string) {
	r.t.Helper()
	connected := r.count(pluginConnected)
	r.launchPlugins(args...)
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.until("both plugins", func() bool { return r.count(pluginConnected) >= connected+2 })
}

func (r *rig) launchPlugins(args ...string) {
	r.t.Helper()
	r.launchWithout(append([]string{"--home", variant(r.t, "plugins"), "--simulate", pluginDevices(r.t)}, args...)...)
}

// setting writes to a device until the write is taken: a plugin that has just connected is yet to
// say it's online.
func (r *rig) setting(device, value string) {
	r.t.Helper()
	r.until("a write to "+device, func() bool {
		_, err := r.oikade(nil, "set", "device/"+device+"/on", value)
		return err == nil
	})
}

func (r *rig) value(device string) any {
	r.t.Helper()
	return onOf(r.t, r.status(), device).Value
}

func (r *rig) untilValue(device string, want any) {
	r.t.Helper()
	r.until(fmt.Sprintf("%s to be %v", device, want), func() bool {
		for _, d := range r.status().Devices {
			if d.Name != device {
				continue
			}
			for _, c := range d.Capabilities {
				if c.Name == "on" {
					return c.Value == want
				}
			}
		}
		return false
	})
}

func TestAnImportedPluginsDevicesAreUsableAtOnce(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.untilValue("relay", false)
	r.stdout(nil, "set", "device/relay/on", "on")
	r.untilValue("relay", true)
	if d := deviceOf(t, r.devices(), "relay"); d.Driver != "plugin/relays" || d.Endpoint == 0 ||
		d.Manufacturer != "Oikade" || d.Model != "Simulated switch" || !strings.HasPrefix(d.SerialNumber, "SIM-") ||
		d.FirmwareVersion != "1.0.0" {
		t.Fatalf("got %+v", d)
	}
}

func TestADeviceSaysWhatItsPluginSaysIsWrong(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	const problem = "no key: give this relay one"
	r.until("the device's problem", func() bool { return strings.Contains(string(r.stdout(nil, "status")), problem) })
	// What's wrong now is status's, and the list is the record's.
	if listed := string(r.stdout(nil, "list", "devices", "--json")); strings.Contains(listed, problem) {
		t.Errorf("list devices --json: %s", listed)
	}
	if status := string(r.stdout(nil, "status", "locked")); !strings.Contains(status, problem) {
		t.Errorf("status: %s", status)
	}
}

func TestAPluginSaysWhatsWrongWithItUntilItsFixed(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	const problem = "Plugin quiet: its simulated account refused the login"
	r.until("the plugin's problem", func() bool {
		stderr, err := r.oikade(nil, "status")
		return err == nil && strings.Contains(stderr, problem)
	})
	if got := withProblems(r.status()); len(got) != 1 || got[0].Name != "quiet" {
		t.Errorf("status --json: plugins with a problem %+v", got)
	}

	if err := os.WriteFile(filepath.Join(r.dir, "plugin-down.fixed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.until("the problem to clear", func() bool {
		stderr, err := r.oikade(nil, "status")
		return err == nil && !strings.Contains(stderr, problem)
	})
	if got := withProblems(r.status()); got != nil {
		t.Errorf("status --json once fixed: plugins with a problem %+v", got)
	}
}

func TestADeviceYouRemoveStaysRemovedWhileItsPluginLists(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.untilValue("relay", false)
	r.stdout(nil, "forget", "relay", "--allow-safety")
	// Closed now, so that the valve opening again shows the restarted plugin has published every
	// relay: it opens it last.
	r.setting("valve", "off")
	r.untilValue("valve", false)
	rejected := r.count(rejectedMessage, "reason", "undeclared_device")

	// A restart is the plugin listing everything again.
	connected := r.count(pluginConnected)
	r.oikade(nil, "set", "device/panics/on", "on") //nolint:errcheck // It dies taking this.
	r.until("the plugin to come back", func() bool { return r.count(pluginConnected) > connected })
	r.untilValue("panics", false)
	r.untilValue("valve", true)
	for _, d := range r.devices().Devices {
		if d.ID == "relay" && !d.Removed {
			t.Fatalf("it came back: %+v", d)
		}
	}
	// Nor is what the plugin still publishes for it counted against the plugin. The host logs
	// a rejection before it takes in the valve, so its line is read by the time sync's is.
	r.sync()
	if got := r.count(rejectedMessage, "reason", "undeclared_device"); got != rejected {
		t.Errorf("the removed relay's messages were rejected as undeclared. The log:\n%s", r.dump())
	}
}

func TestEachPluginIsItsOwnProcess(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	pids := map[any]bool{float64(r.pid()): true}
	r.mu.Lock()
	for _, l := range r.lines {
		if l.is(pluginStarted) {
			pids[l["pid"]] = true
		}
	}
	r.mu.Unlock()
	if len(pids) != 3 {
		t.Fatalf("got processes %v. The log:\n%s", pids, r.dump())
	}
}

func TestAPluginThatPanicsIsRestartedWhileTheRestCarriesOn(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.untilValue("relay", false)
	r.stdout(nil, "set", "device/relay/on", "on")
	r.untilValue("relay", true)
	automations := r.pid()
	r.stdout(nil, "set", "device/panics/on", "on")
	r.waitFor(func(l line) bool { return l.is(pluginRestarted, "plugin", "relays") })
	r.until("the plugin to come back", func() bool { return r.count(pluginConnected, "plugin", "relays") == 2 })
	if r.pid() != automations || r.count(attached) != 1 || r.count(pluginConnected, "plugin", "quiet") != 1 {
		t.Fatalf("it reached further than the plugin. The log:\n%s", r.dump())
	}
	// A relay starts off, as a device that lost power does.
	r.untilValue("relay", false)
	r.setting("relay", "on")
	r.untilValue("relay", true)
	r.sync()
}

func TestAPluginThatHangsIsKilledAndRestarted(t *testing.T) {
	t.Parallel()
	r := startPlugins(t, "--heartbeat-timeout=1s", "--plugin-set-timeout=100ms")
	r.untilValue("relay", false)
	automations := r.pid()
	r.stdout(nil, "set", "device/hangs/on", "on")
	r.waitFor(func(l line) bool { return l.is(pluginKilled, "plugin", "relays") })
	r.until("the plugin to come back", func() bool { return r.count(pluginConnected, "plugin", "relays") == 2 })
	if r.pid() != automations {
		t.Fatalf("automations went too. The log:\n%s", r.dump())
	}
	r.setting("relay", "on")
	r.untilValue("relay", true)
}

func TestASafetyClaimOutlastsItsPlugin(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.untilValue("valve", true)
	r.change("leak-sensor", opened, true)
	r.untilValue("valve", false)

	// Its valve starts open, so coming back is a change the claim has to undo.
	const changed = "Changed capability"
	before := r.count(changed, "device", "valve")
	r.stdout(nil, "set", "device/panics/on", "on")
	r.waitFor(func(l line) bool { return l.is(pluginRestarted, "plugin", "relays") })
	r.until("the plugin to come back", func() bool { return r.count(pluginConnected, "plugin", "relays") == 2 })
	r.until("the valve to be seen open and shut again", func() bool {
		return r.count(changed, "device", "valve") >= before+2 && r.value("valve") == false
	})
	st := onOf(t, r.status(), "valve")
	if st.Level != "Safety" {
		t.Fatalf("got %+v. The log:\n%s", st, r.dump())
	}
	if _, err := r.oikade(nil, "set", "device/valve/on", "on"); err == nil {
		t.Fatal("a write beat the Safety claim")
	}
}

func TestAWriteToADownPluginFailsOutLoud(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.untilValue("relay", false)
	if err := os.WriteFile(filepath.Join(r.dir, "plugin-down"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.stdout(nil, "set", "device/panics/on", "on")
	r.waitFor(func(l line) bool { return l.is(pluginRestarted, "plugin", "relays") })
	stderr, err := r.oikade(nil, "set", "device/relay/on", "on")
	if err == nil || !strings.Contains(stderr, "offline") {
		t.Fatalf("got %v, %q", err, stderr)
	}
	if err := os.Remove(filepath.Join(r.dir, "plugin-down")); err != nil {
		t.Fatal(err)
	}
	// A process told to stay down still connects before it gives up, so counting connections
	// can't say it's back. A write it takes can.
	r.setting("relay", "on")
	r.untilValue("relay", true)
}

func TestTheDeployCheckSeesAClaimOnADeviceWhosePluginIsDown(t *testing.T) {
	t.Parallel()
	r := startPlugins(t, settle)
	r.untilValue("valve", true)
	r.change("leak-sensor", opened, true)
	r.untilValue("valve", false)
	r.sync()
	r.stop()
	if err := os.WriteFile(filepath.Join(r.dir, "plugin-down"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.launchPlugins(settle)
	r.waitFor(func(l line) bool { return l.is(attached) })
	stderr, err := r.deploy("noleak")
	if err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	if !strings.Contains(stderr, `"valve on leak-shutoff"`) || !strings.Contains(stderr, "--allow-safety") {
		t.Fatalf("got %q", stderr)
	}
}

func TestAControllerDrivesAPluginsDevice(t *testing.T) {
	t.Parallel()
	r := preparePlugins(t)
	port := r.serveMatter(freeUDP(t), func(flag string) { r.launchedPlugins(flag) })
	r.untilValue("relay", false)
	r.until("the commissioning window", func() bool { return r.matter().Window })
	c := &controller{t: t, r: r, port: port, cm: nodetest.New(t)}
	c.admin = c.cm.Commission(nodetest.Addr(port), r.matter().Passcode)
	endpoint := c.endpoint("relay")
	if vendor, product := c.info(endpoint, 0x01), c.info(endpoint, 0x03); vendor != "Oikade" || product != "Simulated switch" {
		t.Errorf("VendorName %v and ProductName %v, want what the plugin says", vendor, product)
	}
	relay := deviceOf(t, r.devices(), "relay")
	if serial, firmware := c.info(endpoint, 0x0F), c.info(endpoint, 0x0A); serial != "SIM-"+strings.ToUpper(relay.ID) ||
		firmware != "1.0.0" {
		t.Errorf("SerialNumber %v and SoftwareVersionString %v, want what the plugin says", serial, firmware)
	}
	c.command(endpoint, turnOn)
	r.untilValue("relay", true)
	r.stdout(nil, "set", "device/relay/on", "off")
	r.until("the controller to hear relay", func() bool { return !c.isOn(endpoint) })
}

func TestAHomeKeepsAPluginsDevicesOffMatter(t *testing.T) {
	t.Parallel()
	r := preparePlugins(t)
	port := r.serveMatter(freeUDP(t), func(flag string) { r.launchedPlugins(flag) })
	r.untilValue("relay", false)
	r.until("the commissioning window", func() bool { return r.matter().Window })
	c := &controller{t: t, r: r, port: port, cm: nodetest.New(t)}
	c.admin = c.cm.Commission(nodetest.Addr(port), r.matter().Passcode)
	relay, valve := c.endpoint("relay"), c.endpoint("valve")
	r.until("the relay on the bridge", func() bool { return len(c.deviceTypes(relay)) > 0 })

	stderr, err := r.deploy("plugins-hidden")
	if err != nil {
		t.Fatalf("%v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
	// The plugin may have reported spare-relay by now, which would be in the list too.
	if !strings.Contains(stderr, "Took hangs, once, panics, relay") || !strings.Contains(stderr, "off Matter") {
		t.Errorf("the deploy said:\n%s", stderr)
	}
	r.until("the relay off the bridge", func() bool { return len(c.deviceTypes(relay)) == 0 })
	if len(c.deviceTypes(valve)) == 0 {
		t.Error("ShowOnMatter's valve went too")
	}
	// The host still drives it.
	r.setting("relay", "on")
	r.untilValue("relay", true)
	var listed []struct {
		Name   string `json:"name"`
		Driver string `json:"driver"`
		Matter *bool  `json:"matter"`
	}
	if err := json.Unmarshal(r.stdout(nil, "list", "devices", "--json"), &listed); err != nil {
		t.Fatal(err)
	}
	for _, d := range listed {
		// Each plugin reports a spare relay, so which is whose is by driver.
		hidden := d.Driver == "plugin/relays" && d.Name != "valve"
		if hidden != (d.Matter != nil && !*d.Matter) {
			t.Errorf("%s: matter is %v", d.Name, d.Matter)
		}
	}

	// A host that restarts keeps it off.
	r.stop()
	c.relaunch(func(flag string) {
		r.launchPlugins(flag)
		r.waitFor(func(l line) bool { return l.is(attached) })
	})
	if len(c.deviceTypes(relay)) != 0 {
		t.Error("after a restart, the relay is on the bridge")
	}

	// Shown again, it's the same device to a controller.
	stderr, err = r.deploy("plugins")
	if err != nil {
		t.Fatalf("%v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
	r.until("the relay back on the bridge", func() bool { return len(c.deviceTypes(relay)) > 0 })
	if got := c.endpoint("relay"); got != relay {
		t.Errorf("the relay came back on endpoint %d, not %d", got, relay)
	}
	if strings.Contains(stderr, "off Matter") {
		t.Errorf("the deploy said:\n%s", stderr)
	}
}

func TestAFailedDeployLeavesThePluginsDevicesOnMatter(t *testing.T) {
	t.Parallel()
	port := freeUDP(t)
	r := prepare(t)
	r.env = append(r.env, "OIKADE_TEST_PLUGIN_DOWN="+filepath.Join(r.dir, "plugin-down"),
		"OIKADE_TEST_CRASH="+filepath.Join(r.dir, "crash"))
	r.launchPlugins(matterFlag(port), longSettle)
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.until("the relay", func() bool { return r.count(pluginConnected, "plugin", "relays") >= 1 })
	r.untilValue("relay", false)

	bin := variant(t, "plugins-hidden-crash")
	done := make(chan error, 1)
	go func() {
		_, err := r.oikade(nil, "deploy", "--binary", bin)
		done <- err
	}()
	r.until("the new version to connect", func() bool { return r.count(attached) >= 2 })
	r.sync()
	if err := os.WriteFile(filepath.Join(r.dir, "crash"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatalf("the deploy went ahead. The log:\n%s", r.dump())
	}
	r.waitFor(func(l line) bool { return l.is(restored) })
	r.sync()
	// Off and back on, a controller would have lost the devices' rooms and scenes.
	if n := r.count("Removed bridged device"); n != 0 {
		t.Errorf("a version that never settled took %d devices off the bridge. The log:\n%s", n, r.dump())
	}
	if got := deviceOf(t, r.devices(), "relay"); got.ID == "" {
		t.Errorf("relay: %+v", got)
	}
}

// switchedOn is an outside process's device, as simswitch would do it.
type switchedOn struct{ sets atomic.Int32 }

func (s *switchedOn) set(ctx context.Context, p *plugin.Plugin, cmd contract.SetCommand) error {
	s.sets.Add(1)
	return p.SetState(ctx, cmd.ID, contract.State{cmd.Capability: cmd.Value})
}

func TestAWriteLostToACrashIsWrittenAgain(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.untilValue("once", false)
	r.change("once-sensor", opened, true)
	r.until("the plugin to come back", func() bool { return r.count(pluginConnected, "plugin", "relays") == 2 })
	// Taken for somebody turning it off, it'd stay off under an hour's override of a Normal claim.
	r.untilValue("once", true)
}

func TestOutsidePluginsConnectToABrokerThatStartsLate(t *testing.T) {
	t.Parallel()
	broker, err := mqtttest.Listen()
	if err != nil {
		t.Fatal(err)
	}
	broker.Close()
	r := prepare(t)
	r.launch("--broker", broker.URL, "--outside-plugins", "kettle")
	r.waitFor(func(l line) bool { return l.is(attached) })

	broker.Start(t)
	t.Cleanup(broker.Close)
	ctx, cancel := context.WithTimeout(t.Context(), waitLimit)
	defer cancel()
	p, err := plugin.Start(ctx, plugin.Options{
		Environment: plugin.Environment{MQTTURL: broker.URL, Root: contract.NewRoot("kettle")},
		OnSet:       (&switchedOn{}).set,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) }) //nolint:usetesting // t.Context() is done once cleanups run
	err = p.SetDevices(ctx, []contract.DeviceReport{{ID: "kettle", Found: true, Capabilities: []string{"on"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Reported until heard: nothing retains it for a host that subscribes later.
	r.until("the kettle", func() bool {
		_ = p.SetState(ctx, "kettle", contract.State{"on": false})
		_ = p.SetDevices(ctx, []contract.DeviceReport{{ID: "kettle", Found: true, Capabilities: []string{"on"}}}, 0)
		return slices.ContainsFunc(r.status().Devices, func(d api.Device) bool { return d.Name == "kettle" })
	})
}

func TestAnOutsideProcessSpeaksMQTT(t *testing.T) {
	t.Parallel()
	broker, err := mqtttest.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	r := prepare(t)
	r.launch("--broker", broker.URL, "--outside-plugins", "kettle")
	r.waitFor(func(l line) bool { return l.is(attached) })

	ctx, cancel := context.WithTimeout(t.Context(), waitLimit)
	defer cancel()
	device := &switchedOn{}
	p, err := plugin.Start(ctx, plugin.Options{
		Environment: plugin.Environment{MQTTURL: broker.URL, Root: contract.NewRoot("kettle")},
		OnSet:       device.set,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) }) //nolint:usetesting // t.Context() is done once cleanups run
	err = p.SetDevices(ctx, []contract.DeviceReport{{ID: "kettle", Found: true, Capabilities: []string{"on"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetAvailability(ctx, "kettle", true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, "kettle", contract.State{"on": false}); err != nil {
		t.Fatal(err)
	}
	r.untilValue("kettle", false)
	r.stdout(nil, "set", "device/kettle/on", "on")
	r.untilValue("kettle", true)
	if device.sets.Load() != 1 {
		t.Fatalf("got %d sets", device.sets.Load())
	}
}

func TestAVersionThatDropsAPluginSaysWhatItLeftBehind(t *testing.T) {
	t.Parallel()
	r := startPlugins(t, settle)
	r.untilValue("relay", false)
	// The fixture's module, built as it is, imports no plugins.
	env := []string{"GOFLAGS=-mod=mod", "GOPROXY=off"}
	if stderr, err := r.oikade(env, "check", "--module", filepath.Join(buildDir, "module")); err != nil ||
		!strings.Contains(stderr, "oikade forget --plugin relays") {
		t.Fatalf("check: %v: %q", err, stderr)
	}
	stderr, err := r.deploy("nofan")
	if err != nil {
		t.Fatalf("%v: %s\nThe log:\n%s", err, stderr, r.dump())
	}
	r.awaitDeployed()
	if !strings.Contains(stderr, "relays: ") || !strings.Contains(stderr, "oikade forget --plugin relays") {
		t.Fatalf("got %q", stderr)
	}

	// Once nothing imports it, the list and status say so, rather than just offline.
	type listed struct {
		Name              string `json:"name"`
		PluginNotImported bool   `json:"pluginNotImported"`
	}
	r.until("relay to be marked", func() bool {
		var list []listed
		if err := json.Unmarshal(r.stdout(nil, "list", "devices", "--json"), &list); err != nil {
			t.Fatal(err)
		}
		return slices.Contains(list, listed{Name: "relay", PluginNotImported: true})
	})
	if !slices.ContainsFunc(r.status().Devices, func(d api.Device) bool { return d.Name == "relay" && d.PluginNotImported }) {
		t.Fatal("status doesn't mark relay")
	}
	if strings.Contains(string(r.stdout(nil, "status", "--json", "leak-sensor")), "pluginNotImported") {
		t.Error("status marks a simulated device")
	}

	r.stdout(nil, "forget", "--plugin", "relays", "--allow-safety")
	for _, d := range r.devices().Devices {
		if d.Driver == "plugin/relays" && !d.Removed {
			t.Errorf("%s stayed", d.Name)
		}
	}
}

// Stopping goes a phase at a time, and a plugin is asked to stop and says it's offline itself, before
// its socket goes.
func TestTheHostStopsInOrderAndPluginsSayGoodbye(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	r.until("both plugins online", func() bool { return r.count("Plugin came online") >= 2 })
	r.stop()
	for _, phase := range []string{
		"Stopping host", "Stopping API", "Stopping automations", "Finishing device writes", "Stopping plugins", "Closing store", "Stopped host",
	} {
		r.waitFor(func(l line) bool { return l.is(phase) })
	}
	if r.count("Cut off host shutdown at its deadline") != 0 {
		t.Errorf("a clean stop was cut off:\n%s", r.dump())
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	offline := map[string]bool{}
	asked, disconnected := false, 0
	for _, l := range r.lines {
		name, _ := l["plugin"].(string)
		switch {
		case l.is("Stopping plugins"):
			asked = true
		case l.is("Plugin went offline"):
			if !asked {
				t.Errorf("a plugin went offline before it was asked to stop: %v", l)
			}
			offline[fmt.Sprint(l["plugin"])] = true
		case l.is("Disconnected plugin"):
			disconnected++
			if reason, _ := l["reason"].(string); strings.Contains(reason, "closed network connection") {
				t.Errorf("the host closed %s's socket on it: %v", name, l)
			}
			said := false
			for who := range offline {
				said = said || strings.Contains(who, name)
			}
			if !said {
				t.Errorf("%s's socket went before it said it was offline", name)
			}
		}
	}
	if disconnected != 2 {
		t.Errorf("%d plugins disconnected, want 2", disconnected)
	}
}
