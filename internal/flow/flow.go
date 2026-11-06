// Package flow runs flows: automations written as ordinary Go that survive a crash at any point.
//
// A run writes each step to a journal in the host, and continues only once the entry is on disk.
// After a crash the run starts again from the top, and a step already in the journal returns what
// it returned the first time instead of happening again. So a flow must reach the same steps in the
// same order, which is why anything that varies between runs goes through a step.
//
// An action runs at least once, not exactly once. A crash after a write and before its entry
// repeats the write, so actions must be safe to repeat: set a value, never toggle.
package flow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	"github.com/harrymitchinson/oikade/internal/engine"
)

const (
	// DefaultLate is how late a flow may wake and still carry on.
	DefaultLate = 10 * time.Minute
	// MaxSteps bounds a run's journal, because replay reads all of it. This isn't a workflow
	// engine: a flow that loops forever is a hold or a moment.
	MaxSteps = 10000
	// MaxResult bounds what one Do records.
	MaxResult = 1 << 20
	// MaxRunBytes bounds a run's journal, which reaches a new process in one message of the host's
	// welcome. Past that limit the process couldn't read it, and would die at every start.
	MaxRunBytes = 4 << 20

	// ResultGaveUp is a run that woke too late to be useful.
	ResultGaveUp = "gave_up"
	// ResultDropped is a run dropped because its code changed.
	ResultDropped = "dropped"
)

// Journal is where runs are recorded, which is the host.
type Journal interface {
	// Journal returns once the entry is on disk. After an error it may or may not be there.
	Journal(ctx context.Context, run string, data []byte) (uint64, error)
	// Finish forgets a run.
	Finish(ctx context.Context, run string) error
}

// Actions is what a run does to the home, which is the engine.
type Actions interface {
	Apply(ctx context.Context, key types.NamespacedName, a engine.Action) error
	WaitFor(ctx context.Context, key types.NamespacedName, cond engine.Condition, until time.Time) (bool, error)
}

// Flow is one flow and what it runs against.
type Flow struct {
	Key types.NamespacedName
	// Safety flows never expire, so a leak response still acts late.
	Safety bool
	// Late is how late a run may wake and still carry on. Zero means DefaultLate.
	Late time.Duration
	Body func(*Run) error

	Journal Journal
	Actions Actions
	Clock   clock.Clock
	Log     logr.Logger
	// Process ends when the process stops. A run stopped by that keeps its journal and resumes, and
	// a run cancelled while it goes on, by a Restart flow's new trigger, is over.
	Process context.Context
}

// Start is a new run, as an engine.Body.
func (f *Flow) Start(ctx context.Context) string {
	id, err := newRunID(f.Key.Name)
	if err != nil {
		f.Log.Error(err, "Failed to start flow run", "rule", f.Key.Name)
		return engine.ResultFailed
	}
	return f.run(ctx, id, nil)
}

// Resume continues a run from its journal, as an engine.Body.
func (f *Flow) Resume(id string, recorded []entry) engine.Body {
	return func(ctx context.Context) string { return f.run(ctx, id, recorded) }
}

// newRunID makes an ID that's never used again: the journal restarts a deleted run's numbering, so
// one that came back would be a different run.
func newRunID(flow string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return flow + "/" + hex.EncodeToString(b[:]), nil
}

type stopReason int

const (
	// stopCancelled is a run whose context ended.
	stopCancelled stopReason = iota + 1
	// stopLost is a run that can't reach the journal.
	stopLost
	// stopLate is a run that woke too late.
	stopLate
	// stopEdited is a run whose code no longer matches its journal.
	stopEdited
	// stopTooLong is a run with too many steps.
	stopTooLong
)

// stop unwinds a flow's Go stack from inside a step. The steps have no error to return for these,
// and a flow shouldn't have to handle them.
type stop struct {
	reason stopReason
	detail string
	late   time.Duration
}

func (f *Flow) run(ctx context.Context, id string, recorded []entry) string {
	r := &Run{f: f, ctx: ctx, id: id, recorded: recorded, steps: len(recorded), live: len(recorded) == 0}
	for _, e := range recorded {
		r.bytes += len(e.Result) + entryOverhead
	}
	if !r.live {
		r.pos = 1 // Its start.
	}
	log := f.Log.WithValues("rule", f.Key.Name, "run", id)
	var failure error
	func() {
		defer func() {
			switch p := recover().(type) {
			case nil:
			case stop:
				r.stopped = &p
			default:
				failure = fmt.Errorf("flow panicked: %v", p)
			}
		}()
		if len(recorded) == 0 {
			r.record(entry{Kind: kindStart, ID: f.Key.Name})
		}
		failure = f.Body(r)
		if r.stopped == nil && r.pos < len(r.recorded) {
			// A step came out of the end of the flow.
			r.halt(stop{reason: stopEdited, detail: "it has fewer steps than its journal"})
		}
	}()
	return r.conclude(log, failure)
}

// conclude ends a run, keeping its journal only if it can carry on later.
func (r *Run) conclude(log logr.Logger, failure error) string {
	f := r.f
	result := engine.ResultSucceeded
	if s := r.stopped; s != nil {
		switch s.reason {
		case stopLost:
			log.Info("Stopped flow run; lost the host")
			return engine.ResultFailed
		case stopCancelled:
			if f.Process != nil && f.Process.Err() != nil {
				log.Info("Paused flow run; the process is stopping")
				return engine.ResultCancelled
			}
			log.Info("Cancelled flow run")
			result = engine.ResultCancelled
		case stopLate:
			log.Info("Gave up on flow run; it woke too late", "late", s.late.String())
			result = ResultGaveUp
		case stopEdited:
			log.Info("Dropped flow run; its code changed while it waited", "detail", s.detail)
			result = ResultDropped
		case stopTooLong:
			log.Info("Failed flow run; its journal is too long", "steps", r.steps, "bytes", r.bytes)
			result = engine.ResultFailed
		}
	} else if failure != nil {
		log.Error(failure, "Failed flow run")
		result = engine.ResultFailed
	}
	if err := f.Journal.Finish(context.WithoutCancel(r.ctx), r.id); err != nil {
		log.Error(err, "Failed to finish flow run; the next process will")
		return result
	}
	if result == engine.ResultSucceeded {
		log.Info("Finished flow run")
	}
	return result
}

// Saved is a run from the journal, with its raw entries.
type Saved struct {
	ID      string
	Entries [][]byte
}

// Recover works out what to resume from the runs a host kept, and returns a body for each flow
// that has one to continue. A run that can't be resumed is dropped and logged: its flow has gone, or
// a newer run of the flow replaced it. Call it before the engine runs.
func Recover(ctx context.Context, flows map[string]*Flow, saved []Saved, journal Journal) map[string]engine.Body {
	log := logr.FromContextOrDiscard(ctx)
	type candidate struct {
		id      string
		entries []entry
	}
	newest := map[string]candidate{}
	drop := func(id, reason string) {
		log.Info("Dropped flow run", "run", id, "reason", reason)
		if err := journal.Finish(ctx, id); err != nil {
			log.Error(err, "Failed to drop flow run", "run", id)
		}
	}
	for _, s := range saved {
		entries, err := decode(s.Entries)
		if err != nil || len(entries) == 0 || entries[0].Kind != kindStart {
			drop(s.ID, "its journal is unreadable")
			continue
		}
		name := entries[0].ID
		if flows[name] == nil {
			drop(s.ID, "its flow has gone")
			continue
		}
		cur, ok := newest[name]
		switch {
		case !ok:
			newest[name] = candidate{s.ID, entries}
		case entries[0].At.After(cur.entries[0].At):
			drop(cur.id, "a newer run replaced it")
			newest[name] = candidate{s.ID, entries}
		default:
			drop(s.ID, "a newer run replaced it")
		}
	}
	bodies := map[string]engine.Body{}
	for name, c := range newest {
		bodies[name] = flows[name].Resume(c.id, c.entries)
	}
	return bodies
}

func decode(raw [][]byte) ([]entry, error) {
	entries := make([]entry, 0, len(raw))
	for _, b := range raw {
		var e entry
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

var errNoResult = errors.New("recorded result is missing")
