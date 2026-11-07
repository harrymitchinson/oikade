package host

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/contain"
	"github.com/harrymitchinson/oikade/internal/matter/im"
)

// matterMetrics is what the Matter bridge exports. A host has one bridge, so nothing is labelled
// with it, and nothing is labelled with a node ID either, which a controller chooses.
type matterMetrics struct {
	bridge       *matterBridge
	interactions *prometheus.CounterVec
	reports      prometheus.Counter
	writes       prometheus.Histogram

	fabrics, sessions, subscriptions, serving, panics *prometheus.Desc
}

func newMatterMetrics(bridge *matterBridge) *matterMetrics {
	return &matterMetrics{
		bridge: bridge,
		interactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oikade_matter_interactions_total",
			Help: "Attribute writes and commands from controllers, by cluster and the status they were answered with.",
		}, []string{"interaction", "cluster", "result"}),
		reports: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oikade_matter_reports_total",
			Help: "Reports sent to controllers on their subscriptions, the first and keep-alives included.",
		}),
		writes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "oikade_matter_write_duration_seconds",
			Help: "How long a controller's change took to get through arbitration to its device's driver, " +
				"once gathered, for changes that worked.",
			Buckets: prometheus.DefBuckets,
		}),
		fabrics: prometheus.NewDesc("oikade_matter_fabrics",
			"Fabrics the bridge is commissioned on: one for each controller's home.", nil, nil),
		sessions: prometheus.NewDesc("oikade_matter_sessions",
			"CASE sessions that commissioned controllers hold.", nil, nil),
		subscriptions: prometheus.NewDesc("oikade_matter_subscriptions",
			"Subscriptions that controllers hold.", nil, nil),
		serving: prometheus.NewDesc("oikade_matter_serving",
			"1 while the bridge serves controllers, 0 while it can't.", nil, nil),
		panics: prometheus.NewDesc("oikade_matter_panics_total",
			"Panics in handling Matter messages that cost only that message, by where they happened. Each is a bug.",
			[]string{"part"}, nil),
	}
}

// Wrote implements im.Observer.
func (m *matterMetrics) Wrote(cluster uint32, st im.Status) {
	m.interactions.WithLabelValues("write", clusters.Name(cluster), st.Name()).Inc()
}

// Invoked implements im.Observer.
func (m *matterMetrics) Invoked(cluster uint32, st im.Status) {
	m.interactions.WithLabelValues("invoke", clusters.Name(cluster), st.Name()).Inc()
}

// Reported implements im.Observer.
func (m *matterMetrics) Reported() { m.reports.Inc() }

// Describe implements prometheus.Collector.
func (m *matterMetrics) Describe(ch chan<- *prometheus.Desc) {
	m.interactions.Describe(ch)
	m.reports.Describe(ch)
	m.writes.Describe(ch)
	ch <- m.fabrics
	ch <- m.sessions
	ch <- m.subscriptions
	ch <- m.serving
	ch <- m.panics
}

// Collect implements prometheus.Collector. A bridge whose state couldn't be read has no node, and
// says only that it isn't serving.
func (m *matterMetrics) Collect(ch chan<- prometheus.Metric) {
	m.interactions.Collect(ch)
	m.reports.Collect(ch)
	m.writes.Collect(ch)
	serving := 0.0
	if m.bridge.working() && m.bridge.serving.Load() {
		serving = 1
	}
	ch <- prometheus.MustNewConstMetric(m.serving, prometheus.GaugeValue, serving)
	for _, part := range contain.Parts {
		ch <- prometheus.MustNewConstMetric(m.panics, prometheus.CounterValue, float64(contain.Panics(part)), part)
	}
	if !m.bridge.working() {
		return
	}
	n := m.bridge.node
	ch <- prometheus.MustNewConstMetric(m.fabrics, prometheus.GaugeValue, float64(n.Fabrics().Count()))
	ch <- prometheus.MustNewConstMetric(m.sessions, prometheus.GaugeValue, float64(n.Sessions()))
	ch <- prometheus.MustNewConstMetric(m.subscriptions, prometheus.GaugeValue, float64(n.Subscriptions()))
}
