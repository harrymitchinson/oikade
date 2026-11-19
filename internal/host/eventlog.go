package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
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
	log   *slog.Logger
	// clock times the flushes.
	clock clock.Clock

	mu      sync.Mutex
	queue   []api.Event
	dropped int
	// flushing keeps two flushes from reordering what they add.
	flushing sync.Mutex
}

func newEventLog(events *store.Events, bound store.Bound, log *slog.Logger) *eventLog {
	if bound.MaxAge <= 0 {
		bound.MaxAge = DefaultEventLogAge
	}
	if bound.MaxBytes == 0 {
		bound.MaxBytes = DefaultEventLogBytes
	}
	return &eventLog{store: events, bound: bound, log: log, clock: clock.Real{}}
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
	for {
		next := l.clock.NewTimer(eventFlush)
		select {
		case <-next.C():
			l.flush()
		case <-ctx.Done():
			next.Stop()
			return
		}
	}
}

// flush commits what's queued, and applies the bound even if nothing is. A failure loses those events
// and nothing else: the log is history, and a home runs without it.
func (l *eventLog) flush() {
	l.flushing.Lock()
	defer l.flushing.Unlock()
	l.commit(l.take())
}

// flushedAt is flush for a tail that starts with the log's past: it returns the place of the log's
// last record once what's queued is in it. at is given the taking of the queue to call, so that its
// caller can start listening at that same moment: every event is then in the log up to that place,
// or heard, and none is both. Nothing is added to the log meanwhile, as a flush under way has it.
func (l *eventLog) flushedAt(at func(take func())) (uint64, error) {
	l.flushing.Lock()
	defer l.flushing.Unlock()
	var queue []api.Event
	var dropped int
	at(func() { queue, dropped = l.take() })
	if last := l.commit(queue, dropped); last != 0 {
		return last, nil
	}
	// Nothing was queued, or it was lost: the log ends where it did.
	last, err := l.store.Last()
	if err != nil {
		return 0, fmt.Errorf("reading the event log: %w", err)
	}
	return last, nil
}

// take empties the queue.
func (l *eventLog) take() (queue []api.Event, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	queue, dropped = l.queue, l.dropped
	l.queue, l.dropped = nil, 0
	return queue, dropped
}

// commit adds a queue to the log, and returns the place of the last record it added, or 0 if it
// added none. Call it with flushing held.
func (l *eventLog) commit(queue []api.Event, dropped int) uint64 {
	if dropped > 0 {
		l.log.Info("Dropped events from the log; they came too fast", "events", dropped)
	}
	records := make([]store.Record, 0, len(queue))
	for _, e := range queue {
		raw, err := encodeEvent(e)
		if err != nil {
			l.log.Error("Failed to encode event", "err", err, "type", e.Type)
			continue
		}
		records = append(records, store.Record{Time: e.Time, Data: raw})
	}
	if err := l.store.Add(records, time.Now(), l.bound, foldBaseline); err != nil {
		l.log.Error("Failed to add to the event log", "err", err, "events", len(records))
		return 0
	}
	if len(records) == 0 {
		return 0
	}
	return records[len(records)-1].Seq
}

// encodeEvent is an event as the log keeps it. The store has its time, and decodeEvent works out a
// ref that only repeats the event's device, capability or rule, which is most of them and a third
// of each one's size.
func encodeEvent(e api.Event) ([]byte, error) {
	e.Time = time.Time{}
	if e.Ref == eventRef(e) {
		e.Ref = ""
	}
	return json.Marshal(e)
}

// decodeEvent is the event a record keeps, as it was before encodeEvent.
func decodeEvent(r store.Record) (api.Event, error) {
	var e api.Event
	if err := json.Unmarshal(r.Data, &e); err != nil {
		return e, err
	}
	e.Time = r.Time
	if e.Ref == "" {
		e.Ref = eventRef(e)
	}
	return e, nil
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

// lineWriter writes a stream's lines. write buffers one, and flush sends what's buffered. Each says
// false once the caller has gone.
type lineWriter interface {
	write(v any) bool
	flush() bool
}

// stream writes the log's events that pass to out, a page at a time, until it catches up or ctx ends,
// which its last line says if the client was revoked. Events added meanwhile are included. A nil
// pass lets every event through. A log that can't be read goes to fail instead, before any line.
func (l *eventLog) stream(ctx context.Context, out lineWriter, fail func(error), pass func(api.Event) bool) {
	l.flush()
	now := time.Now()
	head := api.EventLog{Now: now, Zone: zoneName()}
	_, head.Offset = now.Zone()
	// Together, or a drop between the two would be in neither.
	raw, page, err := l.store.Start(eventPage)
	if err == nil && raw != nil {
		err = json.Unmarshal(raw, &head.Baseline)
	}
	if err != nil {
		fail(fmt.Errorf("reading the event log: %w", err))
		return
	}
	if len(page) > 0 {
		head.Start = page[0].Time
	}
	if !out.write(head) {
		return
	}
	if short, told := l.send(ctx, out, page, 0, pass); told {
		endLines(out, short)
	}
}

// endLines closes a stream, saying why if it stopped short.
func endLines(out lineWriter, short string) {
	if out.write(api.LogEnd(short)) {
		out.flush()
	}
}

// history writes the log's events that pass to out, up to the record at upTo, which flushedAt gave. It
// says why it stopped short, if it did, and told is false if there's nobody to tell, as send's is.
func (l *eventLog) history(ctx context.Context, out lineWriter, upTo uint64, pass func(api.Event) bool) (short string, told bool) {
	if upTo == 0 {
		return "", true
	}
	_, page, err := l.store.Start(eventPage)
	if err != nil {
		return fmt.Sprintf("reading the event log: %v", err), true
	}
	return l.send(ctx, out, page, upTo, pass)
}

// send writes the events that pass, from page on: to the record at upTo, or with none, until it
// catches up with the log. short is why it stopped before that, if it did. told is false if there's
// nobody to tell: the caller went, or the host is stopping, which says nothing, as a host that
// crashed can't, so that its caller reads it as a stop. A revoked client is told why.
func (l *eventLog) send(ctx context.Context, out lineWriter, page []store.Record, upTo uint64,
	pass func(api.Event) bool,
) (short string, told bool) {
	for len(page) > 0 {
		if ctx.Err() != nil {
			if errors.Is(context.Cause(ctx), errRevoked) {
				return errRevoked.Error(), true
			}
			return "", false
		}
		for _, r := range page {
			if upTo != 0 && r.Seq > upTo {
				return "", out.flush()
			}
			e, err := decodeEvent(r)
			if err != nil {
				return fmt.Sprintf("event %d: %v", r.Seq, err), true
			}
			if (pass == nil || pass(e)) && !out.write(e) {
				return "", false
			}
		}
		if !out.flush() {
			return "", false
		}
		var err error
		if page, err = l.store.Read(page[len(page)-1].Seq, eventPage); err != nil {
			return err.Error(), true
		}
	}
	return "", true
}

// renames are the log's rename events, oldest first: what a device's ref needs to find it under the
// names it had. It reads the log through, decoding only those.
func (l *eventLog) renames(ctx context.Context) ([]api.Event, error) {
	l.flush()
	var out []api.Event
	_, page, err := l.store.Start(eventPage)
	for err == nil && len(page) > 0 && ctx.Err() == nil {
		for _, r := range page {
			if !bytes.Contains(r.Data, renameMark) {
				continue
			}
			if e, err := decodeEvent(r); err == nil && e.Type == api.EventRename {
				out = append(out, e)
			}
		}
		page, err = l.store.Read(page[len(page)-1].Seq, eventPage)
	}
	if err != nil {
		return nil, err
	}
	return out, ctx.Err()
}

// renameMark is in every rename event as the log keeps it, and in few others.
var renameMark = []byte(`"type":"` + api.EventRename + `"`)

// zoneName finds this machine's time zone's IANA name, which Go doesn't say, by the rules Go uses to
// choose it. Empty means it can't tell.
func zoneName() string {
	if tz, set := os.LookupEnv("TZ"); set {
		tz = strings.TrimPrefix(tz, ":")
		if tz == "" {
			return "UTC"
		}
		if _, name, found := strings.Cut(tz, "zoneinfo/"); found {
			return name
		}
		return tz
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, name, found := strings.Cut(target, "zoneinfo/"); found {
			return name
		}
	}
	// Debian's, for an /etc/localtime that's a copy rather than a link.
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}
