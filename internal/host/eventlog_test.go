package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/clock"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/store"
)

// aHouse is a home of 50 devices, as events a day. It's an estimate, which no real home has
// measured yet: Zigbee2MQTT's defaults for sensors and plugs, and a family's use of the rest.
var aHouse = []struct {
	name       string
	devices    int
	capability string
	perDay     int
}{
	{"climate", 15, "temperature", 288}, // every 5 minutes
	{"climate", 15, "humidity", 288},
	{"plug", 10, "link-quality", 1440}, // a chatty device, every minute
	{"motion", 10, "occupied", 60},
	{"light", 15, "on", 12},
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
				// As the host logs a change: with its ref, and the value before.
				e := api.Event{
					Time: day.Add(time.Duration(n) * 24 * time.Hour / time.Duration(kind.perDay)),
					Type: api.EventChange, Device: device, Capability: kind.capability,
					Value: 21.5 + float64(n%40)/10, Previous: 21.5 + float64((n+39)%40)/10,
				}
				e.Ref = eventRef(e)
				out = append(out, e)
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
		log.add(api.Event{Time: time.Now(), Type: api.EventChange, Device: "d", Capability: "temperature", Value: 1.0})
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
	raw, err := encodeEvent(api.Event{Type: api.EventChange, Device: "lamp", Capability: "on", Value: true})
	if err != nil {
		t.Fatal(err)
	}
	write, err := encodeEvent(api.Event{Type: api.EventWrite, Device: "lamp", Capability: "on", Value: false})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := foldBaseline([]byte(`{"sensor":{"temperature":3}}`), []store.Record{{Data: raw}, {Data: write}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"lamp":{"on":true},"sensor":{"temperature":3}}`; string(baseline) != want {
		t.Fatalf("baseline = %s, want %s: only a change says what a device's value is", baseline, want)
	}
}

// recorded is a stream's lines, as a caller that never goes.
type recorded struct{ lines []any }

func (r *recorded) write(v any) bool { r.lines = append(r.lines, v); return true }
func (r *recorded) flush() bool      { return true }

func TestAStreamCutShortSaysWhyOnlyToARevokedClient(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	log := newEventLog(data.Events(), store.Bound{}, logging.Discard())
	log.add(api.Event{Time: time.Now(), Type: api.EventChange, Device: "kettle", Capability: "on", Value: true})
	fail := func(err error) { t.Fatalf("the log failed: %v", err) }

	revoked, cancel := context.WithCancelCause(t.Context())
	cancel(errRevoked)
	var out recorded
	log.stream(revoked, &out, fail, nil)
	if len(out.lines) != 2 || out.lines[1] != api.LogEnd(errRevoked.Error()) {
		t.Errorf("a revoked client's stream: %+v", out.lines)
	}

	// A host that stops says nothing, so the caller reads it as one that stopped.
	stopped, stop := context.WithCancel(t.Context())
	stop()
	out = recorded{}
	log.stream(stopped, &out, fail, nil)
	if len(out.lines) != 1 {
		t.Errorf("a stopped host's stream: %+v", out.lines)
	}

	out = recorded{}
	log.stream(t.Context(), &out, fail, nil)
	if len(out.lines) != 3 || out.lines[2] != api.LogEnd("") {
		t.Errorf("a whole stream: %+v", out.lines)
	}
}

// The log gives back each event as it was reported: a change with the value before it, and every
// event with its ref, though the log doesn't keep a ref it can work out.
func TestTheLogGivesBackWhatWasReported(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	log := newEventLog(data.Events(), store.Bound{}, logging.Discard())
	at := time.Now().Truncate(time.Second)
	reported := []api.Event{
		{Type: api.EventChange, Device: "kettle", Capability: "on", Value: true, Previous: false},
		{Type: api.EventWrite, Device: "kettle", Capability: "on", Value: false, By: "flow/morning"},
		{Type: api.EventAvailability, Device: "kettle", Message: api.AvailabilityOffline, Result: "no LAN key"},
		{Type: api.EventPlugin, Ref: "plugin/overkiz", Message: "login refused"},
		{Type: api.EventMatter, Message: api.MatterControllerPaired, Value: 2.0},
		{Type: api.EventForget, Device: "kettle"},
		{Type: api.EventRename, Device: "tea-kettle", Previous: "kettle"},
		{Type: api.EventRule, Rule: "morning", Result: "succeeded"},
	}
	for i := range reported {
		reported[i].Time = at.Add(time.Duration(i) * time.Second)
		if reported[i].Ref == "" {
			reported[i].Ref = eventRef(reported[i])
		}
		log.add(reported[i])
	}
	var out recorded
	log.stream(t.Context(), &out, func(err error) { t.Fatalf("the log failed: %v", err) }, nil)
	if len(out.lines) != len(reported)+2 {
		t.Fatalf("%d lines, want the opening, %d events and the end", len(out.lines), len(reported))
	}
	for i, want := range reported {
		got, ok := out.lines[i+1].(api.Event)
		if !ok || !got.Time.Equal(want.Time) {
			t.Fatalf("line %d is %+v, want %+v", i+1, out.lines[i+1], want)
		}
		got.Time = want.Time
		if !reflect.DeepEqual(got, want) {
			t.Errorf("event %d came back as %+v, want %+v", i, got, want)
		}
	}
	if raw, err := encodeEvent(reported[0]); err != nil || strings.Contains(string(raw), "ref") {
		t.Errorf("a change is kept as %s, %v: its ref only repeats its device and capability", raw, err)
	}
}

// Events reach the store every 5 seconds, so a crash loses at most that long, as
// docs/architecture.md says: one added just after a flush is on disk by the next, and not before.
func TestEventsReachTheStoreWithinAFlush(t *testing.T) {
	const promised = 5 * time.Second
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	log := newEventLog(data.Events(), store.Bound{}, logging.Discard())
	c := clock.NewFake(time.Now())
	log.clock = c
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { log.run(ctx); close(stopped) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("waited 5s for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	saved := func() int {
		records, err := data.Events().Read(0, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(records)
	}
	for want := 1; want <= 2; want++ {
		waitFor("the next flush to be set", c.HasWaiters)
		log.add(api.Event{Time: time.Now(), Type: api.EventChange, Device: "d", Capability: "temperature", Value: float64(want)})
		c.Step(promised - time.Millisecond)
		if got := saved(); got != want-1 {
			t.Fatalf("%d events saved before the flush, want %d", got, want-1)
		}
		c.Step(time.Millisecond)
		waitFor("the event to be saved", func() bool { return saved() == want })
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the flushes went on after the host stopped")
	}
}

// A tail that starts with the log's past has every event once, in order: the log's up to where the
// tail began, then the feed's, though events and flushes keep coming as it starts.
func TestATailsPastMeetsItsFeedWithNoGapOrRepeat(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	srv := &server{feed: newFeed(), events: newEventLog(data.Events(), store.Bound{}, logging.Discard())}
	const total = 2000
	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		for n := range total {
			srv.emit(api.Event{Time: time.Now(), Type: api.EventChange, Device: "counter", Capability: "brightness", Value: float64(n)})
			if n%97 == 0 {
				// As the host's own flush does, whenever it likes.
				srv.events.flush()
			}
		}
	}()
	for range 20 {
		events, stop, upTo, err := srv.tailWithPast()
		if err != nil {
			t.Fatal(err)
		}
		var past recorded
		if short, told := srv.events.history(t.Context(), &past, upTo, nil); short != "" || !told {
			t.Fatalf("the past stopped short: %q, told %t", short, told)
		}
		next := 0.0
		for _, line := range past.lines {
			if v := line.(api.Event).Value; v != next {
				t.Fatalf("the past has %v where %v should be", v, next)
			}
			next++
		}
		// The feed carries on from exactly there. Its next event shows it, unless they've all been, or
		// so many came while the past was read that the feed cut this tail off.
		carriesOn := func(e api.Event, open bool) {
			if open && e.Value != next {
				t.Fatalf("the feed starts at %v after a past of %v events", e.Value, next)
			}
		}
		select {
		case e, open := <-events:
			carriesOn(e, open)
		case <-emitted:
			select {
			case e, open := <-events:
				carriesOn(e, open)
			default:
				if next != total {
					t.Fatalf("the past has %v events and the feed none, of %d", next, total)
				}
			}
		}
		stop()
	}
	<-emitted
}

// A tail writing the log's past has its feed's events kept for it, in order, where the feed alone
// would have cut it off, and is cut off only when even that's full.
func TestATailsFeedWaitsWhileItWritesItsPast(t *testing.T) {
	f := newFeed()
	sub, stop := f.subscribe()
	defer stop()
	events := queued(sub, 4*feedBuffer)
	publish := func(from, to int, paced bool) {
		for n := from; n < to; n++ {
			f.publish(api.Event{Type: api.EventChange, Value: float64(n)})
			// The queue takes each as it comes: paced, the feed's own buffer never fills.
			for paced && len(sub) == feedBuffer {
				time.Sleep(time.Millisecond)
			}
		}
	}
	publish(0, 3*feedBuffer, true)
	for n := range 3 * feedBuffer {
		if e, open := <-events; !open || e.Value != float64(n) {
			t.Fatalf("event %d came as %+v, open %t", n, e, open)
		}
	}
	// Nobody reads, and more come than even the queue holds.
	publish(3*feedBuffer, 9*feedBuffer, false)
	last := -1.0
	for e := range events {
		last = e.Value.(float64)
	}
	if last >= float64(9*feedBuffer-1) {
		t.Errorf("the tail was never cut off: it read to %v", last)
	}
}
