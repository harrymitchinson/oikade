// Package metrics exports live device state to Prometheus.
package metrics

import (
	"io"

	"github.com/VictoriaMetrics/metrics"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/driver/plugin"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/series"
)

// The families a Collector writes. docs/metrics.md says what each one is.
const (
	// valueFamily is a capability's current value. Bools are 0 or 1.
	valueFamily = "oikade_capability_value"
	// changesFamily counts the times a capability changed value, including changes between scrapes.
	changesFamily = "oikade_capability_changes_total"
	// availableFamily is 1 if the driver reports the device online, and 0 if offline. It's absent
	// until reported.
	availableFamily = "oikade_device_available"
	// lastSeenFamily is when the driver last heard from the device.
	lastSeenFamily = "oikade_device_last_seen_timestamp_seconds"
	// undeclaredFamily is the devices paired with a driver instance that aren't on record.
	undeclaredFamily = "oikade_undeclared_devices"
	// pluginUpFamily is 1 while the plugin's retained status is online with a supported contract.
	pluginUpFamily = "oikade_plugin_up"
	// pluginRejectedFamily counts plugin messages dropped by the decoder, by reason.
	pluginRejectedFamily = "oikade_plugin_rejected_messages_total"
)

// PluginStatser reports the decoder's view of every running Plugin.
type PluginStatser interface {
	PluginStats() map[ident.ID]plugin.Stats
}

// Collector reads the store and plugins at scrape time, so series for deleted or renamed devices
// and plugins disappear.
type Collector struct {
	Store *hub.Store
	// Plugins is optional.
	Plugins PluginStatser
}

// Register makes set write c's series whenever it's written.
func (c Collector) Register(set *metrics.Set) { set.RegisterMetricsWriter(c.Write) }

// Write writes c's series, a family at a time and each family sorted, as Prometheus expects.
func (c Collector) Write(w io.Writer) {
	values := series.Family{Name: valueFamily, Type: series.TypeGauge}
	changes := series.Family{Name: changesFamily, Type: series.TypeCounter}
	available := series.Family{Name: availableFamily, Type: series.TypeGauge}
	lastSeen := series.Family{Name: lastSeenFamily, Type: series.TypeGauge}
	undeclared := series.Family{Name: undeclaredFamily, Type: series.TypeGauge}
	up := series.Family{Name: pluginUpFamily, Type: series.TypeGauge}
	rejected := series.Family{Name: pluginRejectedFamily, Type: series.TypeCounter}
	for key, s := range c.Store.List() {
		device := []string{"device", key.Name, "room", s.Room}
		for name, v := range s.Values {
			values.Add(capability.Float(v), append(device, "capability", name)...)
		}
		for name, n := range s.Changes {
			changes.Add(float64(n), append(device, "capability", name)...)
		}
		if s.Online != nil {
			available.Add(capability.Float(*s.Online), device...)
		}
		if !s.LastSeen.IsZero() {
			lastSeen.Add(float64(s.LastSeen.UnixMilli())/1000, device...)
		}
	}
	for key, n := range c.Store.UndeclaredCounts() {
		undeclared.Add(float64(n), "instance", key.Label())
	}
	if c.Plugins != nil {
		for key, s := range c.Plugins.PluginStats() {
			up.Add(capability.Float(s.Up), "plugin", key.Name)
			for reason, n := range s.Rejected {
				rejected.Add(float64(n), "plugin", key.Name, "reason", reason)
			}
		}
	}
	for _, f := range []*series.Family{&values, &changes, &available, &lastSeen, &undeclared, &up, &rejected} {
		f.Write(w)
	}
}
