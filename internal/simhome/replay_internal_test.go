package simhome

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/logging"
)

func noRules() ([]*engine.Compiled, map[string]*flow.Flow, error) { return nil, nil, nil }

// A sensor with nothing new to say hasn't gone: a budget's reading would go stale otherwise, however
// busy the rest of the log keeps the clock.
func TestDevicesStayFreshWhileOthersKeepTheClockBusy(t *testing.T) {
	start := time.Date(2030, time.January, 7, 8, 0, 0, 0, time.UTC)
	sim, err := New(Options{
		Home: Home{Compile: noRules}, Start: start, Log: logging.Discard(),
		Devices: []Device{{Name: "quiet", Capabilities: []string{"sensor.temperature"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	for m := 1; m <= 30; m++ {
		if err := sim.replayTo(start.Add(time.Duration(m) * time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	snap, ok := sim.store.Get(key("quiet"))
	if !ok {
		t.Fatal("no device")
	}
	if snap.LastSeen.Before(start.Add(20 * time.Minute)) {
		t.Fatalf("last seen at %s", snap.LastSeen)
	}
}
