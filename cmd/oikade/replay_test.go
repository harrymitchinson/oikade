package main

import (
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/simhome"
)

func TestRenderShowsWritesInOrderWithTheirSign(t *testing.T) {
	at := time.Date(2030, time.January, 7, 8, 0, 0, 0, time.UTC)
	got := render(simhome.Report{
		From: at, To: at.Add(time.Hour), Changes: 3, Deployed: 1, Yours: 1,
		Removed: []simhome.Write{{Time: at.Add(2 * time.Minute), Device: "hue/0x1", Capability: "on", Value: true}},
		Added:   []simhome.Write{{Time: at.Add(time.Minute), Device: "fan", Capability: "on", Value: true}},
		Skipped: map[string]int{"forecast": 2},
		Notes:   []string{"Rule hue/0x1 on on dusk was paused", "Rule hue/0x10 on on dusk was paused"},
	}, time.UTC, map[string]string{"hue/0x1": "hall-lamp", "hue/0x10": "porch-lamp", "fan": "fan"})
	// The replay has each device by its key, and a person reads its name.
	want := `Replayed Mon 7 Jan 08:00:00 to Mon 7 Jan 09:00:00: 3 changes. The deployed version wrote 1 times, your code 1.
+ Mon 7 Jan 08:01:00 fan on = true
- Mon 7 Jan 08:02:00 hall-lamp on = true
Skipped forecast 2 times: a replay makes no outside calls, and has no answer on record
Note: Rule hall-lamp on on dusk was paused
Note: Rule porch-lamp on on dusk was paused
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderSaysSoWhenNothingDiffers(t *testing.T) {
	if got := render(simhome.Report{}, time.UTC, nil); !strings.Contains(got, "No differences.") {
		t.Fatal(got)
	}
}
