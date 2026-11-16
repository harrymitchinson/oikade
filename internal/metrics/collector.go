// Package metrics exports live device state to Prometheus.
package metrics

import (
	"io"
	"slices"
	"strings"

	"github.com/VictoriaMetrics/metrics"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver/plugin"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/series"
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
	var values, changes, available, lastSeen, undeclared, up, rejected samples
	for key, s := range c.Store.List() {
		device := []string{"device", key.Name, "room", s.Room}
		for name, v := range s.Values {
			values.add(capability.Float(v), valueFamily, append(device, "capability", name)...)
		}
		for name, n := range s.Changes {
			changes.add(float64(n), changesFamily, append(device, "capability", name)...)
		}
		if s.Online != nil {
			available.add(capability.Float(*s.Online), availableFamily, device...)
		}
		if !s.LastSeen.IsZero() {
			lastSeen.add(float64(s.LastSeen.UnixMilli())/1000, lastSeenFamily, device...)
		}
	}
	for key, n := range c.Store.UndeclaredCounts() {
		undeclared.add(float64(n), undeclaredFamily, "instance", key.Label())
	}
	if c.Plugins != nil {
		for key, s := range c.Plugins.PluginStats() {
			up.add(capability.Float(s.Up), pluginUpFamily, "plugin", key.Name)
			for reason, n := range s.Rejected {
				rejected.add(float64(n), pluginRejectedFamily, "plugin", key.Name, "reason", reason)
			}
		}
	}
	for _, f := range []samples{values, changes, available, lastSeen, undeclared, up, rejected} {
		f.write(w)
	}
}

// samples are one family's samples, written once they're sorted.
type samples []sample

type sample struct {
	name  string
	value float64
}

func (s *samples) add(v float64, family string, labels ...string) {
	*s = append(*s, sample{name: series.Name(family, labels...), value: v})
}

func (s samples) write(w io.Writer) {
	slices.SortFunc(s, func(a, b sample) int { return strings.Compare(a.name, b.name) })
	for _, x := range s {
		series.WriteNamed(w, x.name, x.value)
	}
}
