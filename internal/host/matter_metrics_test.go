package host

import (
	"bytes"
	"strings"
	"testing"

	"github.com/VictoriaMetrics/metrics"

	"oikade.dev/internal/matter/contain"
)

// TestContainedPanicsAreCounted: each panic the node contains is counted by its site, and every
// site has a series from the start.
func TestContainedPanicsAreCounted(t *testing.T) {
	set := metrics.NewSet()
	m := newMatterMetrics(set, nil)
	m.Panicked(contain.MDNS)
	m.Panicked(contain.MDNS)
	m.Panicked(contain.Handler)
	var out bytes.Buffer
	set.WritePrometheus(&out)
	for _, want := range []string{
		`oikade_matter_panics_total{site="mdns"} 2`,
		`oikade_matter_panics_total{site="handler"} 1`,
		`oikade_matter_panics_total{site="receive"} 0`,
		`oikade_matter_panics_total{site="subscription"} 0`,
	} {
		if !strings.Contains(out.String(), want+"\n") {
			t.Errorf("no %s in\n%s", want, out.String())
		}
	}
}
