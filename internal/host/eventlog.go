package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/store"
)

const (
	// DefaultEventLogAge keeps a week to replay, and a week before it for rules to settle into.
	DefaultEventLogAge = 14 * 24 * time.Hour
	// DefaultEventLogBytes is the most the log holds. docs/architecture.md has what it's sized on.
	DefaultEventLogBytes = 64 << 20

	// eventFlush is how often events reach the store. One commit a few seconds apart is what keeps
	// the log out of the journal's way, and a crash loses at most that many seconds of history.
	eventFlush = 5 * time.Second
	// eventQueue bounds what waits for the next flush. Past it events are dropped, not waited for.
	eventQueue = 4096
	// eventPage is how many events a read takes at once, each page in a transaction of its own.
	eventPage = 1024
)

// eventLog keeps what happened for oikade replay. Adding only appends to a queue, so nothing that
// reports an event ever waits for the disk.
type eventLog struct {
	store *store.Events
	bound store.Bound
	log   logr.Logger

	mu      sync.Mutex
	queue   []api.Event
	dropped int
	// flushing keeps two flushes from reordering what they add.
	flushing sync.Mutex
}

func newEventLog(events *store.Events, bound store.Bound, log logr.Logger) *eventLog {
	if bound.MaxAge <= 0 {
		bound.MaxAge = DefaultEventLogAge
	}
	if bound.MaxBytes == 0 {
		bound.MaxBytes = DefaultEventLogBytes
	}
	return &eventLog{store: events, bound: bound, log: log}
}

func (l *eventLog) add(e api.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) >= eventQueue {
		l.dropped++
		return
	}
	l.queue = append(l.queue, e)
}

// run flushes until ctx ends. The host flushes once more when everything has stopped.
func (l *eventLog) run(ctx context.Context) {
	tick := time.NewTicker(eventFlush)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			l.flush()
		case <-ctx.Done():
			return
		}
	}
}

// flush commits what's queued, and applies the bound even if nothing is. A failure loses those events
// and nothing else: the log is history, and a home runs without it.
func (l *eventLog) flush() {
	l.flushing.Lock()
	defer l.flushing.Unlock()
	l.mu.Lock()
	queue, dropped := l.queue, l.dropped
	l.queue, l.dropped = nil, 0
	l.mu.Unlock()
	if dropped > 0 {
		l.log.Info("Dropped events from the log; they came too fast", "events", dropped)
	}
	records := make([]store.Record, 0, len(queue))
	for _, e := range queue {
		raw, err := encodeEvent(e)
		if err != nil {
			l.log.Error(err, "Failed to encode event", "type", e.Type)
			continue
		}
		records = append(records, store.Record{Time: e.Time, Data: raw})
	}
	if err := l.store.Add(records, time.Now(), l.bound, foldBaseline); err != nil {
		l.log.Error(err, "Failed to add to the event log", "events", len(records))
	}
}

// encodeEvent is an event as the log keeps it. The store has its time, and nothing reads Previous.
func encodeEvent(e api.Event) ([]byte, error) {
	e.Time, e.Previous = time.Time{}, nil
	return json.Marshal(e)
}

// foldBaseline keeps the last value of everything being dropped, because a device that hasn't changed
// since still has it.
func foldBaseline(baseline []byte, dropped []store.Record) ([]byte, error) {
	values := map[string]map[string]any{}
	if baseline != nil {
		if err := json.Unmarshal(baseline, &values); err != nil {
			return nil, fmt.Errorf("reading the baseline: %w", err)
		}
	}
	for _, r := range dropped {
		var e api.Event
		if err := json.Unmarshal(r.Data, &e); err != nil {
			return nil, fmt.Errorf("reading event %d: %w", r.Seq, err)
		}
		if e.Type != api.EventChange {
			continue
		}
		if values[e.Device] == nil {
			values[e.Device] = map[string]any{}
		}
		values[e.Device][e.Capability] = e.Value
	}
	return json.Marshal(values)
}

// stream writes the log to conn, in pages. Events added meanwhile are included, and it ends when it
// catches up.
func (l *eventLog) stream(conn net.Conn) {
	l.flush()
	out := json.NewEncoder(conn)
	write := func(v any) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		return out.Encode(v) == nil
	}
	now := time.Now()
	head := api.EventLog{Now: now, Zone: zoneName()}
	_, head.Offset = now.Zone()
	raw, err := l.store.Baseline()
	if err == nil && raw != nil {
		err = json.Unmarshal(raw, &head.Baseline)
	}
	if err != nil {
		write(api.EventLog{Error: fmt.Sprintf("reading the event log: %v", err)})
		return
	}
	if !write(head) {
		return
	}
	var after uint64
	for {
		page, err := l.store.Read(after, eventPage)
		if err != nil {
			write(api.LogEnd(err.Error()))
			return
		}
		if len(page) == 0 {
			write(api.LogEnd(""))
			return
		}
		for _, r := range page {
			var e api.Event
			if err := json.Unmarshal(r.Data, &e); err != nil {
				write(api.LogEnd(fmt.Sprintf("event %d: %v", r.Seq, err)))
				return
			}
			e.Time = r.Time
			if !write(e) {
				return
			}
		}
		after = page[len(page)-1].Seq
	}
}

// zoneName finds this machine's time zone's IANA name, which Go doesn't say. Empty means it can't
// tell.
func zoneName() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	target, err := filepath.EvalSymlinks("/etc/localtime")
	if err != nil {
		return ""
	}
	if _, name, found := strings.Cut(target, "zoneinfo/"); found {
		return name
	}
	return ""
}
