package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/harrymitchinson/oikade/internal/engine"
)

var epoch = time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)

// fakeHome is a journal and the home's actions, in memory.
type fakeHome struct {
	mu       sync.Mutex
	journal  map[string][][]byte
	finished []string
	applied  []string
	failing  error
	// met is what WaitFor's condition says, and woken ends a wait for it.
	woken chan struct{}
}

func newFakeHome() *fakeHome {
	return &fakeHome{journal: map[string][][]byte{}, woken: make(chan struct{})}
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
	return nil
}

func (h *fakeHome) Apply(_ context.Context, _ types.NamespacedName, a engine.Action) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, _ := a.(engine.Set)
	h.applied = append(h.applied, s.Device)
	return nil
}

func (h *fakeHome) WaitFor(ctx context.Context, _ types.NamespacedName, cond engine.Condition, _ time.Time) (bool, error) {
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
}

func (l *logs) logger() logr.Logger {
	return funcr.New(func(_, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, args)
	}, funcr.Options{})
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

func newFlow(h *fakeHome, c *clocktesting.FakeClock, l *logs, body func(*Run) error) *Flow {
	return &Flow{
		Key:  types.NamespacedName{Namespace: "home", Name: "bedtime"},
		Body: body, Journal: h, Actions: h, Clock: c, Log: l.logger(), Process: context.Background(),
	}
}

// resume restarts the flow's one saved run, as a new process does, and returns its result.
func resume(ctx context.Context, t *testing.T, f *Flow, h *fakeHome, l *logs) <-chan string {
	t.Helper()
	bodies := Recover(logr.NewContext(ctx, l.logger()), map[string]*Flow{f.Key.Name: f}, h.saved(), h)
	body := bodies[f.Key.Name]
	if body == nil {
		t.Fatal("nothing to resume")
	}
	result := make(chan string, 1)
	go func() { result <- body(ctx) }()
	return result
}

// parked waits until something is waiting on the clock.
func parked(t *testing.T, c *clocktesting.FakeClock) {
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
func crash(t *testing.T, f *Flow, c *clocktesting.FakeClock) {
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
			h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
			h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
}

func TestRunStopsWithoutFinishingWhenTheJournalFails(t *testing.T) {
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
			h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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

func TestTooManyStepsEndTheRun(t *testing.T) {
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
	f := newFlow(h, c, l, func(r *Run) error {
		for {
			r.Now()
		}
	})
	if got := f.Start(context.Background()); got != engine.ResultFailed {
		t.Fatalf("got %q", got)
	}
	if !l.has("too many steps") {
		t.Fatalf("not logged: %v", l.lines)
	}
}

func TestDoRefusesAHugeResult(t *testing.T) {
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	h, c, l := newFakeHome(), clocktesting.NewFakeClock(epoch), &logs{}
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
	bodies := Recover(context.Background(), map[string]*Flow{"bedtime": f}, h.saved(), h)
	if len(bodies) != 1 || bodies["bedtime"] == nil {
		t.Fatalf("resumed %v", bodies)
	}
	saved := h.saved()
	if len(saved) != 1 || saved[0].ID != "bedtime/new" {
		t.Fatalf("kept %v", saved)
	}
}
