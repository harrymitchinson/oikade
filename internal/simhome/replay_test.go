package simhome_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/framework"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/simhome"
)

const (
	on       = "switch.on"
	humidity = "sensor.relative-humidity"
	contact  = "sensor.contact-open"
)

var monday = time.Date(2030, time.January, 7, 8, 0, 0, 0, time.UTC)

func fanAbove(threshold float64) framework.Home {
	return framework.Home{Holds: []framework.Hold{{
		Name: "fan", Device: "fan", Capability: on, Value: true, OffDelay: 10 * time.Minute,
		When: func(s framework.State) bool { return s.Number("sensor", humidity) > threshold },
	}}}
}

func options(t *testing.T, home framework.Home) simhome.Options {
	t.Helper()
	simulated, err := simhome.FromFramework(home)
	if err != nil {
		t.Fatal(err)
	}
	return simhome.Options{Home: simulated, Start: monday, Log: logr.Discard(), Devices: []simhome.Device{
		{Name: "sensor", Capabilities: []string{humidity}, Values: map[string]any{humidity: 40.0}},
		{Name: "fan", Capabilities: []string{on}, Values: map[string]any{on: false}},
	}}
}

// reading is an outside change: what a sensor did.
type reading struct {
	after time.Duration
	value float64
}

// recorded plays a home for real, as far as a simulation goes, and writes the log a host would have:
// each outside change, each write a moment after what caused it, and the device's answer after that.
func recorded(t *testing.T, home framework.Home, readings []reading) []api.Event {
	t.Helper()
	sim, err := simhome.New(options(t, home))
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	events := []api.Event{{
		Time: monday.Add(time.Second), Type: api.EventChange, Device: "sensor", Capability: humidity, Value: 40.0,
	}}
	seen := 0
	drain := func() {
		writes := sim.Writes()
		for _, w := range writes[seen:] {
			events = append(events,
				api.Event{Time: w.Time.Add(40 * time.Millisecond), Type: api.EventWrite, Device: w.Device, Capability: w.Capability, Value: w.Value},
				api.Event{Time: w.Time.Add(90 * time.Millisecond), Type: api.EventChange, Device: w.Device, Capability: w.Capability, Value: w.Value},
			)
		}
		seen = len(writes)
	}
	at := monday
	for _, r := range readings {
		if err := sim.Advance(monday.Add(r.after).Sub(at)); err != nil {
			t.Fatal(err)
		}
		drain()
		at = monday.Add(r.after)
		events = append(events, api.Event{Time: at, Type: api.EventChange, Device: "sensor", Capability: humidity, Value: r.value})
		if err := sim.Set("sensor", humidity, r.value); err != nil {
			t.Fatal(err)
		}
		drain()
	}
	if err := sim.Advance(monday.Add(end).Sub(at)); err != nil {
		t.Fatal(err)
	}
	drain()
	return events
}

// end is when the logs here stop.
const end = 60 * time.Hour

var damp = []reading{
	{time.Hour, 65}, {2 * time.Hour, 75}, {3 * time.Hour, 50},
	{24 * time.Hour, 80}, {26 * time.Hour, 45}, {50 * time.Hour, 72}, {51 * time.Hour, 40},
}

func head(now time.Time) simhome.ReplayHeader {
	return simhome.ReplayHeader{Log: api.EventLog{Now: now}, Since: monday}
}

func TestUnchangedCodeShowsNoDifferences(t *testing.T) {
	log := recorded(t, fanAbove(70), damp)
	r, err := simhome.Replay(options(t, fanAbove(70)), head(monday.Add(end)), log)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Removed)+len(r.Added) != 0 {
		t.Fatalf("Differences from the same code: removed %v, added %v", r.Removed, r.Added)
	}
	if r.Deployed != 6 || r.Yours != 6 {
		t.Fatalf("%d writes deployed and %d replayed, want 6 of each", r.Deployed, r.Yours)
	}
	if r.Changes != len(damp) {
		t.Fatalf("%d changes went in, want %d: a device's answer to a write isn't one", r.Changes, len(damp))
	}
}

func TestAChangedThresholdShowsExactlyTheWritesThatMoved(t *testing.T) {
	log := recorded(t, fanAbove(70), damp)
	r, err := simhome.Replay(options(t, fanAbove(60)), head(monday.Add(end)), log)
	if err != nil {
		t.Fatal(err)
	}
	// The reading of 65 now turns the fan on an hour sooner. Nothing else moves.
	if len(r.Removed) != 1 || !r.Removed[0].Time.Round(time.Second).Equal(monday.Add(2*time.Hour)) || r.Removed[0].Value != true {
		t.Fatalf("Removed = %v", r.Removed)
	}
	if len(r.Added) != 1 || !r.Added[0].Time.Equal(monday.Add(time.Hour)) || r.Added[0].Value != true {
		t.Fatalf("Added = %v", r.Added)
	}
}

func TestCodeThatStopsWritingLeavesTheDeviceAlone(t *testing.T) {
	log := recorded(t, fanAbove(70), damp)
	r, err := simhome.Replay(options(t, framework.Home{}), head(monday.Add(end)), log)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Removed) != 6 || len(r.Added) != 0 {
		t.Fatalf("removed %v, added %v", r.Removed, r.Added)
	}
}

func TestOnlyWritesSinceAreCompared(t *testing.T) {
	log := recorded(t, fanAbove(70), damp)
	h := head(monday.Add(end))
	h.Since = monday.Add(12 * time.Hour)
	r, err := simhome.Replay(options(t, fanAbove(60)), h, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Removed)+len(r.Added) != 0 || r.Deployed != 4 {
		t.Fatalf("removed %v, added %v, %d deployed", r.Removed, r.Added, r.Deployed)
	}
}

func TestABaselineGivesValuesThatNeverChanged(t *testing.T) {
	h := head(monday.Add(time.Hour))
	h.Log.Baseline = map[string]map[string]any{"sensor": {humidity: 90.0}, "fan": {on: true}}
	log := []api.Event{{Time: monday.Add(time.Minute), Type: api.EventChange, Device: "other", Capability: on, Value: true}}
	r, err := simhome.Replay(options(t, fanAbove(70)), h, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Added)+len(r.Removed) != 0 {
		t.Fatalf("A hold already holding should have nothing new to write: added %v, removed %v", r.Added, r.Removed)
	}
}

// A flow's outside call could do anything, so a replay must not make it. A device write it would
// have made through one is what the check is for.
func TestReplayMakesNoOutsideCalls(t *testing.T) {
	calls := 0
	home := framework.Home{Flows: []framework.Flow{{
		Name: "alarm", On: []framework.Change{{Device: "door", Capability: contact}},
		Run: func(r *framework.FlowRun) error {
			if err := r.Do("notify", nil, func(context.Context) error { calls++; return nil }); err != nil {
				return err
			}
			return r.Set(framework.Set{Device: "fan", Capability: on, Value: true})
		},
	}}}
	opts := options(t, home)
	opts.Devices = append(opts.Devices, simhome.Device{Name: "door", Capabilities: []string{contact}, Values: map[string]any{contact: false}})
	log := []api.Event{
		{Time: monday, Type: api.EventChange, Device: "door", Capability: contact, Value: false},
		{Time: monday.Add(time.Minute), Type: api.EventChange, Device: "door", Capability: contact, Value: true},
	}
	r, err := simhome.Replay(opts, head(monday.Add(time.Hour)), log)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("The flow's outside call ran %d times", calls)
	}
	if r.Skipped["notify"] != 1 {
		t.Fatalf("Skipped = %v", r.Skipped)
	}
	if len(r.Added) != 1 {
		t.Fatalf("Added = %v: the flow should have carried on past the call", r.Added)
	}
}

func TestAFlowTakesTheAnswerItGotLastTime(t *testing.T) {
	home := framework.Home{Flows: []framework.Flow{{
		Name: "weather", On: []framework.Change{{Device: "sensor", Capability: humidity}},
		Run: func(r *framework.FlowRun) error {
			var rain bool
			if err := r.Do("forecast", &rain, func(context.Context) error {
				return errors.New("a replay must not make this call")
			}); err != nil {
				return err
			}
			return r.Set(framework.Set{Device: "fan", Capability: on, Value: rain})
		},
	}}}
	log := []api.Event{
		{Time: monday, Type: api.EventChange, Device: "sensor", Capability: humidity, Value: 40.0},
		{Time: monday.Add(time.Minute), Type: api.EventChange, Device: "sensor", Capability: humidity, Value: 50.0},
		{Time: monday.Add(time.Minute), Type: api.EventCall, Message: "forecast", Value: true},
		{Time: monday.Add(time.Minute), Type: api.EventWrite, Device: "fan", Capability: on, Value: true},
	}
	r, err := simhome.Replay(options(t, home), head(monday.Add(time.Hour)), log)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Skipped)+len(r.Added)+len(r.Removed) != 0 {
		t.Fatalf("skipped %v, added %v, removed %v", r.Skipped, r.Added, r.Removed)
	}
}

func TestNotesSayWhatElseCouldExplainADifference(t *testing.T) {
	log := []api.Event{
		{Time: monday.Add(-time.Minute), Type: api.EventChange, Device: "sensor", Capability: humidity, Value: 41.0},
		{Time: monday.Add(time.Hour), Type: api.EventProcess, Message: "disconnected"},
		{Time: monday.Add(time.Hour + 2*time.Second), Type: api.EventProcess, Message: "connected"},
		{Time: monday.Add(2 * time.Hour), Type: api.EventProcess, Message: "disconnected"},
		{Time: monday.Add(3 * time.Hour), Type: api.EventProcess, Message: "connected"},
		{Time: monday.Add(4 * time.Hour), Type: api.EventDeploy, Message: "abc"},
		{Time: monday.Add(5 * time.Hour), Type: api.EventPause, Rule: "fan", Message: "paused"},
	}
	r, err := simhome.Replay(options(t, fanAbove(70)), head(monday.Add(6*time.Hour)), log)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Notes) != 3 {
		t.Fatalf("Notes = %q, want the long outage, the deploy and the pause", r.Notes)
	}
}
