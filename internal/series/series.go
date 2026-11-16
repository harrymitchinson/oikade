// Package series names labelled series for VictoriaMetrics/metrics, which takes each series by its
// full name, labels and all.
package series

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/VictoriaMetrics/metrics"
)

// Name returns a series' full name: family, then labels as name and value pairs, sorted by name as
// Prometheus writes them, with values escaped.
func Name(family string, labels ...string) string {
	if len(labels) == 0 {
		return family
	}
	pairs := make([][2]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		pairs = append(pairs, [2]string{labels[i], labels[i+1]})
	}
	slices.SortStableFunc(pairs, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	var b strings.Builder
	b.WriteString(family)
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p[0])
		b.WriteString(`="`)
		b.WriteString(escape(p[1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(v string) string { return escaper.Replace(v) }

// Vec is a family of series in a Set, one for each set of label values. Its methods do nothing on a
// nil Set, so a caller that exports nothing needn't check.
type Vec struct {
	set    *metrics.Set
	family string
	labels []string
}

// NewVec returns the family's series in set, labelled with labels in this order.
func NewVec(set *metrics.Set, family string, labels ...string) Vec {
	return Vec{set: set, family: family, labels: labels}
}

// Name returns the series for values, one for each of the Vec's labels.
func (v Vec) Name(values ...string) string {
	pairs := make([]string, 0, 2*len(v.labels))
	for i, l := range v.labels {
		pairs = append(pairs, l, values[i])
	}
	return Name(v.family, pairs...)
}

// Counter returns the counter for values, making it at 0 if it's new. The Set mustn't be nil.
func (v Vec) Counter(values ...string) *metrics.Counter {
	return v.set.GetOrCreateCounter(v.Name(values...))
}

// FloatCounter returns the float counter for values, making it at 0 if it's new. The Set mustn't be nil.
func (v Vec) FloatCounter(values ...string) *metrics.FloatCounter {
	return v.set.GetOrCreateFloatCounter(v.Name(values...))
}

// Gauge returns the gauge for values, making it at 0 if it's new. The Set mustn't be nil.
func (v Vec) Gauge(values ...string) *metrics.Gauge {
	return v.set.GetOrCreateGauge(v.Name(values...), nil)
}

// Inc adds one to the counter for values.
func (v Vec) Inc(values ...string) {
	if v.set != nil {
		v.Counter(values...).Inc()
	}
}

// Add adds n to the float counter for values.
func (v Vec) Add(n float64, values ...string) {
	if v.set != nil {
		v.FloatCounter(values...).Add(n)
	}
}

// Set sets the gauge for values to n.
func (v Vec) Set(n float64, values ...string) {
	if v.set != nil {
		v.Gauge(values...).Set(n)
	}
}

// Delete removes the series for values.
func (v Vec) Delete(values ...string) {
	if v.set != nil {
		v.set.UnregisterMetric(v.Name(values...))
	}
}

// DeleteMatching removes every series in the family whose label is value.
func (v Vec) DeleteMatching(label, value string) {
	for _, name := range v.Names() {
		if hasLabel(name, label, value) {
			v.set.UnregisterMetric(name)
		}
	}
}

// Names returns the family's series, sorted.
func (v Vec) Names() []string {
	if v.set == nil {
		return nil
	}
	var out []string
	for _, name := range v.set.ListMetricNames() {
		if strings.HasPrefix(name, v.family+"{") {
			out = append(out, name)
		}
	}
	return out
}

func hasLabel(name, label, value string) bool {
	pair := label + `="` + escape(value) + `"`
	labels := name[strings.IndexByte(name, '{'):]
	return strings.Contains(labels, "{"+pair+",") || strings.Contains(labels, "{"+pair+"}") ||
		strings.Contains(labels, ","+pair+",") || strings.Contains(labels, ","+pair+"}")
}

// Write writes one sample, for a writer that reads its values when it's scraped. Whole numbers
// are written without an exponent, as the Set writes them.
func Write(w io.Writer, value float64, family string, labels ...string) {
	WriteNamed(w, Name(family, labels...), value)
}

// WriteNamed writes one sample of the series name.
func WriteNamed(w io.Writer, name string, value float64) {
	if value == math.Trunc(value) && math.Abs(value) < 1<<63 {
		_, _ = fmt.Fprintf(w, "%s %d\n", name, int64(value))
		return
	}
	_, _ = fmt.Fprintf(w, "%s %g\n", name, value)
}
