package metrics

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/driver/plugin"
	"github.com/harrymitchinson/oikade/internal/hub"
)

func TestUndeclaredInstanceLabel(t *testing.T) {
	store := hub.NewStore(logr.Discard())
	name := types.NamespacedName{Namespace: "home", Name: "garden"}
	store.Undeclared(driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: name}, 3)
	store.Undeclared(driver.InstanceKey{Driver: driver.Plugin, NamespacedName: name}, 1)

	want := `
# HELP oikade_undeclared_devices Devices paired with a driver instance that have no Device object.
# TYPE oikade_undeclared_devices gauge
oikade_undeclared_devices{instance="garden",namespace="home"} 3
oikade_undeclared_devices{instance="plugin/garden",namespace="home"} 1
`
	if err := testutil.CollectAndCompare(Collector{Store: store}, strings.NewReader(want), "oikade_undeclared_devices"); err != nil {
		t.Error(err)
	}
}

type fakePlugins map[types.NamespacedName]plugin.Stats

func (f fakePlugins) PluginStats() map[types.NamespacedName]plugin.Stats { return f }

func TestPluginMetrics(t *testing.T) {
	c := Collector{Store: hub.NewStore(logr.Discard()), Plugins: fakePlugins{
		{Namespace: "home", Name: "sonoff"}: {Up: true, Rejected: map[string]uint64{plugin.ReasonUnknownCapability: 2}},
		{Namespace: "home", Name: "cozy"}:   {},
	}}

	want := `
# HELP oikade_plugin_rejected_messages_total Plugin messages dropped by the decoder, by reason.
# TYPE oikade_plugin_rejected_messages_total counter
oikade_plugin_rejected_messages_total{namespace="home",plugin="sonoff",reason="unknown_capability"} 2
# HELP oikade_plugin_up 1 while the plugin's retained status is online with a supported contract.
# TYPE oikade_plugin_up gauge
oikade_plugin_up{namespace="home",plugin="cozy"} 0
oikade_plugin_up{namespace="home",plugin="sonoff"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"oikade_plugin_up", "oikade_plugin_rejected_messages_total"); err != nil {
		t.Error(err)
	}
}
