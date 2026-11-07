package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
)

var epoch = time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)

// fakeHome is a journal and the home's actions, in memory.
type fakeHome struct {
	mu       sync.Mutex
	journal  map[string][][]byte
	finished []string
	applied  []string
	// claims is what's claimed under the flow's source, of any run, by device, and says which are
	// Safety. order is the releases and finishes as they happened. onClaim runs after a claim lands.
	claims      map[string]bool
	onClaim     func() error
	order       []string
	releasedFor ident.ID
	releaseErr  error
	// releasing, if set, holds up a release until it closes, and entered closes when one begins.
	releasing, entered chan struct{}
	failing            error
	// met is what WaitFor's condition says, and woken ends a wait for it.
	woken chan struct{}
}

func newFakeHome() *fakeHome {
	return &fakeHome{journal: map[string][][]byte{}, woken: make(chan struct{}), claims: map[string]bool{}}
}

func (h *fakeHome) Journal(_ context.Context, run string, data []byte) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failing != nil {
		return 0, h.failing
	}
	h.journal[run] = append(h.journal[run], data)
	return uint64(len(h.journal[run])), nil
}

func (h *fakeHome) Finish(_ context.Context, run string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.journal, run)
	h.finished = append(h.finished, run)
	h.order = append(h.order, "finish")
	return nil
}

func (h *fakeHome) Apply(_ context.Context, _ ident.ID, a engine.Action) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := a.(engine.Release); ok {
		// As the engine does: everything under the flow's source, whichever run made it.
		clear(h.claims)
		h.order = append(h.order, "release all")
		return nil
	}
	s, _ := a.(engine.Set)
	if s.Until != "" {
		h.claims[s.Device] = s.Priority == engine.PrioritySafety
	}
	if h.onClaim != nil {
		return h.onClaim()
	}
	h.applied = append(h.applied, s.Device)
	return nil
}

func (h *fakeHome) ReleaseFlow(_ context.Context, key ident.ID) error {
	if h.releasing != nil {
		close(h.entered)
		<-h.releasing
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.releaseErr != nil {
		return h.releaseErr
	}
	h.releasedFor = key
	for device, safety := range h.claims {
		if !safety {
			delete(h.claims, device)
		}
	}
	h.order = append(h.order, "release")
	return nil
}

// holds reports whether a claim on the device is still made.
func (h *fakeHome) holds(device string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.claims[device]
	return ok
}

func (h *fakeHome) WaitFor(ctx context.Context, _ ident.ID, cond engine.Condition, _ time.Time) (bool, error) {
	if cond == nil {
		return false, errors.New("a wait needs a condition") // As the engine answers.
	}
	if ok, _ := cond(nil); ok {
		return true, nil
	}
	select {
	case <-h.woken:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (h *fakeHome) saved() []Saved {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Saved, 0, len(h.journal))
	for id, entries := range h.journal {
		out = append(out, Saved{ID: id, Entries: entries})
	}
	return out
}

func (h *fakeHome) appliedTo() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.applied...)
}

type logs struct {
	mu    sync.Mutex
	lines []string
	// onLine, if set, runs for each line, outside the lock.
	onLine func(string)
}

func (l *logs) logger() *slog.Logger {
	return logging.Lines(func(line string) {
		l.mu.Lock()
		l.lines = append(l.lines, line)
		onLine := l.onLine
		l.mu.Unlock()
		if onLine != nil {
			onLine(line)
		}
	}, slog.LevelInfo)
}

func (l *logs) has(want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func newFlow(h *fakeHome, c *clock.Fake, l *logs, body func(*Run) error) *Flow {
	return &Flow{
		Key:  ident.ID{Namespace: "home", Name: "bedtime"},
		Body: body, Journal: h, Actions: h, Clock: c, Log: l.logger(), Process: context.Background(),
	}
}

// resume restarts the flow's one saved run, as a new process does, and returns its result.
func resume(ctx context.Context, t *testing.T, f *Flow, h *fakeHome, l *logs) <-chan string {
	t.Helper()
	bodies := Recover(logging.NewContext(ctx, l.logger()), "home", map[string]*Flow{f.Key.Name: f}, h.saved(), h, h)
	body := bodies[f.Key.Name]
	if body == nil {
		t.Fatal("nothing to resume")
	}
	result := make(chan string, 1)
	go func() { result <- body(ctx) }()
	return result
}

// parked waits until something is waiting on the clock.
func parked(t *testing.T, c *clock.Fake) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !c.HasWaiters() {
		if time.Now().After(deadline) {
			t.Fatal("nothing started waiting on the clock")
		}
		time.Sleep(time.Millisecond)
	}
}

func outcome(t *testing.T, result <-chan string) string {
	t.Helper()
	select {
	case r := <-result:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the run didn't end")
		return ""
	}
}

// crash starts a run and stops it the way a dead process does: the context ends, the process with
// it, and the journal stays.
func crash(t *testing.T, f *Flow, c *clock.Fake) {
	t.Helper()
	process, die := context.WithCancel(context.Background())
	f.Process = process
	result := make(chan string, 1)
	go func() { result <- f.Start(process) }()
	parked(t, c)
	die()
	if got := outcome(t, result); got != engine.ResultCancelled {
		t.Fatalf("crashed run ended %q", got)
	}
	f.Process = context.Background()
}

func bedtime(r *Run) error {
	r.Sleep(time.Hour)
	return r.Apply(engine.Set{Device: "downstairs", Capability: "switch.on", Value: false})
}

func TestAWaitOutlivesARestartAndWakesAtItsOriginalTime(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, bedtime)
	crash(t, f, c)

	// Back up 40 minutes into the hour. A wait that started again would end 40 minutes too late.
	c.Step(40 * time.Minute)
	result := resume(context.Background(), t, f, h, l)
	parked(t, c)
	c.Step(19 * time.Minute)
	if got := h.appliedTo(); len(got) != 0 {
		t.Fatalf("woke before its time: %v", got)
	}
	parked(t, c)
	c.Step(time.Minute)
	if got := outcome(t, result); got != engine.ResultSucceeded {
		t.Fatalf("got %q", got)
	}
	if got := h.appliedTo(); len(got) != 1 || got[0] != "downstairs" {
		t.Fatalf("applied %v", got)
	}
	if len(h.saved()) != 0 {
		t.Fatal("a finished run kept its journal")
	}
}

func TestLateNormalFlowGivesUpAndLogsIt(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, bedtime)
	crash(t, f, c)

	c.Step(10 * time.Hour) // Nine hours past its wake.
	if got := outcome(t, resume(context.Background(), t, f, h, l)); got != ResultGaveUp {
		t.Fatalf("got %q", got)
	}
	if got := h.appliedTo(); len(got) != 0 {
		t.Fatalf("a late flow acted: %v", got)
	}
	if !l.has("Gave up on flow run") || !l.has("9h0m0s") {
		t.Fatalf("not logged: %v", l.lines)
	}
	if len(h.saved()) != 0 {
		t.Fatal("a run that gave up kept its journal")
	}
}

func TestLateSafetyFlowRuns(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, bedtime)
	f.Safety = true
	crash(t, f, c)

	c.Step(10 * time.Hour)
	if got := outcome(t, resume(context.Background(), t, f, h, l)); got != engine.ResultSucceeded {
		t.Fatalf("got %q", got)
	}
	if got := h.appliedTo(); len(got) != 1 {
		t.Fatalf("applied %v", got)
	}
}

func TestFlowSetsItsOwnLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		limit, late time.Duration
		want        string
	}{
		"within it": {limit: 2 * time.Hour, late: time.Hour, want: engine.ResultSucceeded},
		"past it":   {limit: time.Minute, late: 2 * time.Minute, want: ResultGaveUp},
		"at it":     {limit: time.Minute, late: time.Minute, want: engine.ResultSucceeded},
	} {
		t.Run(name, func(t *testing.T) {
			h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
			f := newFlow(h, c, l, bedtime)
			f.Late = tc.limit
			crash(t, f, c)
			c.Step(time.Hour + tc.late)
			if got := outcome(t, resume(context.Background(), t, f, h, l)); got != tc.want {
				t.Fatalf("got %q", got)
			}
		})
	}
}

// A run that went down between steps, with no wait in sight, is late from its last step.
func TestRunDownBetweenStepsIsLateFromItsLastStep(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	body := func(r *Run) error {
		r.Now()
		return r.Apply(engine.Set{Device: "a", Capability: "switch.on", Value: true})
	}
	f := newFlow(h, c, l, body)
	if _, err := h.Journal(context.Background(), "bedtime/x", mustEntry(t, entry{Kind: kindStart, ID: "bedtime", At: epoch})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Journal(context.Background(), "bedtime/x", mustEntry(t, entry{Kind: kindNow, At: epoch})); err != nil {
		t.Fatal(err)
	}
	c.Step(11 * time.Minute)
	if got := outcome(t, resume(context.Background(), t, f, h, l)); got != ResultGaveUp {
		t.Fatalf("got %q", got)
	}
}

func mustEntry(t *testing.T, e entry) []byte {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRecordedStepsReturnWhatTheyReturned(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	calls := 0
	var seen []string
	body := func(r *Run) error {
		now := r.Now()
		var price float64
		err := r.Do("price", &price, func(context.Context) error {
			calls++
			price = 21.5
			return nil
		})
		failed := r.Do("flaky", nil, func(context.Context) error { calls++; return errors.New("timed out") })
		r.Sleep(time.Hour)
		seen = append(seen, fmt.Sprintf("%v %v %v %v", now.Equal(epoch), price, err, failed))
		return nil
	}
	f := newFlow(h, c, l, body)
	crash(t, f, c)
	c.Step(30 * time.Minute)
	result := resume(context.Background(), t, f, h, l)
	parked(t, c)
	c.Step(30 * time.Minute)
	if got := outcome(t, result); got != engine.ResultSucceeded {
		t.Fatalf("got %q", got)
	}
	if calls != 2 {
		t.Fatalf("outside calls ran %d times, want 2", calls)
	}
	if len(seen) != 1 || seen[0] != "true 21.5 <nil> timed out" {
		t.Fatalf("replay saw %v", seen)
	}
}

func TestWaitForKeepsItsDeadlineAndResult(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	never := func(engine.Vars) (bool, error) { return false, nil }
	ran := 0
	f := newFlow(h, c, l, func(r *Run) error {
		ran++
		if r.WaitFor(never, time.Hour) {
			return nil
		}
		return r.Apply(engine.Set{Device: "timed-out", Capability: "switch.on", Value: true})
	})
	// The fake's wait ends with its context, and a flow past its deadline takes the timeout path.
	process, die := context.WithCancel(context.Background())
	f.Process = process
	done := make(chan string, 1)
	go func() { done <- f.Start(process) }()
	waitJournal(t, h, 2)
	die()
	outcome(t, done)
	f.Process = context.Background()

	// Five minutes past the deadline, so the fake's wait returns at once by being told so.
	c.Step(65 * time.Minute)
	close(h.woken)
	if got := outcome(t, resume(context.Background(), t, f, h, l)); got != engine.ResultSucceeded {
		t.Fatalf("got %q", got)
	}
	if ran != 2 {
		t.Fatalf("ran %d times", ran)
	}
}

func TestWaitForFarPastItsDeadlineGivesUp(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	never := func(engine.Vars) (bool, error) { return false, nil }
	f := newFlow(h, c, l, func(r *Run) error {
		r.WaitFor(never, time.Hour)
		return nil
	})
	process, die := context.WithCancel(context.Background())
	f.Process = process
	done := make(chan string, 1)
	go func() { done <- f.Start(process) }()
	waitJournal(t, h, 2)
	die()
	outcome(t, done)
	f.Process = context.Background()

	c.Step(2 * time.Hour)
	if got := outcome(t, resume(context.Background(), t, f, h, l)); got != ResultGaveUp {
		t.Fatalf("got %q", got)
	}
}

// waitJournal waits for a run to have n entries.
func waitJournal(t *testing.T, h *fakeHome, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, s := range h.saved() {
			if len(s.Entries) >= n {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the journal never grew")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestChangedFlowIsDropped(t *testing.T) {
	for name, edited := range map[string]func(*Run) error{
		"a different wait": func(r *Run) error { r.Sleep(2 * time.Hour); return nil },
		"a different step": func(r *Run) error { r.Now(); return nil },
		"a step put first": func(r *Run) error {
			if err := r.Apply(engine.Set{Device: "new", Capability: "switch.on", Value: true}); err != nil {
				return err
			}
			r.Sleep(time.Hour)
			return nil
		},
		"steps taken out": func(*Run) error { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
			f := newFlow(h, c, l, bedtime)
			crash(t, f, c)

			f.Body = edited
			if got := outcome(t, resume(context.Background(), t, f, h, l)); got != ResultDropped {
				t.Fatalf("got %q", got)
			}
			if !l.has("Dropped flow run") {
				t.Fatalf("not logged: %v", l.lines)
			}
			if got := h.appliedTo(); len(got) != 0 {
				t.Fatalf("a changed flow acted: %v", got)
			}
			if len(h.saved()) != 0 {
				t.Fatal("a dropped run kept its journal")
			}
		})
	}
}

// A Restart flow's new trigger cancels the run with the process still going, which is the end of it.
func TestCancelledRunIsOver(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, bedtime)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() { done <- f.Start(ctx) }()
	parked(t, c)
	cancel()
	if got := outcome(t, done); got != engine.ResultCancelled {
		t.Fatalf("got %q", got)
	}
	if len(h.saved()) != 0 {
		t.Fatal("a cancelled run kept its journal")
	}
	if c.HasWaiters() {
		t.Fatal("a cancelled run left its timer on the clock")
	}
}

func TestRunStopsWithoutFinishingWhenTheJournalFails(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, func(r *Run) error {
		r.Now()
		h.mu.Lock()
		h.failing = errors.New("disk full")
		h.mu.Unlock()
		return r.Apply(engine.Set{Device: "a", Capability: "switch.on", Value: true})
	})
	if got := f.Start(context.Background()); got != engine.ResultFailed {
		t.Fatalf("got %q", got)
	}
	if len(h.finished) != 0 {
		t.Fatal("a run that lost the journal mustn't be finished")
	}
}

func TestFailedFlowIsLoggedAndOver(t *testing.T) {
	for name, body := range map[string]func(*Run) error{
		"an error": func(*Run) error { return errors.New("no") },
		"a panic":  func(*Run) error { panic("no") },
	} {
		t.Run(name, func(t *testing.T) {
			h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
			if got := newFlow(h, c, l, body).Start(context.Background()); got != engine.ResultFailed {
				t.Fatalf("got %q", got)
			}
			if !l.has("Failed flow run") {
				t.Fatalf("not logged: %v", l.lines)
			}
			if len(h.saved()) != 0 {
				t.Fatal("a failed run kept its journal")
			}
		})
	}
}

// A flow that swallows the stop still stops.
func TestRecoveringFromAStopDoesNotContinueTheRun(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, func(r *Run) error {
		func() {
			defer func() { _ = recover() }()
			r.Sleep(time.Hour)
		}()
		return r.Apply(engine.Set{Device: "after", Capability: "switch.on", Value: true})
	})
	crash(t, f, c)
	if got := h.appliedTo(); len(got) != 0 {
		t.Fatalf("a stopped run went on: %v", got)
	}
}

// A wait that finished long ago mustn't excuse a run that went down after it.
func TestRunAfterACompletedWaitIsStillLate(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	met := func(engine.Vars) (bool, error) { return true, nil }
	f := newFlow(h, c, l, func(r *Run) error {
		r.WaitFor(met, time.Hour)
		r.Sleep(time.Hour)
		return r.Apply(engine.Set{Device: "a", Capability: "switch.on", Value: true})
	})
	crash(t, f, c)

	// Now take the wait out of it: the run is down between its wait and a new last step.
	saved := h.saved()
	if len(saved[0].Entries) != 4 {
		t.Fatalf("journal has %d entries", len(saved[0].Entries))
	}
	h.journal[saved[0].ID] = saved[0].Entries[:3]
	f.Body = func(r *Run) error {
		r.WaitFor(met, time.Hour)
		return r.Apply(engine.Set{Device: "a", Capability: "switch.on", Value: true})
	}
	c.Step(11 * time.Minute)
	if got := outcome(t, resume(context.Background(), t, f, h, l)); got != ResultGaveUp {
		t.Fatalf("got %q", got)
	}
}

func TestLongJournalEndsTheRun(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, func(r *Run) error {
		for {
			var s string
			if err := r.Do("big", &s, func(context.Context) error {
				s = strings.Repeat("x", MaxResult/2)
				return nil
			}); err != nil {
				return err
			}
		}
	})
	if got := f.Start(context.Background()); got != engine.ResultFailed {
		t.Fatalf("got %q", got)
	}
	if !l.has("its journal is too long") {
		t.Fatalf("not logged: %v", l.lines)
	}
}

// set claims a device for a test's flow until it's released, at normal priority.
func set(device string) engine.Set {
	return engine.Set{Device: device, Capability: "switch.on", Value: false, Until: engine.UntilReleased}
}

// claimThenSleep claims the lamp and parks, so a crash leaves the claim in its journal.
func claimThenSleep(r *Run) error {
	if err := r.Apply(set("lamp")); err != nil {
		return err
	}
	r.Sleep(time.Hour)
	return nil
}

// A run that ends badly can't reach its Release, so the flow's claims mustn't outlive it.
func TestRunEndingBadlyReleasesTheFlowsClaims(t *testing.T) {
	t.Run("gave up", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, claimThenSleep)
		crash(t, f, c)
		c.Step(10 * time.Hour)
		if got := outcome(t, resume(context.Background(), t, f, h, l)); got != ResultGaveUp {
			t.Fatalf("got %q", got)
		}
		if h.holds("lamp") {
			t.Fatal("the claim wasn't released")
		}
	})
	t.Run("dropped", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, claimThenSleep)
		crash(t, f, c)
		f.Body = func(r *Run) error { r.Sleep(time.Minute); return nil }
		if got := outcome(t, resume(context.Background(), t, f, h, l)); got != ResultDropped {
			t.Fatalf("got %q", got)
		}
		if h.holds("lamp") {
			t.Fatal("the claim wasn't released")
		}
	})
	for name, end := range map[string]func() error{
		"failed":   func() error { return errors.New("no") },
		"panicked": func() error { panic("no") },
	} {
		t.Run(name, func(t *testing.T) {
			h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
			newFlow(h, c, l, func(r *Run) error {
				if err := r.Apply(set("lamp")); err != nil {
					return err
				}
				return end()
			}).Start(context.Background())
			if h.holds("lamp") {
				t.Fatal("the claim wasn't released")
			}
		})
	}
	t.Run("keeps the journal if the release fails, for the next process", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		h.releaseErr = errors.New("no host")
		newFlow(h, c, l, func(*Run) error { return errors.New("no") }).Start(context.Background())
		if len(h.saved()) != 1 {
			t.Fatal("it lost the journal, and with it the next process's chance to release")
		}
	})
}

// A claim whose run was replaced passes to the run that replaced it: none of these leaves it with
// nothing to end it.
func TestReplacedRunsClaimPassesToTheRunThatReplacedIt(t *testing.T) {
	// replace claims in a run, and has a trigger cancel it, which is what Restart does.
	replace := func(t *testing.T, f *Flow, c *clock.Fake) {
		t.Helper()
		f.Body = claimThenSleep
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan string, 1)
		go func() { done <- f.Start(ctx) }()
		parked(t, c)
		cancel()
		if got := outcome(t, done); got != engine.ResultCancelled {
			t.Fatalf("the replaced run ended %q", got)
		}
	}
	t.Run("the replacement fails on its first step", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, nil)
		replace(t, f, c)
		if !h.holds("lamp") {
			t.Fatal("the replaced run released the claim")
		}
		f.Body = func(*Run) error { return errors.New("no") }
		f.Start(context.Background())
		if h.holds("lamp") {
			t.Fatal("the claim was left with nothing to end it")
		}
	})
	t.Run("the replacement succeeds, and a later run fails", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, nil)
		replace(t, f, c)
		f.Body = func(*Run) error { return nil }
		f.Start(context.Background())
		if !h.holds("lamp") {
			t.Fatal("a run that succeeded released a claim")
		}
		f.Body = func(*Run) error { return errors.New("no") }
		f.Start(context.Background())
		if h.holds("lamp") {
			t.Fatal("the claim was left with nothing to end it")
		}
	})
	t.Run("recovery drops the replaced run, and the run that carries on gives up", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, claimThenSleep)
		crash(t, f, c)
		c.Step(time.Minute)
		crash(t, f, c)
		c.Step(10 * time.Hour)
		bodies := Recover(context.Background(), "home", map[string]*Flow{f.Key.Name: f}, h.saved(), h, h)
		if !h.holds("lamp") {
			t.Fatal("dropping the replaced run released the claim")
		}
		if got := bodies[f.Key.Name](context.Background()); got != ResultGaveUp {
			t.Fatalf("got %q", got)
		}
		if h.holds("lamp") {
			t.Fatal("the claim was left with nothing to end it")
		}
	})
}

// A process stopping during a claim's round trip leaves the claim made and no entry to say so.
func TestStopDuringAClaimThenAGiveUpLeavesNoClaim(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	process, die := context.WithCancel(context.Background())
	f := newFlow(h, c, l, claimThenSleep)
	f.Process = process
	h.onClaim = func() error {
		die()
		return context.Canceled
	}
	if got := f.Start(process); got != engine.ResultCancelled {
		t.Fatalf("got %q", got)
	}
	if !h.holds("lamp") {
		t.Fatal("the test's claim wasn't made")
	}
	h.onClaim = nil
	f.Process = context.Background()
	c.Step(10 * time.Hour)
	if got := outcome(t, resume(context.Background(), t, f, h, l)); got != ResultGaveUp {
		t.Fatalf("got %q", got)
	}
	if h.holds("lamp") {
		t.Fatal("a claim with no entry outlived the run")
	}
}

// A process can stop between a run failing and its release. It can't release, and Finish would lose
// the only thing telling the next process to.
func TestStopJustBeforeAReleaseKeepsTheJournal(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	process, die := context.WithCancel(context.Background())
	f := newFlow(h, c, l, func(r *Run) error {
		if err := r.Apply(set("lamp")); err != nil {
			return err
		}
		return errors.New("no")
	})
	f.Process = process
	// This line comes after the check for a stop at the top of conclude, and before the release.
	l.onLine = func(line string) {
		if strings.Contains(line, "Failed flow run") {
			die()
		}
	}
	if got := f.Start(process); got != engine.ResultCancelled {
		t.Fatalf("got %q", got)
	}
	if len(h.saved()) != 1 {
		t.Fatal("the journal went with the claim still held")
	}
	if !h.holds("lamp") {
		t.Fatal("the test's claim went")
	}
	l.onLine = nil
	f.Process = context.Background()
	outcome(t, resume(context.Background(), t, f, h, l))
	if h.holds("lamp") {
		t.Fatal("the next process didn't release the claim")
	}
}

// Restart cancels a run and starts the next without waiting for it. A slow step can then come back,
// and the old run end badly, with the newer run's claim made on the same capability under the same
// source.
func TestRunEndingBadlyLeavesANewerRunsClaim(t *testing.T) {
	endings := map[string]func() error{
		"failing":   func() error { return errors.New("no") },
		"panicking": func() error { panic("no") },
	}
	for name, end := range endings {
		t.Run(name, func(t *testing.T) {
			h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
			started, finish := make(chan struct{}), make(chan struct{})
			first := true
			f := newFlow(h, c, l, nil)
			f.Body = func(r *Run) error {
				if err := r.Apply(set("lamp")); err != nil {
					return err
				}
				if !first {
					return nil
				}
				first = false
				if err := r.Do("slow", nil, func(context.Context) error {
					close(started)
					<-finish // It ignores its context.
					return nil
				}); err != nil {
					return err
				}
				return end() // With no step left to notice the cancel.
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan string, 1)
			go func() { done <- f.Start(ctx) }()
			<-started
			cancel()
			if got := f.Start(context.Background()); got != engine.ResultSucceeded {
				t.Fatalf("the newer run ended %q", got)
			}
			close(finish)
			got := outcome(t, done)
			if !h.holds("lamp") {
				t.Fatal("the old run dropped the newer run's claim")
			}
			if got != engine.ResultCancelled {
				t.Fatalf("the old run ended %q", got)
			}
		})
	}
}

// Any way of ending badly counts, not just the ones a step sees.
func TestStoppedRunWithAnEndedContextReleasesNothing(t *testing.T) {
	for name, reason := range map[string]stopReason{"late": stopLate, "too long": stopTooLong, "edited": stopEdited} {
		t.Run(name, func(t *testing.T) {
			h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
			f := newFlow(h, c, l, nil)
			h.claims["lamp"] = false
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			r := &Run{f: f, ctx: ctx, id: "bedtime/1", stopped: &stop{reason: reason}}
			if got := r.conclude(l.logger(), nil); got != engine.ResultCancelled {
				t.Fatalf("got %q", got)
			}
			if !h.holds("lamp") {
				t.Fatal("it released a claim a newer run may hold")
			}
		})
	}
}

// A run can look at its context, find it live, and be cancelled just after. The newer run's claim
// has to wait for its release, or the release lands on it.
func TestReleaseWaitsOutANewRunsClaim(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	h.releasing, h.entered = make(chan struct{}), make(chan struct{})
	first := true
	f := newFlow(h, c, l, nil)
	f.Body = func(r *Run) error {
		if err := r.Apply(set("lamp")); err != nil {
			return err
		}
		if first {
			first = false
			return errors.New("no")
		}
		return nil
	}
	old := make(chan string, 1)
	go func() { old <- f.Start(context.Background()) }()
	<-h.entered
	newer := make(chan string, 1)
	go func() { newer <- f.Start(context.Background()) }()
	select {
	case <-newer:
		t.Fatal("the newer run claimed in the middle of the old run's release")
	case <-time.After(50 * time.Millisecond):
	}
	close(h.releasing)
	outcome(t, old)
	outcome(t, newer)
	if !h.holds("lamp") {
		t.Fatal("the release landed on the newer run's claim")
	}
}

// Claims belong to the flow, so a later failing run ending an earlier run's claim is intended.
func TestFailingRunReleasesAnEarlierRunsClaim(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, func(r *Run) error { return r.Apply(set("lamp")) })
	if got := f.Start(context.Background()); got != engine.ResultSucceeded {
		t.Fatalf("got %q", got)
	}
	if !h.holds("lamp") {
		t.Fatal("a run that succeeded released its claim")
	}
	f.Body = func(*Run) error { return errors.New("no") }
	f.Start(context.Background())
	if h.holds("lamp") {
		t.Fatal("the earlier run's claim stayed")
	}
}

// A failed notification can't reopen a valve.
func TestRunEndingBadlyKeepsItsSafetyClaims(t *testing.T) {
	safety := set("valve")
	safety.Priority = engine.PrioritySafety
	notify := func(r *Run) error {
		return r.Do("notify", nil, func(context.Context) error { return errors.New("no signal") })
	}
	t.Run("failed", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		newFlow(h, c, l, func(r *Run) error {
			if err := r.Apply(safety); err != nil {
				return err
			}
			return notify(r)
		}).Start(context.Background())
		if !h.holds("valve") {
			t.Fatal("a failed notification released a Safety claim")
		}
	})
	t.Run("left by an earlier run", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, func(r *Run) error { return r.Apply(safety) })
		f.Start(context.Background())
		f.Body = notify
		f.Start(context.Background())
		if !h.holds("valve") {
			t.Fatal("a later failure released a Safety claim")
		}
	})
	t.Run("after a replay", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, func(r *Run) error {
			if err := r.Apply(safety); err != nil {
				return err
			}
			r.Sleep(time.Hour)
			return nil
		})
		crash(t, f, c)
		c.Step(10 * time.Hour)
		outcome(t, resume(context.Background(), t, f, h, l))
		if !h.holds("valve") {
			t.Fatal("a run that gave up released a Safety claim")
		}
	})
}

func TestRecoveringARunNothingContinues(t *testing.T) {
	logged := func(h *fakeHome, c *clock.Fake, l *logs, body func(*Run) error) {
		crash(t, newFlow(h, c, l, body), c)
	}
	t.Run("its flow has gone: everything stays, for the host to end", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		logged(h, c, l, claimThenSleep)
		if bodies := Recover(context.Background(), "home", nil, h.saved(), h, h); len(bodies) != 0 {
			t.Fatalf("got %d bodies", len(bodies))
		}
		if !h.holds("lamp") || len(h.saved()) != 1 || len(h.order) != 0 {
			t.Fatalf("a binary without the flow ended it: %v", h.order)
		}
	})
	t.Run("an unreadable journal of a gone flow stays too", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		logged(h, c, l, claimThenSleep)
		for id := range h.journal {
			h.journal[id] = append(h.journal[id], []byte("not json"))
		}
		Recover(context.Background(), "home", nil, h.saved(), h, h)
		if !h.holds("lamp") || len(h.saved()) != 1 {
			t.Fatal("a binary without the flow ended it")
		}
	})
	t.Run("an unreadable journal is named by its run ID", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, claimThenSleep)
		crash(t, f, c)
		for id := range h.journal {
			h.journal[id] = append(h.journal[id], []byte("not json"))
		}
		Recover(context.Background(), "home", map[string]*Flow{f.Key.Name: f}, h.saved(), h, h)
		if h.holds("lamp") {
			t.Fatal("an unreadable journal's claim was kept for ever")
		}
		if h.releasedFor.Name != "bedtime" {
			t.Fatalf("released for %v", h.releasedFor)
		}
	})
	t.Run("a failed release keeps the journal", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, claimThenSleep)
		crash(t, f, c)
		for id := range h.journal {
			h.journal[id] = append(h.journal[id], []byte("not json"))
		}
		h.releaseErr = errors.New("no host")
		Recover(context.Background(), "home", map[string]*Flow{f.Key.Name: f}, h.saved(), h, h)
		if len(h.saved()) != 1 {
			t.Fatal("the only record of the claim went")
		}
	})
	t.Run("a replaced run is left alone", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, claimThenSleep)
		crash(t, f, c)
		c.Step(time.Minute)
		crash(t, f, c)
		Recover(context.Background(), "home", map[string]*Flow{f.Key.Name: f}, h.saved(), h, h)
		if !h.holds("lamp") {
			t.Fatal("a replaced run dropped the claim of the run that replaced it")
		}
	})
	t.Run("an unreadable journal leaves a run that's carrying on", func(t *testing.T) {
		h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
		f := newFlow(h, c, l, claimThenSleep)
		crash(t, f, c)
		for id := range h.journal {
			h.journal[id] = append(h.journal[id], []byte("not json"))
		}
		c.Step(time.Minute)
		crash(t, f, c)
		Recover(context.Background(), "home", map[string]*Flow{f.Key.Name: f}, h.saved(), h, h)
		if !h.holds("lamp") {
			t.Fatal("a damaged run dropped the claim of a run that's carrying on")
		}
	})
}

// With no condition, the step can never end, and the engine must not see it.
func TestWaitWithNoConditionFailsTheRun(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, func(r *Run) error {
		r.WaitFor(nil, time.Hour)
		return nil
	})
	if got := f.Start(context.Background()); got != engine.ResultFailed {
		t.Fatalf("got %q", got)
	}
}

func TestStepFailingWithItsContextEndedPausesTheRun(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	process, die := context.WithCancel(context.Background())
	f := newFlow(h, c, l, func(r *Run) error {
		return r.Do("call", nil, func(context.Context) error {
			die()
			return errors.New("the connection closed")
		})
	})
	f.Process = process
	if got := f.Start(process); got != engine.ResultCancelled {
		t.Fatalf("got %q", got)
	}
	if len(h.saved()) != 1 {
		t.Fatal("a stopping process lost a run")
	}
}

func TestTimeoutAlreadyPastLooksOnce(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	var got []bool
	f := newFlow(h, c, l, func(r *Run) error {
		r.Sleep(-time.Hour)
		got = append(got, r.WaitFor(func(engine.Vars) (bool, error) { return true, nil }, -time.Hour))
		return nil
	})
	if result := f.Start(context.Background()); result != engine.ResultSucceeded {
		t.Fatalf("got %q", result)
	}
	if len(got) != 1 || !got[0] {
		t.Fatalf("got %v", got)
	}
}

func TestTooManyStepsEndTheRun(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, func(r *Run) error {
		for {
			r.Now()
		}
	})
	if got := f.Start(context.Background()); got != engine.ResultFailed {
		t.Fatalf("got %q", got)
	}
	if !l.has("its journal is too long") {
		t.Fatalf("not logged: %v", l.lines)
	}
}

func TestDoRefusesAHugeResult(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	var got error
	f := newFlow(h, c, l, func(r *Run) error {
		var s string
		got = r.Do("huge", &s, func(context.Context) error {
			s = strings.Repeat("x", MaxResult)
			return nil
		})
		return nil
	})
	f.Start(context.Background())
	if got == nil {
		t.Fatal("a result over the limit was recorded")
	}
}

func TestRecoverKeepsOnlyTheNewestRunOfAFlow(t *testing.T) {
	h, c, l := newFakeHome(), clock.NewFake(epoch), &logs{}
	f := newFlow(h, c, l, func(*Run) error { return nil })
	add := func(id string, at time.Time, flow string) {
		if _, err := h.Journal(context.Background(), id, mustEntry(t, entry{Kind: kindStart, ID: flow, At: at})); err != nil {
			t.Fatal(err)
		}
	}
	add("bedtime/old", epoch, "bedtime")
	add("bedtime/new", epoch.Add(time.Minute), "bedtime")
	add("removed/x", epoch, "removed")
	if _, err := h.Journal(context.Background(), "junk", []byte("{")); err != nil {
		t.Fatal(err)
	}
	bodies := Recover(context.Background(), "home", map[string]*Flow{"bedtime": f}, h.saved(), h, h)
	if len(bodies) != 1 || bodies["bedtime"] == nil {
		t.Fatalf("resumed %v", bodies)
	}
	saved := h.saved()
	kept := make([]string, 0, len(saved))
	for _, s := range saved {
		kept = append(kept, s.ID)
	}
	slices.Sort(kept)
	if !slices.Equal(kept, []string{"bedtime/new", "removed/x"}) { // A gone flow's are the host's to end.
		t.Fatalf("kept %v", kept)
	}
}
