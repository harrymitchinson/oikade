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
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
)

const (
	// DefaultLate is how late a flow may wake and still carry on.
	DefaultLate = 10 * time.Minute
	// MaxSteps bounds a run's journal, because replay reads all of it. This isn't a workflow
	// engine: a flow that loops forever is a hold or a moment.
	MaxSteps = 10000
	// MaxResult bounds what one Do records.
	MaxResult = 1 << 20
	// MaxRunBytes bounds a run's journal, which reaches a new process as one message. Past the
	// socket's limit the process couldn't read it, and would die at every start.
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
	Apply(ctx context.Context, key ident.ID, a engine.Action) error
	// ReleaseFlow ends a flow's claims, whichever run made them, but for Safety ones.
	ReleaseFlow(ctx context.Context, key ident.ID) error
	WaitFor(ctx context.Context, key ident.ID, cond engine.Condition, until time.Time) (bool, error)
}

// Flow is one flow and what it runs against.
type Flow struct {
	Key ident.ID
	// Safety flows never expire, so a leak response still acts late.
	Safety bool
	// Late is how late a run may wake and still carry on. Zero means DefaultLate.
	Late time.Duration
	Body func(*Run) error

	Journal Journal
	Actions Actions
	Clock   clock.Clock
	Log     *slog.Logger
	// Process ends when the process stops. A run stopped by that keeps its journal and resumes, and
	// a run cancelled while it goes on, by a Restart flow's new trigger, is over.
	Process context.Context
	// OnPark, if set, hears a run begin waiting on the clock.
	OnPark engine.Park
	// Dry, if set, stands in for each outside call, for oikade replay: what's being replayed has
	// already happened. It sets result, a pointer or nil, and returns what the call would.
	Dry func(name string, result any) error

	// claiming is held while a run acts, and while a run that ended badly releases its claims. A
	// Restart flow's next run starts without waiting for the last to end, and claims under the same
	// source, so this keeps the release from landing on the newer claim.
	claiming sync.Mutex
}

// Start is a new run, as an engine.Body.
func (f *Flow) Start(ctx context.Context) string {
	id, err := newRunID(f.Key.Name)
	if err != nil {
		f.Log.Error("Failed to start flow run", "err", err, "rule", f.Key.Name)
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
		r.bytes += e.size
	}
	if !r.live {
		r.pos = 1 // Its start.
	}
	log := f.Log.With("rule", f.Key.Name, "run", id)
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
func (r *Run) conclude(log *slog.Logger, failure error) string {
	f := r.f
	result := engine.ResultSucceeded
	// A run told to stop that ended some other way was still stopped: a newer run may be going, or
	// the process is stopping, and either way what it left is no longer its to clean up.
	if s := r.stopped; r.ctx.Err() != nil && ((s == nil && failure != nil) || (s != nil && s.reason != stopLost)) {
		if failure != nil {
			log.Error("Failed flow run after it was told to stop", "err", failure)
		}
		r.stopped, failure = &stop{reason: stopCancelled}, nil
	}
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
		log.Error("Failed flow run", "err", failure)
		result = engine.ResultFailed
	}
	if result == ResultGaveUp || result == ResultDropped || result == engine.ResultFailed {
		switch err := r.releaseClaims(); {
		case errors.Is(err, errStopping):
			log.Info("Paused flow run; the process is stopping")
			return engine.ResultCancelled
		case err != nil:
			log.Error("Failed to release the claims of a flow run; the next process will", "err", err)
			return result
		}
	}
	if err := f.Journal.Finish(context.WithoutCancel(r.ctx), r.id); err != nil {
		log.Error("Failed to finish flow run; the next process will", "err", err)
		return result
	}
	if result == engine.ResultSucceeded {
		log.Info("Finished flow run")
	}
	return result
}

// errStopping is a run that ended badly with the process stopping before it could release.
var errStopping = errors.New("the process is stopping")

// releaseClaims ends the flow's claims, since this run may never reach its Release and nothing else
// would end an UntilReleased one. A flow's claims belong to the flow, so a replaced run leaves them
// to the run that replaced it. A stopping process can't release, so its run must keep its journal.
func (r *Run) releaseClaims() error {
	f := r.f
	f.claiming.Lock()
	defer f.claiming.Unlock()
	if f.Process != nil && f.Process.Err() != nil {
		return errStopping
	}
	if r.ctx.Err() != nil {
		return nil
	}
	return f.Actions.ReleaseFlow(context.WithoutCancel(r.ctx), f.Key)
}

// Saved is a run from the journal, with its raw entries.
type Saved struct {
	ID      string
	Entries [][]byte
}

// Recover works out what to resume from the runs a host kept, and returns a body for each flow
// that has one to continue. A run a newer one of its flow replaced, or with an unreadable journal, is
// dropped and logged. Unless a run of its flow carries on, its flow's claims go first, as when any run
// ends badly. A run of a flow this binary doesn't have is left alone: this binary hasn't proved itself,
// so the host ends those once a deploy has. Call it before the engine runs.
func Recover(ctx context.Context, namespace string, flows map[string]*Flow, saved []Saved, journal Journal, actions Actions) map[string]engine.Body {
	log := logging.FromContext(ctx)
	type candidate struct {
		id      string
		entries []entry
	}
	newest := map[string]candidate{}
	drop := func(id, reason string) {
		log.Info("Dropped flow run", "run", id, "reason", reason)
		if err := journal.Finish(ctx, id); err != nil {
			log.Error("Failed to drop flow run", "err", err, "run", id)
		}
	}
	// retire ends a run nothing will continue, and with it what its flow was left holding.
	retire := func(id, name, reason string) {
		if name != "" {
			key := ident.ID{Namespace: namespace, Name: name}
			if err := actions.ReleaseFlow(ctx, key); err != nil {
				log.Error("Failed to release the claims of a flow run; the next process will", "err", err, "run", id)
				return
			}
		}
		drop(id, reason)
	}
	var unreadable []Saved
	for _, s := range saved {
		entries, err := decode(s.Entries)
		if err != nil || len(entries) == 0 || entries[0].Kind != kindStart {
			unreadable = append(unreadable, s)
			continue
		}
		name := entries[0].ID
		if flows[name] == nil {
			log.Info("Left flow run alone; this binary doesn't have its flow", "run", s.ID)
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
	for _, s := range unreadable {
		// Its claims are under the flow's name, and a run that's carrying on may hold the same ones.
		name := FlowOf(s.ID)
		if name != "" && flows[name] == nil {
			log.Info("Left flow run alone; this binary doesn't have its flow", "run", s.ID)
			continue
		}
		if _, carriesOn := newest[name]; carriesOn {
			drop(s.ID, "its journal is unreadable")
			continue
		}
		retire(s.ID, name, "its journal is unreadable")
	}
	bodies := map[string]engine.Body{}
	for name, c := range newest {
		bodies[name] = flows[name].Resume(c.id, c.entries)
	}
	return bodies
}

// FlowOf returns the flow a run ID names.
func FlowOf(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[:i]
	}
	return ""
}

func decode(raw [][]byte) ([]entry, error) {
	entries := make([]entry, 0, len(raw))
	for _, b := range raw {
		var e entry
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		e.size = len(b)
		entries = append(entries, e)
	}
	return entries, nil
}

var errNoResult = errors.New("recorded result is missing")
