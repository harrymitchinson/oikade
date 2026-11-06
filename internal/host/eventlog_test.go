package host

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/store"
)

// aHouse is a home of 50 devices, as events a day. It's an estimate, as no real home runs on a host
// yet: Zigbee2MQTT's defaults for sensors and plugs, and a family's use of the rest.
var aHouse = []struct {
	name       string
	devices    int
	capability string
	perDay     int
}{
	{"climate", 15, "sensor.temperature", 288}, // every 5 minutes
	{"climate", 15, "sensor.relative-humidity", 288},
	{"plug", 10, "link.quality", 1440}, // a chatty device, every minute
	{"motion", 10, "sensor.occupancy-detected", 60},
	{"light", 15, "light.on", 12},
}

func eventsPerDay() int {
	total := 0
	for _, kind := range aHouse {
		total += kind.devices * kind.perDay
	}
	return total
}

// aDay is a day of aHouse, evenly over the day.
func aDay(day time.Time) []api.Event {
	out := make([]api.Event, 0, eventsPerDay())
	for _, kind := range aHouse {
		for d := range kind.devices {
			device := fmt.Sprintf("%s-%02d", kind.name, d)
			for n := range kind.perDay {
				out = append(out, api.Event{
					Time: day.Add(time.Duration(n) * 24 * time.Hour / time.Duration(kind.perDay)),
					Type: api.EventChange, Device: device, Capability: kind.capability, Value: 21.5 + float64(n%40)/10,
				})
			}
		}
	}
	return out
}

func TestAMonthOfAHouseStaysInsideTheDefaultBound(t *testing.T) {
	dir := t.TempDir()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	log := newEventLog(data.Events(), store.Bound{}, logging.Discard())
	start := time.Now().Add(-30 * 24 * time.Hour)
	for day := range 30 {
		at := start.Add(time.Duration(day) * 24 * time.Hour)
		if err := addAll(log, at, aDay(at)); err != nil {
			t.Fatal(err)
		}
	}
	size, err := data.Events().Size()
	if err != nil {
		t.Fatal(err)
	}
	if size > DefaultEventLogBytes {
		t.Fatalf("the log holds %d bytes, over %d", size, DefaultEventLogBytes)
	}
	file, err := os.Stat(filepath.Join(dir, "oikade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d events a day: %.1f MiB in the log and a %.1f MiB file after a month, bounded at %d days and %d MiB",
		eventsPerDay(), float64(size)/(1<<20), float64(file.Size())/(1<<20), DefaultEventLogAge/(24*time.Hour), DefaultEventLogBytes>>20)
	first, err := data.Events().Read(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the log goes back %.1f days", start.Add(30*24*time.Hour).Sub(first[0].Time).Hours()/24)
}

// addAll flushes a day, an hour's events to a commit. The host commits far oftener, but a month of
// that takes minutes to test.
func addAll(log *eventLog, day time.Time, events []api.Event) error {
	const commits = 24
	sorted := make([][]api.Event, commits)
	for _, e := range events {
		i := int(e.Time.Sub(day) * commits / (24 * time.Hour))
		sorted[i] = append(sorted[i], e)
	}
	for i, batch := range sorted {
		records := make([]store.Record, 0, len(batch))
		for _, e := range batch {
			raw, err := encodeEvent(e)
			if err != nil {
				return err
			}
			records = append(records, store.Record{Time: e.Time, Data: raw})
		}
		now := day.Add(time.Duration(i+1) * 24 * time.Hour / commits)
		if err := log.store.Add(records, now, log.bound, foldBaseline); err != nil {
			return err
		}
	}
	return nil
}

func TestQueueNeverWaitsAndCountsWhatItDrops(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	log := newEventLog(data.Events(), store.Bound{}, logging.Discard())
	for range eventQueue + 10 {
		log.add(api.Event{Time: time.Now(), Type: api.EventChange, Device: "d", Capability: "sensor.temperature", Value: 1.0})
	}
	if log.dropped != 10 {
		t.Fatalf("dropped %d, want 10", log.dropped)
	}
	log.flush()
	if log.dropped != 0 || len(log.queue) != 0 {
		t.Fatalf("after a flush: %d dropped and %d queued", log.dropped, len(log.queue))
	}
	records, err := data.Events().Read(0, eventQueue+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != eventQueue {
		t.Fatalf("%d records, want %d", len(records), eventQueue)
	}
}

func TestDroppedChangesLiveOnInTheBaseline(t *testing.T) {
	raw, err := encodeEvent(api.Event{Type: api.EventChange, Device: "lamp", Capability: "light.on", Value: true})
	if err != nil {
		t.Fatal(err)
	}
	write, err := encodeEvent(api.Event{Type: api.EventWrite, Device: "lamp", Capability: "light.on", Value: false})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := foldBaseline([]byte(`{"sensor":{"sensor.temperature":3}}`), []store.Record{{Data: raw}, {Data: write}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"lamp":{"light.on":true},"sensor":{"sensor.temperature":3}}`; string(baseline) != want {
		t.Fatalf("baseline = %s, want %s: only a change says what a device's value is", baseline, want)
	}
}
