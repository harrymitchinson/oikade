// Package metrics exports live device state to Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver/plugin"
	"github.com/harrymitchinson/oikade/internal/hub"
)

const namespaceLabel = "namespace"

var (
	deviceLabels     = []string{namespaceLabel, "device", "room"}
	capabilityLabels = append(append([]string{}, deviceLabels...), "capability")
	pluginLabels     = []string{namespaceLabel, "plugin"}
	rejectedLabels   = append(append([]string{}, pluginLabels...), "reason")

	valueDesc = prometheus.NewDesc("oikade_capability_value",
		"Current value of a device capability. Bools are 0 or 1.", capabilityLabels, nil)
	changesDesc = prometheus.NewDesc("oikade_capability_changes_total",
		"Number of times a capability changed value, including changes between scrapes.", capabilityLabels, nil)
	availableDesc = prometheus.NewDesc("oikade_device_available",
		"1 if the driver reports the device online, 0 if offline. Absent until reported.", deviceLabels, nil)
	lastSeenDesc = prometheus.NewDesc("oikade_device_last_seen_timestamp_seconds",
		"When the driver last heard from the device.", deviceLabels, nil)
	undeclaredDesc = prometheus.NewDesc("oikade_undeclared_devices",
		"Devices paired with a driver instance that aren't on record.", []string{namespaceLabel, "instance"}, nil)
	pluginUpDesc = prometheus.NewDesc("oikade_plugin_up",
		"1 while the plugin's retained status is online with a supported contract.", pluginLabels, nil)
	pluginRejectedDesc = prometheus.NewDesc("oikade_plugin_rejected_messages_total",
		"Plugin messages dropped by the decoder, by reason.", rejectedLabels, nil)
)

// PluginStatser reports the decoder's view of every running Plugin.
type PluginStatser interface {
	PluginStats() map[types.NamespacedName]plugin.Stats
}

// Collector reads the store and plugins at scrape time, so series for deleted or renamed devices
// and plugins disappear.
type Collector struct {
	Store *hub.Store
	// Plugins is optional.
	Plugins PluginStatser
}

var _ prometheus.Collector = Collector{}

// Describe implements prometheus.Collector.
func (Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		valueDesc, changesDesc, availableDesc, lastSeenDesc, undeclaredDesc, pluginUpDesc, pluginRejectedDesc,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c Collector) Collect(ch chan<- prometheus.Metric) {
	for key, s := range c.Store.List() {
		labels := []string{key.Namespace, key.Name, s.Room}
		for name, v := range s.Values {
			ch <- prometheus.MustNewConstMetric(valueDesc, prometheus.GaugeValue, capability.Float(v), append(labels, name)...)
		}
		for name, n := range s.Changes {
			ch <- prometheus.MustNewConstMetric(changesDesc, prometheus.CounterValue, float64(n), append(labels, name)...)
		}
		if s.Online != nil {
			ch <- prometheus.MustNewConstMetric(availableDesc, prometheus.GaugeValue, capability.Float(*s.Online), labels...)
		}
		if !s.LastSeen.IsZero() {
			ch <- prometheus.MustNewConstMetric(lastSeenDesc, prometheus.GaugeValue,
				float64(s.LastSeen.UnixMilli())/1000, labels...)
		}
	}
	for key, n := range c.Store.UndeclaredCounts() {
		ch <- prometheus.MustNewConstMetric(undeclaredDesc, prometheus.GaugeValue, float64(n), key.Namespace, key.Label())
	}
	if c.Plugins == nil {
		return
	}
	for key, s := range c.Plugins.PluginStats() {
		ch <- prometheus.MustNewConstMetric(pluginUpDesc, prometheus.GaugeValue, capability.Float(s.Up), key.Namespace, key.Name)
		for reason, n := range s.Rejected {
			ch <- prometheus.MustNewConstMetric(pluginRejectedDesc, prometheus.CounterValue, float64(n),
				key.Namespace, key.Name, reason)
		}
	}
}
