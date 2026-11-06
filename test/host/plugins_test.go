package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

const (
	pluginConnected = "Connected plugin"
	pluginStarted   = "Started plugin process"
	pluginRestarted = "Restarting plugin process"
	pluginKilled    = "Killing plugin process; it stopped answering"
)

// pluginDevices is the usual simulated home, with its valve on the relays plugin instead.
func pluginDevices(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "devices-novalve.json"))
	if err != nil {
		t.Fatal(err)
	}
	var devices []map[string]any
	if err := json.Unmarshal(raw, &devices); err != nil {
		t.Fatal(err)
	}
	devices = append(devices, map[string]any{
		"plugin": "relays", "name": "valve", "capabilities": []string{"switch.on"},
		"control": []map[string]any{{
			"capability": "switch.on", "default": true, "resolve": "PreferOff", "external": "Override", "overrideHold": "1h",
		}},
	})
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
	r := prepare(t)
	r.env = append(r.env, "OIKADE_TEST_PLUGIN_DOWN="+filepath.Join(r.dir, "plugin-down"))
	r.launchPlugins(args...)
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.until("both plugins", func() bool { return r.count(pluginConnected) >= 2 })
	return r
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
		_, err := r.oikade(nil, "set", device, "switch.on", value)
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
				if c.Name == "switch.on" {
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
	r.stdout(nil, "set", "relay", "switch.on", "on")
	r.untilValue("relay", true)
	if d := deviceOf(t, r.devices(), "relay"); d.Driver != "plugin/home/relays" || d.Endpoint == 0 {
		t.Fatalf("got %+v", d)
	}

	r.stdout(nil, "devices", "name", "relay", "porch", "--allow-safety")
	r.setting("porch", "off")
	r.untilValue("porch", false)
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
	r.stdout(nil, "set", "relay", "switch.on", "on")
	r.untilValue("relay", true)
	automations := r.pid()
	r.stdout(nil, "set", "panics", "switch.on", "on")
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
	r.stdout(nil, "set", "hangs", "switch.on", "on")
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
	r.stdout(nil, "set", "panics", "switch.on", "on")
	r.waitFor(func(l line) bool { return l.is(pluginRestarted, "plugin", "relays") })
	r.until("the plugin to come back", func() bool { return r.count(pluginConnected, "plugin", "relays") == 2 })
	r.until("the valve to be seen open and shut again", func() bool {
		return r.count(changed, "device", "valve") >= before+2 && r.value("valve") == false
	})
	st := onOf(t, r.status(), "valve")
	if st.Level != "Safety" {
		t.Fatalf("got %+v. The log:\n%s", st, r.dump())
	}
	if _, err := r.oikade(nil, "set", "valve", "switch.on", "on"); err == nil {
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
	r.stdout(nil, "set", "panics", "switch.on", "on")
	r.waitFor(func(l line) bool { return l.is(pluginRestarted, "plugin", "relays") })
	stderr, err := r.oikade(nil, "set", "relay", "switch.on", "on")
	if err == nil || !strings.Contains(stderr, "offline") {
		t.Fatalf("got %v, %q", err, stderr)
	}
	if err := os.Remove(filepath.Join(r.dir, "plugin-down")); err != nil {
		t.Fatal(err)
	}
	r.until("the plugin to come back", func() bool { return r.count(pluginConnected, "plugin", "relays") == 2 })
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
	if !strings.Contains(stderr, `"leak-shutoff"`) || !strings.Contains(stderr, "--allow-safety-removal") {
		t.Fatalf("got %q", stderr)
	}
}

func TestAControllerDrivesAPluginsDevice(t *testing.T) {
	t.Parallel()
	port := freeUDP(t)
	r := startPlugins(t, matterFlag(port))
	r.untilValue("relay", false)
	r.until("the commissioning window", func() bool { return r.matter().Window })
	c := &controller{t: t, r: r, port: port, cm: nodetest.New(t)}
	c.admin = c.cm.Commission(nodetest.Addr(port), r.matter().Passcode)
	endpoint := c.endpoint("relay")
	c.command(endpoint, turnOn)
	r.untilValue("relay", true)
	r.stdout(nil, "set", "relay", "switch.on", "off")
	r.until("the controller to hear relay", func() bool { return !c.isOn(endpoint) })
}

// switchedOn is an outside process's device, as simswitch would do it.
type switchedOn struct{ sets atomic.Int32 }

func (s *switchedOn) set(ctx context.Context, p *plugin.Plugin, cmd contractv1.SetCommand) error {
	s.sets.Add(1)
	return p.SetState(ctx, cmd.ID, contractv1.State{cmd.Capability: cmd.Value})
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

	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	device := &switchedOn{}
	p, err := plugin.Start(ctx, plugin.Options{
		Environment: plugin.Environment{MQTTURL: broker.URL, Root: contractv1.NewRoot("home", "kettle")},
		OnSet:       device.set,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	err = p.SetDevices(ctx, []contractv1.DeviceReport{{ID: "kettle", Found: true, Capabilities: []string{"switch.on"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetAvailability(ctx, "kettle", true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, "kettle", contractv1.State{"switch.on": false}); err != nil {
		t.Fatal(err)
	}
	r.untilValue("kettle", false)
	r.stdout(nil, "set", "kettle", "switch.on", "on")
	r.untilValue("kettle", true)
	if device.sets.Load() != 1 {
		t.Fatalf("got %d sets", device.sets.Load())
	}
}
