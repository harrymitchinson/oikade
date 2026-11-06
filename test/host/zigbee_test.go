package host

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
	"github.com/harrymitchinson/oikade/plugins/zigbee2mqtt"
)

const (
	zigbeeBase = zigbee2mqtt.DefaultBaseTopic
	deskPlug   = "0x00124b0000000004"
)

// fakeZigbee2MQTT does what a real one does on the broker: it retains its device list and state, and
// answers a /set by publishing the device's new state.
type fakeZigbee2MQTT struct {
	bus *mqtt.Client

	mu     sync.Mutex
	writes []string
}

func startFakeZigbee2MQTT(t *testing.T, url string) *fakeZigbee2MQTT {
	t.Helper()
	// The connection lasts as long as this context.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bus, err := mqtt.Connect(ctx, url, "fake-zigbee2mqtt")
	if err != nil {
		t.Fatal(err)
	}
	z := &fakeZigbee2MQTT{bus: bus}
	devices, err := os.ReadFile(filepath.Join("..", "..", "plugins", "zigbee2mqtt", "testdata", "bridge-devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = bus.Subscribe(ctx, zigbeeBase+"/"+deskPlug+"/set", func(m mqtt.Message) {
		z.mu.Lock()
		z.writes = append(z.writes, string(m.Payload))
		z.mu.Unlock()
		if err := bus.Publish(ctx, zigbeeBase+"/desk-plug", m.Payload, true); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	z.publish(t, "bridge/state", `{"state":"online"}`)
	z.publish(t, "desk-plug", `{"state":"OFF"}`)
	z.publish(t, "bridge/devices", string(devices))
	return z
}

func (z *fakeZigbee2MQTT) publish(t *testing.T, topic, payload string) {
	t.Helper()
	if err := z.bus.Publish(context.Background(), zigbeeBase+"/"+topic, []byte(payload), true); err != nil {
		t.Fatal(err)
	}
}

func TestZigbeeDevicesPairAndCanBeSetThroughAZigbee2MQTTYouRun(t *testing.T) {
	t.Parallel()
	url := mqtttest.StartBroker(t)
	z := startFakeZigbee2MQTT(t, url)
	r := prepare(t)
	r.launchWithout("--home", variant(t, "zigbee"), "--simulate", devicesFile(t), "--broker", url)
	r.waitFor(func(l line) bool { return l.is(attached) })

	// Paired in Zigbee2MQTT, and usable with nothing else done.
	r.untilValue("desk-plug", false)
	d := deviceOf(t, r.devices(), "desk-plug")
	if d.ID != deskPlug || d.Driver != "plugin/home/zigbee" || d.Endpoint == 0 {
		t.Fatalf("got %+v", d)
	}
	r.stdout(nil, "set", "desk-plug", "switch.on", "on")
	r.untilValue("desk-plug", true)
	z.mu.Lock()
	writes := append([]string(nil), z.writes...)
	z.mu.Unlock()
	if len(writes) != 1 || writes[0] != `{"state":"ON"}` {
		t.Fatalf("Zigbee2MQTT got %v", writes)
	}

	// With Zigbee2MQTT gone its devices read offline, and your automations carry on.
	z.publish(t, "bridge/state", `{"state":"offline"}`)
	r.until("the plug to go offline", func() bool {
		online := onlineOf(r, "desk-plug")
		return online != nil && !*online
	})
	r.sync()
	r.change("bathroom-sensor", "sensor.relative-humidity", 80)
	r.wrote("fan", true)

	z.publish(t, "bridge/state", `{"state":"online"}`)
	r.until("the plug to come back", func() bool {
		online := onlineOf(r, "desk-plug")
		return online != nil && *online
	})
	r.stdout(nil, "set", "desk-plug", "switch.on", "off")
	r.untilValue("desk-plug", false)
}

func onlineOf(r *rig, device string) *bool {
	for _, d := range r.status().Devices {
		if d.Name == device {
			return d.Online
		}
	}
	return nil
}

func TestAZigbeePluginWithNoBrokerSaysSo(t *testing.T) {
	t.Parallel()
	r := prepare(t)
	r.launchWithout("--home", variant(t, "zigbee"), "--simulate", devicesFile(t))
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.until("the plugin to give up", func() bool { return r.count(pluginRestarted, "plugin", "zigbee") > 0 })
}
