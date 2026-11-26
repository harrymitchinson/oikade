package metrics

import (
	"bytes"
	"strings"
	"testing"

	"oikade.dev/internal/driver"
	"oikade.dev/internal/driver/plugin"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
)

func TestUndeclaredInstanceLabel(t *testing.T) {
	store := hub.NewStore(logging.Discard())
	name := ident.ID{Name: "garden"}
	store.Undeclared(driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: name}, 3)
	store.Undeclared(driver.InstanceKey{Driver: driver.Plugin, ID: name}, 1)

	want := `oikade_undeclared_devices{instance="garden"} 3
oikade_undeclared_devices{instance="plugin/garden"} 1
`
	if got := written(Collector{Store: store}, "oikade_undeclared_devices"); got != want {
		t.Errorf("wrote\n%s\nwant\n%s", got, want)
	}
}

type fakePlugins map[ident.ID]plugin.Stats

func (f fakePlugins) PluginStats() map[ident.ID]plugin.Stats { return f }

func TestPluginMetrics(t *testing.T) {
	c := Collector{Store: hub.NewStore(logging.Discard()), Plugins: fakePlugins{
		{Name: "sonoff"}: {Up: true, Rejected: map[string]uint64{plugin.ReasonUnknownCapability: 2}},
		{Name: "cozy"}:   {},
	}}

	want := `oikade_plugin_up{plugin="cozy"} 0
oikade_plugin_up{plugin="sonoff"} 1
oikade_plugin_rejected_messages_total{plugin="sonoff",reason="unknown_capability"} 2
`
	if got := written(c, "oikade_plugin_"); got != want {
		t.Errorf("wrote\n%s\nwant\n%s", got, want)
	}
}

// A device's series are labelled with its hardware's ref, as every command names it.
func TestDeviceMetricsAreLabelledByHardwareSortedAndEscaped(t *testing.T) {
	store := hub.NewStore(logging.Discard())
	inst := driver.InstanceKey{Driver: "plugin", Name: "zigbee2mqtt"}
	fan, lamp := ident.ID{Name: "zigbee2mqtt/0x00158d0001a2b3c4"}, ident.ID{Name: "zigbee2mqtt/0x00158d0001a2b3c5"}
	store.SetDevices(inst, map[ident.ID]hub.Meta{
		fan: {Instance: inst, Room: `Kid's "den"`}, lamp: {Instance: inst},
	})
	for _, u := range []driver.Update{
		{Device: lamp, Capability: "on", Value: true},
		{Device: fan, Capability: "speed", Value: 2.5},
		{Device: fan, Capability: "on", Value: false},
	} {
		store.Changed(u)
	}

	want := `oikade_capability_value{capability="on",device="hardware/zigbee2mqtt/0x00158d0001a2b3c4",room="Kid's \"den\""} 0
oikade_capability_value{capability="on",device="hardware/zigbee2mqtt/0x00158d0001a2b3c5",room=""} 1
oikade_capability_value{capability="speed",device="hardware/zigbee2mqtt/0x00158d0001a2b3c4",room="Kid's \"den\""} 2.5
`
	if got := written(Collector{Store: store}, "oikade_capability_value"); got != want {
		t.Errorf("wrote\n%s\nwant\n%s", got, want)
	}
}

// written is the lines c writes for the series whose names start with prefix.
func written(c Collector, prefix string) string {
	var out bytes.Buffer
	c.Write(&out)
	var lines strings.Builder
	for line := range strings.Lines(out.String()) {
		if strings.HasPrefix(line, prefix) {
			lines.WriteString(line)
		}
	}
	return lines.String()
}
