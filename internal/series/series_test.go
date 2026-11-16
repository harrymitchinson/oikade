package series

import (
	"bytes"
	"slices"
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
	want := "oikade_x{room=\"a \\\"quoted\\\", room\"} 1\noikade_x{room=\"ends in \\\\\"} 2\n"
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

func TestWriteKeepsWholeNumbersWhole(t *testing.T) {
	var out bytes.Buffer
	Write(&out, 1763300000, "oikade_seen", "device", "fan")
	Write(&out, 2.5, "oikade_level")
	if want := "oikade_seen{device=\"fan\"} 1763300000\noikade_level 2.5\n"; out.String() != want {
		t.Errorf("wrote\n%s\nwant\n%s", out.String(), want)
	}
}
