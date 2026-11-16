package series

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/VictoriaMetrics/metrics"
)

func TestNameSortsAndEscapesLabels(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   string
	}{
		{nil, "oikade_x"},
		{[]string{"rule", "lamp", "result", "succeeded"}, `oikade_x{result="succeeded",rule="lamp"}`},
		{[]string{"room", `Kid's "den"`}, `oikade_x{room="Kid's \"den\""}`},
		{[]string{"room", "a\\b\nc"}, `oikade_x{room="a\\b\nc"}`},
	} {
		if got := Name("oikade_x", tc.labels...); got != tc.want {
			t.Errorf("Name(%q) = %s, want %s", tc.labels, got, tc.want)
		}
	}
}

func TestASetTakesEscapedNames(t *testing.T) {
	set := metrics.NewSet()
	v := NewVec(set, "oikade_x", "room")
	v.Set(1, `a "quoted", room`)
	v.Set(2, `ends in \`)
	var out bytes.Buffer
	set.WritePrometheus(&out)
	want := "# HELP oikade_x\n# TYPE oikade_x gauge\noikade_x{room=\"a \\\"quoted\\\", room\"} 1\noikade_x{room=\"ends in \\\\\"} 2\n"
	if out.String() != want {
		t.Errorf("wrote\n%s\nwant\n%s", out.String(), want)
	}
}

func TestDeleteMatchingRemovesOnlyThatLabel(t *testing.T) {
	set := metrics.NewSet()
	runs := NewVec(set, "oikade_runs_total", "rule", "result")
	other := NewVec(set, "oikade_runs", "rule")
	for _, rule := range []string{"lamp", "lamp-2", `x",rule="lamp`} {
		runs.Inc(rule, "succeeded")
		runs.Inc(rule, "failed")
	}
	other.Set(1, "lamp")

	runs.DeleteMatching("rule", "lamp")
	want := []string{
		`oikade_runs_total{result="failed",rule="lamp-2"}`, `oikade_runs_total{result="failed",rule="x\",rule=\"lamp"}`,
		`oikade_runs_total{result="succeeded",rule="lamp-2"}`, `oikade_runs_total{result="succeeded",rule="x\",rule=\"lamp"}`,
	}
	if got := runs.Names(); !slices.Equal(got, want) {
		t.Errorf("left %q, want %q", got, want)
	}
	if got := other.Names(); len(got) != 1 {
		t.Errorf("another family lost its series: %q", got)
	}
}

func TestAVecWithoutASetDoesNothing(t *testing.T) {
	v := NewVec(nil, "oikade_x", "rule")
	v.Inc("lamp")
	v.Add(1, "lamp")
	v.Set(1, "lamp")
	v.Delete("lamp")
	v.DeleteMatching("rule", "lamp")
	if v.Names() != nil {
		t.Error("a Vec without a Set has series")
	}
}

func TestAFamilyWritesItsTypeOnceAndKeepsWholeNumbersWhole(t *testing.T) {
	var out bytes.Buffer
	seen := Family{Name: "oikade_seen", Type: TypeGauge}
	seen.Add(2.5, "device", "lamp")
	seen.Add(1763300000, "device", "fan")
	seen.Write(&out)
	(&Family{Name: "oikade_none", Type: TypeGauge}).Write(&out)
	Write(&out, "oikade_up", TypeCounter, 1)
	want := `# HELP oikade_seen
# TYPE oikade_seen gauge
oikade_seen{device="fan"} 1763300000
oikade_seen{device="lamp"} 2.5
# HELP oikade_up
# TYPE oikade_up counter
oikade_up 1
`
	if out.String() != want {
		t.Errorf("wrote\n%s\nwant\n%s", out.String(), want)
	}
}

func TestAWrongNumberOfValuesSaysSo(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), `"device" "capability"`) {
			t.Errorf("panicked with %v", r)
		}
	}()
	NewVec(nil, "oikade_level", "device", "capability").Name("fan")
}
