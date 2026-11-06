package main

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/simhome"
)

func TestRenderShowsWritesInOrderWithTheirSign(t *testing.T) {
	at := time.Date(2030, time.January, 7, 8, 0, 0, 0, time.UTC)
	got := render(simhome.Report{
		From: at, To: at.Add(time.Hour), Changes: 3, Deployed: 1, Yours: 1,
		Removed: []simhome.Write{{Time: at.Add(2 * time.Minute), Device: "fan", Capability: "switch.on", Value: true}},
		Added:   []simhome.Write{{Time: at.Add(time.Minute), Device: "fan", Capability: "switch.on", Value: true}},
		Skipped: map[string]int{"forecast": 2},
		Notes:   []string{"Rule fan was paused"},
	}, time.UTC)
	want := `Replayed Mon 7 Jan 08:00:00 to Mon 7 Jan 09:00:00: 3 changes. The deployed version wrote 1 times, your code 1.
+ Mon 7 Jan 08:01:00 fan switch.on = true
- Mon 7 Jan 08:02:00 fan switch.on = true
Skipped forecast 2 times: a replay makes no outside calls, and has no answer on record
Note: Rule fan was paused
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderSaysSoWhenNothingDiffers(t *testing.T) {
	if got := render(simhome.Report{}, time.UTC); !strings.Contains(got, "No differences.") {
		t.Fatal(got)
	}
}
