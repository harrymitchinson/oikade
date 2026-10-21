package runtime_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/hap"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
	"github.com/harrymitchinson/oikade/internal/runtime"
)

const ns = "home"

func TestRuntimeEndToEnd(t *testing.T) {
	ctx, c, url, reg := startRuntime(t)

	fakeZ2M, err := mqtt.Connect(ctx, url, "fake-z2m")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commands []string
	if _, err := fakeZ2M.Subscribe(ctx, "z2m/+/set", func(m mqtt.Message) {
		mu.Lock()
		defer mu.Unlock()
		commands = append(commands, m.Topic+" "+string(m.Payload))
	}); err != nil {
		t.Fatal(err)
	}

	// The declared home.
	must(t, c.Create(ctx, &zigbeev1alpha1.Zigbee2MQTT{
		ObjectMeta: metav1.ObjectMeta{Name: "z2m", Namespace: ns},
		Spec:       zigbeev1alpha1.Zigbee2MQTTSpec{Serial: zigbeev1alpha1.SerialSpec{Port: "tcp://slzb:6638", Adapter: "zstack"}},
	}))
	device := func(name, ieee, room string) *homev1alpha1.Device {
		return &homev1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: homev1alpha1.DeviceSpec{
				RoomRef: &corev1.LocalObjectReference{Name: room},
				Zigbee2MQTT: &homev1alpha1.ZigbeeDeviceSource{
					InstanceRef: corev1.LocalObjectReference{Name: "z2m"}, IEEEAddress: ieee,
				},
			},
		}
	}
	must(t, c.Create(ctx, device("hallway-light", "0x000b57fffe000001", "hallway")))
	must(t, c.Create(ctx, device("hallway-lamp", secondLamp, "hallway")))
	must(t, c.Create(ctx, device("hallway-motion", "0x00158d0000000002", "hallway")))
	must(t, c.Create(ctx, device("desk-plug", "0x00124b0000000004", "hallway")))
	must(t, c.Create(ctx, device("front-door", "0x00158d0000000003", "porch")))
	must(t, c.Create(ctx, device("not-paired", "0x00158d00000000ff", "hallway")))
	must(t, c.Create(ctx, &homev1alpha1.Rule{
		ObjectMeta: metav1.ObjectMeta{Name: "motion-light", Namespace: ns},
		Spec: homev1alpha1.RuleSpec{
			Triggers:  []homev1alpha1.Trigger{{Change: &homev1alpha1.ChangeTrigger{Device: "hallway-motion", Capability: capability.Occupancy}}},
			Condition: "trigger.value",
			Do: []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
				Device: "hallway-light", Capability: capability.LightOn, Value: apiextensionsv1.JSON{Raw: []byte("true")},
			}}},
		},
	}))

	// Opening the front door turns off every light in the hallway.
	must(t, c.Create(ctx, &homev1alpha1.Rule{
		ObjectMeta: metav1.ObjectMeta{Name: "hallway-off", Namespace: ns},
		Spec: homev1alpha1.RuleSpec{
			Triggers: []homev1alpha1.Trigger{{Change: &homev1alpha1.ChangeTrigger{Room: "porch", Capability: capability.ContactOpen}}},
			Do: []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
				Room: "hallway", Capability: capability.LightOn, Value: apiextensionsv1.JSON{Raw: []byte("false")},
			}}},
		},
	}))

	// Zigbee2MQTT announces its paired devices.
	must(t, fakeZ2M.Publish(ctx, "z2m/bridge/devices", bridgeDevicesWithSecondLamp(t), true))
	must(t, fakeZ2M.Publish(ctx, "z2m/hallway-motion/availability", []byte(`{"state":"online"}`), true))

	eventually(t, "light status", func() bool {
		var d homev1alpha1.Device
		if c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "hallway-light"}, &d) != nil {
			return false
		}
		var caps []string
		for _, c := range d.Status.Capabilities {
			caps = append(caps, c.Name)
		}
		return meta.IsStatusConditionTrue(d.Status.Conditions, homev1alpha1.ConditionResolved) &&
			d.Status.Manufacturer == "IKEA" &&
			slices.Equal(caps, []string{capability.LightOn, capability.LightLevel, capability.LinkQuality})
	})
	eventually(t, "unpaired device reported", func() bool {
		var d homev1alpha1.Device
		if c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "not-paired"}, &d) != nil {
			return false
		}
		cond := meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionResolved)
		return cond != nil && cond.Reason == "NotPaired"
	})
	eventually(t, "motion sensor online", func() bool {
		var d homev1alpha1.Device
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "hallway-motion"}, &d) == nil &&
			meta.IsStatusConditionTrue(d.Status.Conditions, homev1alpha1.ConditionAvailable)
	})

	// Motion turns the light on through the rule.
	must(t, fakeZ2M.Publish(ctx, "z2m/hallway-motion", []byte(`{"occupancy":true,"temperature":19.5}`), false))
	eventually(t, "light command", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(commands, `z2m/0x000b57fffe000001/set {"state":"ON"}`)
	})

	labels := prometheus.Labels{"namespace": ns, "device": "hallway-motion", "room": "hallway"}
	eventually(t, "metrics", func() bool {
		n, err := testutil.GatherAndCount(reg, "oikade_capability_value", "oikade_device_available", "oikade_undeclared_devices")
		return err == nil && n >= 4
	})
	expectMetric(t, reg, "oikade_capability_value", labels, capability.Occupancy, 1)
	expectMetric(t, reg, "oikade_capability_value", labels, capability.Temperature, 19.5)
	expectMetric(t, reg, "oikade_capability_changes_total", labels, capability.Occupancy, 1)

	eventually(t, "lastTriggered", func() bool {
		var a homev1alpha1.Rule
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "motion-light"}, &a) == nil && !a.Status.LastTriggered.IsZero()
	})

	// One room action sends /set to both hallway lamps, and skips the plug and the motion sensor,
	// which don't have light.on.
	for _, name := range []string{"hallway-lamp", "desk-plug"} {
		eventually(t, name+" discovered", func() bool {
			var d homev1alpha1.Device
			return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d) == nil && len(d.Status.Capabilities) > 0
		})
	}
	must(t, fakeZ2M.Publish(ctx, "z2m/front-door", []byte(`{"contact":false}`), false))
	eventually(t, "room commands", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(commands, `z2m/0x000b57fffe000001/set {"state":"OFF"}`) &&
			slices.Contains(commands, "z2m/"+secondLamp+`/set {"state":"OFF"}`)
	})
	// Arbitrating the second lamp's light.on puts it in Device status, at the default.
	var lamp homev1alpha1.Device
	must(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "hallway-lamp"}, &lamp))
	lamp.Spec.Control = []homev1alpha1.CapabilityControl{{
		Capability: capability.LightOn, Default: apiextensionsv1.JSON{Raw: []byte("false")},
		Resolve: homev1alpha1.ControlResolvePreferOn, External: homev1alpha1.ControlExternalOverride, OverrideHold: "1h",
	}}
	must(t, c.Update(ctx, &lamp))
	eventually(t, "lamp control status", func() bool {
		var d homev1alpha1.Device
		if c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "hallway-lamp"}, &d) != nil || len(d.Status.Control) != 1 {
			return false
		}
		st := d.Status.Control[0]
		return st.Capability == capability.LightOn && st.Level == homev1alpha1.ControlLevelDefault &&
			st.Value != nil && string(st.Value.Raw) == "false" && len(st.Claims) == 0
	})
	if got, ok := metricValue(t, reg, "oikade_device_control_level",
		prometheus.Labels{"namespace": ns, "device": "hallway-lamp", "capability": capability.LightOn}); !ok || got != 0 {
		t.Errorf("oikade_device_control_level = %v (%v), want 0", got, ok)
	}

	tapLamp(ctx, t, c, url, fakeZ2M)

	mu.Lock()
	defer mu.Unlock()
	for _, cmd := range commands {
		if !strings.HasPrefix(cmd, "z2m/0x000b57fffe000001/") && !strings.HasPrefix(cmd, "z2m/"+secondLamp+"/") {
			t.Errorf("unexpected command %s", cmd)
		}
	}
}

// A plugin's retained topics feed Device status, metrics and rules, and rules reach the
// plugin through /set.
func TestRuntimePluginDevices(t *testing.T) {
	ctx, c, url, reg := startRuntime(t)

	fakePlugin, err := mqtt.Connect(ctx, url, "fake-plugin")
	if err != nil {
		t.Fatal(err)
	}
	const root = "oikade/v1/" + ns + "/sonoff"
	var mu sync.Mutex
	var commands []string
	if _, err := fakePlugin.Subscribe(ctx, root+"/+/set", func(m mqtt.Message) {
		mu.Lock()
		defer mu.Unlock()
		commands = append(commands, m.Topic+" "+string(m.Payload))
	}); err != nil {
		t.Fatal(err)
	}

	// The plugin is already running: its retained topics are on the bus before the runtime knows of it.
	for topic, payload := range map[string]string{
		"status": `{"state":"online","contract":1}`,
		"devices": `{"devices":[
			{"id":"relay1","found":true,"manufacturer":"SONOFF","model":"MINIR4","capabilities":["light.on"]},
			{"id":"button","found":true,"capabilities":["switch.on"]},
			{"id":"pump","found":false,"error":"no LAN key for pump in /etc/oikade/secrets"}
		],"undeclared":1}`,
		"relay1/availability": "online",
		"relay1/state":        `{"light.on":false}`,
		"button/state":        `{"switch.on":false}`,
	} {
		must(t, fakePlugin.Publish(ctx, root+"/"+topic, []byte(payload), true))
	}

	device := func(name, id string) *homev1alpha1.Device {
		return &homev1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: homev1alpha1.DeviceSpec{
				RoomRef: &corev1.LocalObjectReference{Name: "porch"},
				Plugin: &homev1alpha1.PluginDeviceSource{
					PluginRef: corev1.LocalObjectReference{Name: "sonoff"}, ID: id,
				},
			},
		}
	}
	must(t, c.Create(ctx, device("porch-light", "relay1")))
	must(t, c.Create(ctx, device("porch-button", "button")))
	must(t, c.Create(ctx, device("pool-pump", "pump")))
	// Devices come first, as they would from a synced cache: retained state for ids the hub doesn't
	// know yet counts as undeclared.
	must(t, c.Create(ctx, &pluginsv1alpha1.Plugin{
		ObjectMeta: metav1.ObjectMeta{Name: "sonoff", Namespace: ns},
		Spec:       pluginsv1alpha1.PluginSpec{Image: "example.com/oikade-plugin-sonoff:v0.1.0"},
	}))
	must(t, c.Create(ctx, &homev1alpha1.Rule{
		ObjectMeta: metav1.ObjectMeta{Name: "button-light", Namespace: ns},
		Spec: homev1alpha1.RuleSpec{
			Triggers:  []homev1alpha1.Trigger{{Change: &homev1alpha1.ChangeTrigger{Device: "porch-button", Capability: capability.SwitchOn}}},
			Condition: "trigger.value",
			Do: []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
				Device: "porch-light", Capability: capability.LightOn, Value: apiextensionsv1.JSON{Raw: []byte("true")},
			}}},
		},
	}))

	eventually(t, "light status", func() bool {
		var d homev1alpha1.Device
		if c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "porch-light"}, &d) != nil {
			return false
		}
		return meta.IsStatusConditionTrue(d.Status.Conditions, homev1alpha1.ConditionResolved) &&
			meta.IsStatusConditionTrue(d.Status.Conditions, homev1alpha1.ConditionAvailable) &&
			d.Status.Manufacturer == "SONOFF" && d.Status.Model == "MINIR4" &&
			len(d.Status.Capabilities) == 1 && d.Status.Capabilities[0].Name == capability.LightOn
	})
	eventually(t, "plugin error", func() bool {
		var d homev1alpha1.Device
		if c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pool-pump"}, &d) != nil {
			return false
		}
		cond := meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionResolved)
		return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == "PluginError" &&
			cond.Message == "no LAN key for pump in /etc/oikade/secrets"
	})
	eventually(t, "button discovered", func() bool {
		var d homev1alpha1.Device
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "porch-button"}, &d) == nil &&
			meta.IsStatusConditionTrue(d.Status.Conditions, homev1alpha1.ConditionResolved)
	})

	// Pressing the button turns the light on through the rule.
	must(t, fakePlugin.Publish(ctx, root+"/button/state", []byte(`{"switch.on":true}`), true))
	eventually(t, "light command", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(commands, root+`/relay1/set {"light.on":true}`)
	})

	// An unknown capability is dropped and counted.
	must(t, fakePlugin.Publish(ctx, root+"/relay1/state", []byte(`{"light.on":true,"made.up":1}`), true))
	plugin := prometheus.Labels{"namespace": ns, "plugin": "sonoff"}
	eventually(t, "plugin metrics", func() bool {
		up, _ := metricValue(t, reg, "oikade_plugin_up", plugin)
		rejected, _ := metricValue(t, reg, "oikade_plugin_rejected_messages_total",
			prometheus.Labels{"namespace": ns, "plugin": "sonoff", "reason": "unknown_capability"})
		undeclared, _ := metricValue(t, reg, "oikade_undeclared_devices",
			prometheus.Labels{"namespace": ns, "instance": "plugin/sonoff"})
		value, _ := metricValue(t, reg, "oikade_capability_value",
			prometheus.Labels{"namespace": ns, "device": "porch-light", "capability": capability.LightOn})
		return up == 1 && rejected >= 1 && undeclared == 1 && value == 1
	})

	// The plugin going away takes its devices offline.
	must(t, fakePlugin.Publish(ctx, root+"/status", []byte(`{"state":"offline","contract":1}`), true))
	eventually(t, "plugin down", func() bool {
		up, ok := metricValue(t, reg, "oikade_plugin_up", plugin)
		var d homev1alpha1.Device
		return ok && up == 0 && c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "porch-light"}, &d) == nil &&
			meta.IsStatusConditionFalse(d.Status.Conditions, homev1alpha1.ConditionAvailable)
	})
}

// TestHubIgnoresStatusPatches checks that the hub reconciles on Device spec changes, creates and
// deletes, but not on the status patches the status writer makes.
func TestHubIgnoresStatusPatches(t *testing.T) {
	ctx, c, _, _ := startRuntime(t)

	reconciles := func() float64 {
		families, err := crmetrics.Registry.Gather()
		must(t, err)
		var n float64
		for _, f := range families {
			if f.GetName() != "controller_runtime_reconcile_total" {
				continue
			}
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "controller" && l.GetValue() == "hub-plugin" {
						n += m.GetCounter().GetValue()
					}
				}
			}
		}
		return n
	}
	// settled waits until the hub has reconciled since before and then stays quiet for a second.
	settled := func(what string, before float64) float64 {
		t.Helper()
		var last float64
		eventually(t, what, func() bool {
			n := reconciles()
			if n <= before {
				return false
			}
			time.Sleep(time.Second)
			last = reconciles()
			return last == n
		})
		return last
	}

	n := reconciles()
	must(t, c.Create(ctx, &pluginsv1alpha1.Plugin{
		ObjectMeta: metav1.ObjectMeta{Name: "sonoff", Namespace: ns},
		Spec:       pluginsv1alpha1.PluginSpec{Image: "example.com/oikade-plugin-sonoff:v0.1.0"},
	}))
	n = settled("Plugin create", n)
	light := &homev1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "porch-light", Namespace: ns},
		Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
			PluginRef: corev1.LocalObjectReference{Name: "sonoff"}, ID: "relay1",
		}},
	}
	must(t, c.Create(ctx, light))
	// The status writer reports the device as waiting for its plugin; none of its patches reach the
	// hub. NoDriver can show briefly before the hub claims the Device, so wait for the steady state.
	n = settled("Device create", n)
	eventually(t, "light status", func() bool {
		must(t, c.Get(ctx, client.ObjectKeyFromObject(light), light))
		resolved := meta.FindStatusCondition(light.Status.Conditions, homev1alpha1.ConditionResolved)
		return resolved != nil && resolved.Reason == "Waiting"
	})

	patch := client.MergeFrom(light.DeepCopy())
	light.Status.Manufacturer = "Patched"
	must(t, c.Status().Patch(ctx, light, patch))
	time.Sleep(2 * time.Second)
	if got := reconciles(); got != n {
		t.Fatalf("hub reconciled %v times after a Device status patch, want 0", got-n)
	}

	must(t, c.Get(ctx, client.ObjectKeyFromObject(light), light))
	light.Spec.DisplayName = "Porch light"
	must(t, c.Update(ctx, light))
	n = settled("Device spec change", n)

	must(t, c.Delete(ctx, light))
	settled("Device delete", n)
}

// startRuntime starts envtest, a broker and the runtime, and creates the home namespace.
func TestRuntimeSchedulesTimeTriggers(t *testing.T) {
	ctx, c, url, reg := startRuntime(t)
	lightOn := []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
		Device: "hallway-light", Capability: capability.LightOn, Value: apiextensionsv1.JSON{Raw: []byte("true")},
	}}}
	// Two hours ahead in the runtime's UTC, so it can't fire during the test.
	at := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	must(t, c.Create(ctx, &homev1alpha1.Rule{
		ObjectMeta: metav1.ObjectMeta{Name: "evening", Namespace: ns},
		Spec: homev1alpha1.RuleSpec{
			Triggers: []homev1alpha1.Trigger{{At: &homev1alpha1.AtTrigger{Time: at.Format("15:04")}}},
			Do:       lightOn,
		},
	}))
	must(t, c.Create(ctx, &homev1alpha1.Rule{
		ObjectMeta: metav1.ObjectMeta{Name: "porch-sunset", Namespace: ns},
		Spec: homev1alpha1.RuleSpec{
			Triggers: []homev1alpha1.Trigger{{Sun: &homev1alpha1.SunTrigger{Event: homev1alpha1.SunEventSunset}}},
			Do:       lightOn,
		},
	}))

	get := func(name string) homev1alpha1.Rule {
		var r homev1alpha1.Rule
		_ = c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &r)
		return r
	}
	eventually(t, "evening nextFire", func() bool {
		r := get("evening")
		return r.Status.NextFire.Time.Equal(at) &&
			meta.IsStatusConditionTrue(r.Status.Conditions, homev1alpha1.ConditionScheduled)
	})
	if got, ok := metricValue(t, reg, "oikade_rule_next_change_timestamp_seconds",
		prometheus.Labels{"namespace": ns, "rule": "evening"}); !ok || got != float64(at.Unix()) {
		t.Errorf("next-change gauge = %v (%v), want %v", got, ok, at.Unix())
	}

	// The runtime has no --latitude or --longitude.
	eventually(t, "porch-sunset NoLocation", func() bool {
		r := get("porch-sunset")
		cond := meta.FindStatusCondition(r.Status.Conditions, homev1alpha1.ConditionScheduled)
		return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == "NoLocation" && r.Status.NextFire.IsZero()
	})

	// A hold whose window opens in two hours reports when, and saves its state on the bus.
	must(t, c.Create(ctx, &homev1alpha1.Rule{
		ObjectMeta: metav1.ObjectMeta{Name: "night-heat", Namespace: ns},
		Spec: homev1alpha1.RuleSpec{
			While: &homev1alpha1.While{Window: &homev1alpha1.Window{
				Start: homev1alpha1.WindowTime{Time: at.Format("15:04")},
				End:   homev1alpha1.WindowTime{Time: at.Add(time.Hour).Format("15:04")},
			}},
			Hold: &homev1alpha1.Hold{Device: "hallway-light", Capability: capability.LightOn, Value: apiextensionsv1.JSON{Raw: []byte("true")}},
		},
	}))
	eventually(t, "night-heat nextChange", func() bool {
		r := get("night-heat")
		return r.Status.NextChange.Time.Equal(at) && !r.Status.Active && r.Status.NextFire.IsZero() &&
			meta.FindStatusCondition(r.Status.Conditions, homev1alpha1.ConditionScheduled) == nil
	})
	if got, ok := metricValue(t, reg, "oikade_rule_active", prometheus.Labels{"namespace": ns, "rule": "night-heat"}); !ok || got != 0 {
		t.Errorf("oikade_rule_active = %v (%v), want 0", got, ok)
	}
	watcher, err := mqtt.Connect(ctx, url, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan string, 1)
	if _, err := watcher.Subscribe(ctx, "oikade/v1/_runtime/rules/"+ns+"/night-heat", func(m mqtt.Message) {
		if m.Retained {
			select {
			case saved <- string(m.Payload):
			default:
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-saved:
		if !strings.Contains(got, `"active":false`) || !strings.Contains(got, `"device":"hallway-light"`) {
			t.Errorf("saved state = %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no saved hold state")
	}
}

func startRuntime(t *testing.T) (context.Context, client.Client, string, *prometheus.Registry) {
	t.Helper()
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestBinaries(t),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := kruntime.NewScheme()
	for _, add := range []func(*kruntime.Scheme) error{
		clientgoscheme.AddToScheme, homev1alpha1.AddToScheme, zigbeev1alpha1.AddToScheme, pluginsv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	url := mqtttest.StartBroker(t)
	// Cleanups run last-in first-out: registered after the broker and the API server, cancel stops
	// the runtime before them, on every exit path.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	bus, err := mqtt.Connect(ctx, url, "runtime")
	if err != nil {
		t.Fatal(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		// Each test starts its own manager with the same controller names.
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	if _, err := runtime.Setup(ctx, mgr, bus, runtime.Options{
		Location: time.UTC, SeenInterval: time.Minute, TriggeredInterval: time.Second, Registry: reg,
	}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	return ctx, c, url, reg
}

// secondLamp is a copy of the fixture's IKEA lamp, so the hallway has two lights.
const secondLamp = "0x000b57fffe000006"

// bridgeDevicesWithSecondLamp returns the driver's bridge/devices fixture with secondLamp added.
func bridgeDevicesWithSecondLamp(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "driver", "zigbee2mqtt", "testdata", "bridge-devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	var devices []map[string]any
	must(t, json.Unmarshal(raw, &devices))
	for _, d := range devices {
		if d["ieee_address"] == "0x000b57fffe000001" {
			lamp := maps.Clone(d)
			lamp["ieee_address"], lamp["friendly_name"] = secondLamp, secondLamp
			devices = append(devices, lamp)
			break
		}
	}
	out, err := json.Marshal(devices)
	must(t, err)
	return out
}

func expectMetric(t *testing.T, reg *prometheus.Registry, name string, labels prometheus.Labels, capName string, want float64) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if got["capability"] != capName || got["device"] != labels["device"] ||
				got["room"] != labels["room"] || got["namespace"] != labels["namespace"] {
				continue
			}
			v := m.GetGauge().GetValue() + m.GetCounter().GetValue()
			if v != want {
				t.Fatalf("%s{capability=%s} = %v, want %v", name, capName, v, want)
			}
			return
		}
	}
	t.Fatalf("%s{capability=%s} not found", name, capName)
}

// metricValue returns the value of the first series of name whose labels include labels.
func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels prometheus.Labels) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue series
				}
			}
			return m.GetGauge().GetValue() + m.GetCounter().GetValue(), true
		}
	}
	return 0, false
}

// tapLamp turns the second lamp on from the Home app: the bridge announces it, then writes the
// device, which reports the change. Its control status then shows an Override from HomeKit, not
// from the device.
func tapLamp(ctx context.Context, t *testing.T, c client.Client, url string, fakeZ2M *mqtt.Client) {
	t.Helper()
	must(t, fakeZ2M.Publish(ctx, "z2m/"+secondLamp, []byte(`{"state":"OFF"}`), false))
	fakeHAP, err := mqtt.Connect(ctx, url, "fake-hap")
	if err != nil {
		t.Fatal(err)
	}
	tap := &hap.Announcer{Bus: fakeHAP, Bridge: types.NamespacedName{Namespace: ns, Name: "living-room"},
		Setter: setterFunc(func(ctx context.Context, _ types.NamespacedName, _ string, _ any) error {
			return fakeZ2M.Publish(ctx, "z2m/"+secondLamp, []byte(`{"state":"ON"}`), false)
		})}
	must(t, tap.Set(ctx, types.NamespacedName{Namespace: ns, Name: "hallway-lamp"}, capability.LightOn, true))
	eventually(t, "lamp HomeKit claim", func() bool {
		var d homev1alpha1.Device
		if c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "hallway-lamp"}, &d) != nil || len(d.Status.Control) != 1 {
			return false
		}
		st := d.Status.Control[0]
		return st.Level == homev1alpha1.ControlLevelOverride && len(st.Claims) == 1 &&
			st.Claims[0].Source == "HomeKit" && st.Claims[0].Winner && string(st.Claims[0].Value.Raw) == "true"
	})
}

// setterFunc is a driver.Setter.
type setterFunc func(ctx context.Context, dev types.NamespacedName, name string, value any) error

func (f setterFunc) Set(ctx context.Context, dev types.NamespacedName, name string, value any) error {
	return f(ctx, dev, name, value)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// envtestBinaries finds binaries installed by `make setup-envtest`, as the controller suites do.
func envtestBinaries(t *testing.T) string {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Skip("envtest binaries not installed; run make setup-envtest")
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	t.Skip("envtest binaries not installed; run make setup-envtest")
	return ""
}
