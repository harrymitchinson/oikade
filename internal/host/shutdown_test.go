package host

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestStopInOrderRunsEveryPhaseInOrder(t *testing.T) {
	t.Parallel()
	var ran []string
	phase := func(name string) stopPhase {
		return stopPhase{name: name, run: func(context.Context) { ran = append(ran, name) }}
	}
	if !stopInOrder(slog.New(slog.DiscardHandler), time.Minute, []stopPhase{phase("one"), phase("two"), phase("three")}) {
		t.Error("the phases didn't all finish")
	}
	if want := []string{"one", "two", "three"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
}

func TestAShutdownStuckPastItsDeadlineStillEnds(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&out, nil))
	stuck := make(chan struct{})
	defer close(stuck)
	var after atomic.Bool
	ended := make(chan bool, 1)
	go func() {
		ended <- stopInOrder(log, 20*time.Millisecond, []stopPhase{
			{name: "fine", run: func(context.Context) {}},
			{name: "stuck", run: func(context.Context) { <-stuck }},
			{name: "next", run: func(context.Context) { after.Store(true) }},
			{name: "last", run: func(context.Context) { after.Store(true) }},
		})
	}()
	select {
	case finished := <-ended:
		if finished {
			t.Error("a stuck phase counted as finished")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown never ended")
	}
	if after.Load() {
		t.Error("a phase started after the deadline")
	}
	var cut struct {
		Msg     string   `json:"msg"`
		Phase   string   `json:"phase"`
		Skipped []string `json:"skipped"`
	}
	for line := range bytes.Lines(out.Bytes()) {
		if err := json.Unmarshal(line, &cut); err != nil {
			t.Fatal(err)
		}
	}
	if cut.Msg != "Cut off host shutdown at its deadline" || cut.Phase != "stuck" || !slices.Equal(cut.Skipped, []string{"next", "last"}) {
		t.Errorf("the last line was %+v", cut)
	}
}

func TestACutOffKillsWhatIsStillRunning(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &Host{}
	defer h.track(cmd)()
	h.killProcesses()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err == nil {
			t.Error("the process exited by itself")
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the process is still running")
	}
}

func TestFinishWritesWaitsForThemOrItsDeadline(t *testing.T) {
	t.Parallel()
	polls := 0
	finishWrites(t.Context(), func() bool {
		polls++
		return polls < 3
	})
	if polls != 3 {
		t.Errorf("looked %d times, want 3", polls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	finishWrites(ctx, func() bool { return true }) // Returns: a write that never finishes can't hold it.
}
