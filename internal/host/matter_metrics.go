package host

import (
	"io"

	"github.com/VictoriaMetrics/metrics"

	"oikade.dev/internal/matter/clusters"
	"oikade.dev/internal/matter/contain"
	"oikade.dev/internal/matter/im"
	"oikade.dev/internal/series"
)

// writeBuckets are the write duration histogram's upper bounds, in seconds: Prometheus's default
// buckets, which the dashboard's quantiles were made with.
var writeBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// matterMetrics is what the Matter bridge exports. A host has one bridge, so nothing is labelled
// with it, and nothing is labelled with a node ID either, which a controller chooses.
type matterMetrics struct {
	bridge *matterBridge
	// interactions counts attribute writes and commands from controllers, by cluster and the status
	// they were answered with.
	interactions series.Vec
	// reports counts the reports sent to controllers on their subscriptions, the first and
	// keep-alives included.
	reports *metrics.Counter
	// writes is how long a controller's change took to get through arbitration to its device's
	// driver, once gathered, for changes that worked.
	writes *metrics.PrometheusHistogram
	// panics counts panics in handling a Matter message that cost only that message and its
	// session, by where. Each is a bug.
	panics series.Vec
}

func newMatterMetrics(set *metrics.Set, bridge *matterBridge) *matterMetrics {
	m := &matterMetrics{
		bridge:       bridge,
		interactions: series.NewVec(set, "oikade_matter_interactions_total", "interaction", "cluster", "result"),
		reports:      set.NewCounter("oikade_matter_reports_total"),
		writes:       set.NewPrometheusHistogramExt("oikade_matter_write_duration_seconds", writeBuckets),
		panics:       series.NewVec(set, "oikade_matter_panics_total", "site"),
	}
	for _, site := range contain.Parts {
		m.panics.Counter(site) // a series from the start, so a rate has something to go up from
	}
	set.RegisterMetricsWriter(m.writeState)
	return m
}

// Wrote implements im.Observer.
func (m *matterMetrics) Wrote(cluster uint32, st im.Status) {
	m.interactions.Inc("write", clusters.Name(cluster), st.Name())
}

// Invoked implements im.Observer.
func (m *matterMetrics) Invoked(cluster uint32, st im.Status) {
	m.interactions.Inc("invoke", clusters.Name(cluster), st.Name())
}

// Reported implements im.Observer.
func (m *matterMetrics) Reported() { m.reports.Inc() }

// Panicked implements im.Observer.
func (m *matterMetrics) Panicked(site string) { m.panics.Inc(site) }

// writeState writes the bridge's state when it's scraped: whether it serves controllers, and the
// fabrics it's commissioned on (one for each controller's home), the CASE sessions commissioned
// controllers hold and their subscriptions. A bridge whose state couldn't be read has no node, and
// says only that it isn't serving.
func (m *matterMetrics) writeState(w io.Writer) {
	serving := 0.0
	if m.bridge.working() && m.bridge.serving.Load() {
		serving = 1
	}
	series.Write(w, "oikade_matter_serving", series.TypeGauge, serving)
	if !m.bridge.working() {
		return
	}
	n := m.bridge.node
	series.Write(w, "oikade_matter_fabrics", series.TypeGauge, float64(n.Fabrics().Count()))
	series.Write(w, "oikade_matter_sessions", series.TypeGauge, float64(n.Sessions()))
	series.Write(w, "oikade_matter_subscriptions", series.TypeGauge, float64(n.Subscriptions()))
}
