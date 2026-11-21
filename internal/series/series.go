// Package series names labelled series for VictoriaMetrics/metrics, which takes each series by its
// full name, labels and all.
//
// Importing it has every Set write # TYPE lines, which Prometheus and Grafana read the type from.
package series

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/VictoriaMetrics/metrics"
)

func init() { metrics.ExposeMetadata(true) }

const (
	TypeCounter = "counter"
	TypeGauge   = "gauge"
)

// Name returns a series' full name: family, then labels as name and value pairs, sorted by name as
// Prometheus writes them, with values escaped.
func Name(family string, labels ...string) string {
	if len(labels)%2 != 0 {
		panic(fmt.Sprintf("series %s has a label without a value: %q", family, labels))
	}
	names, values := make([]string, 0, len(labels)/2), make([]string, 0, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		names, values = append(names, labels[i]), append(values, labels[i+1])
	}
	return NewVec(nil, family, names...).Name(values...)
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(v string) string { return escaper.Replace(v) }

// Vec is a family of series in a Set, one for each set of label values. Its methods do nothing on a
// nil Set, so a caller that exports nothing needn't check.
type Vec struct {
	set    *metrics.Set
	family string
	labels []string
	// order is the indexes of labels sorted by name, the order a name has them in.
	order []int
}

// NewVec returns the family's series in set, labelled with labels in this order.
func NewVec(set *metrics.Set, family string, labels ...string) Vec {
	order := make([]int, len(labels))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return strings.Compare(labels[a], labels[b]) })
	return Vec{set: set, family: family, labels: labels, order: order}
}

// Name returns the series for values, one for each of the Vec's labels.
func (v Vec) Name(values ...string) string {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("series %s has labels %q, and was given values %q", v.family, v.labels, values))
	}
	if len(values) == 0 {
		return v.family
	}
	var b strings.Builder
	b.WriteString(v.family)
	for n, i := range v.order {
		if n == 0 {
			b.WriteByte('{')
		} else {
			b.WriteByte(',')
		}
		b.WriteString(v.labels[i])
		b.WriteString(`="`)
		b.WriteString(escape(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// Counter returns the counter for values, making it at 0 if it's new. The Set mustn't be nil.
func (v Vec) Counter(values ...string) *metrics.Counter {
	return v.set.GetOrCreateCounter(v.Name(values...))
}

// FloatCounter returns the float counter for values, making it at 0 if it's new. The Set mustn't be
// nil.
func (v Vec) FloatCounter(values ...string) *metrics.FloatCounter {
	return v.set.GetOrCreateFloatCounter(v.Name(values...))
}

// Gauge returns the gauge for values, making it at 0 if it's new. The Set mustn't be nil.
func (v Vec) Gauge(values ...string) *metrics.Gauge {
	return v.set.GetOrCreateGauge(v.Name(values...), nil)
}

func (v Vec) Inc(values ...string) {
	if v.set != nil {
		v.Counter(values...).Inc()
	}
}

// Add adds n to the float counter for values. Adding 0 makes it, so it's exported before it counts.
func (v Vec) Add(n float64, values ...string) {
	if v.set != nil {
		v.FloatCounter(values...).Add(n)
	}
}

func (v Vec) Set(n float64, values ...string) {
	if v.set != nil {
		v.Gauge(values...).Set(n)
	}
}

func (v Vec) Delete(values ...string) {
	if v.set != nil {
		v.set.UnregisterMetric(v.Name(values...))
	}
}

// DeleteMatching removes every series in the family whose label is value. Values are escaped, so a
// quote in a name is never one that ends a value, and a pair found in it is a label's.
func (v Vec) DeleteMatching(label, value string) {
	pair := label + `="` + escape(value) + `"`
	for _, name := range v.Names() {
		if hasPair(name[len(v.family):], pair) {
			v.set.UnregisterMetric(name)
		}
	}
}

// hasPair reports whether labels, as {a="b",c="d"}, has pair as one of them.
func hasPair(labels, pair string) bool {
	for _, before := range []string{"{", ","} {
		for _, after := range []string{",", "}"} {
			if strings.Contains(labels, before+pair+after) {
				return true
			}
		}
	}
	return false
}

// Names returns the family's series with labels, sorted.
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

// Family is one family's samples for a writer that reads its values when it's scraped, and writes
// them once they're sorted, after its # HELP and # TYPE lines.
type Family struct {
	Name, Type string
	samples    []sample
}

type sample struct {
	name  string
	value float64
}

// Add adds the sample for labels, given as name and value pairs.
func (f *Family) Add(value float64, labels ...string) {
	f.samples = append(f.samples, sample{Name(f.Name, labels...), value})
}

// Write writes the family, or nothing when it has no samples.
func (f *Family) Write(w io.Writer) {
	if len(f.samples) == 0 {
		return
	}
	slices.SortFunc(f.samples, func(a, b sample) int { return strings.Compare(a.name, b.name) })
	metrics.WriteMetadataIfNeeded(w, f.Name, f.Type)
	for _, s := range f.samples {
		// As the Set writes a gauge: whole numbers without an exponent, so a timestamp reads as one.
		if s.value == math.Trunc(s.value) && math.Abs(s.value) < 1<<63 {
			_, _ = fmt.Fprintf(w, "%s %d\n", s.name, int64(s.value))
		} else {
			_, _ = fmt.Fprintf(w, "%s %g\n", s.name, s.value)
		}
	}
}

// Write writes a family of one sample without labels.
func Write(w io.Writer, family, typ string, value float64) {
	f := Family{Name: family, Type: typ}
	f.Add(value)
	f.Write(w)
}
