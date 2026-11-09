package simhome_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/framework"
	"github.com/harrymitchinson/oikade/internal/simhome"
)

// syncBuffer is a log the engine's goroutines can write to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAHoldWhoseRoomLosesItsLastMemberWarnsOnce(t *testing.T) {
	occupied := string(framework.Occupied)
	home := framework.NewHome(framework.Options{}, framework.Hold{
		Name: "hall-lamp", Set: switchOf("lamp").To(true), OffDelay: time.Minute,
		When: func(s framework.State) bool { return s.Any(framework.InRoom("hall", framework.Occupied)) },
	})
	simulated, err := simhome.FromFramework(home)
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	sim, err := simhome.New(simhome.Options{
		Home: simulated, Start: monday, Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Devices: []simhome.Device{
			{Name: "sensor", Room: "hall", Capabilities: []string{occupied}, Values: map[string]any{occupied: true}},
			{Name: "lamp", Capabilities: []string{on}, Values: map[string]any{on: false}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	if err := sim.Set("sensor", occupied, false); err != nil {
		t.Fatal(err)
	}
	// In its off-delay, the hall's only sensor moves out, and the hold looks again more than once.
	if err := sim.Move("sensor", "attic"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := sim.Advance(time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	const warned = `level=WARN msg="Rule read a Room with no members"`
	if got := strings.Count(logs.String(), warned); got != 1 {
		t.Fatalf("warned %d times, want once:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "room=hall") {
		t.Fatalf("the warning doesn't name the room:\n%s", logs.String())
	}

	// Back in the hall, then out again, is another time it happens.
	if err := sim.Move("sensor", "hall"); err != nil {
		t.Fatal(err)
	}
	if err := sim.Move("sensor", "attic"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(logs.String(), warned); got != 2 {
		t.Fatalf("warned %d times after it emptied again, want twice:\n%s", got, logs.String())
	}
}
