/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package metrics exports live device state to Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/harry/oikade/internal/capability"
	"github.com/harry/oikade/internal/hub"
)

var (
	deviceLabels     = []string{"namespace", "device", "room"}
	capabilityLabels = append(append([]string{}, deviceLabels...), "capability")

	valueDesc = prometheus.NewDesc("oikade_capability_value",
		"Current value of a device capability. Bools are 0 or 1.", capabilityLabels, nil)
	changesDesc = prometheus.NewDesc("oikade_capability_changes_total",
		"Number of times a capability changed value, including changes between scrapes.", capabilityLabels, nil)
	availableDesc = prometheus.NewDesc("oikade_device_available",
		"1 if the driver reports the device online, 0 if offline. Absent until reported.", deviceLabels, nil)
	lastSeenDesc = prometheus.NewDesc("oikade_device_last_seen_timestamp_seconds",
		"When the driver last heard from the device.", deviceLabels, nil)
	undeclaredDesc = prometheus.NewDesc("oikade_undeclared_devices",
		"Devices paired with a driver instance that have no Device object.", []string{"namespace", "instance"}, nil)
)

// Collector reads the store at scrape time, so series for deleted or renamed devices disappear.
type Collector struct {
	Store *hub.Store
}

var _ prometheus.Collector = Collector{}

// Describe implements prometheus.Collector.
func (Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{valueDesc, changesDesc, availableDesc, lastSeenDesc, undeclaredDesc} {
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
}
