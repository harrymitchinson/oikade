package host

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// DefaultStopTimeout is how long the host gives itself to stop, well under systemd's
	// TimeoutStopSec.
	DefaultStopTimeout = 10 * time.Second
	// writePoll is how often the host looks whether the last writes have finished.
	writePoll = 10 * time.Millisecond
)

// stage is the goroutines one phase of stopping ends.
type stage struct {
	ctx     context.Context
	cancel  context.CancelFunc
	running sync.WaitGroup
}

// newStage makes a stage whose context ends only when it's told to, whatever ends parent.
func newStage(parent context.Context) *stage {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	return &stage{ctx: ctx, cancel: cancel}
}

func (s *stage) run(f func(context.Context)) { s.running.Go(func() { f(s.ctx) }) }

// stop ends the stage's context, and returns once its goroutines have.
func (s *stage) stop() {
	s.cancel()
	s.running.Wait()
}

// stopPhase is one step of stopping the host. name is what the log calls it.
type stopPhase struct {
	name string
	run  func(ctx context.Context)
}

// stopInOrder runs the phases one at a time, each given a context that ends at the deadline, and says
// whether they all finished. A phase still running at the deadline is left behind and the rest never
// start, because a host that's asked to stop has to stop: the host only ever crashes safe, so what's
// cut off is a tidy goodbye and no data.
func stopInOrder(log *slog.Logger, timeout time.Duration, phases []stopPhase) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for i, phase := range phases {
		if ctx.Err() == nil {
			log.Info(phase.name)
			done := make(chan struct{})
			go func() {
				defer close(done)
				phase.run(ctx)
			}()
			select {
			case <-done:
				continue
			case <-ctx.Done():
			}
			// Finishing just as the deadline passed still counts.
			select {
			case <-done:
				continue
			default:
			}
		}
		skipped := make([]string, 0, len(phases)-i-1)
		for _, later := range phases[i+1:] {
			skipped = append(skipped, later.name)
		}
		log.Error("Cut off host shutdown at its deadline", "phase", phase.name, "skipped", skipped, "timeout", timeout.String())
		return false
	}
	return true
}

// finishWrites waits until nothing is busy, or ctx ends.
func finishWrites(ctx context.Context, busy func() bool) {
	tick := time.NewTicker(writePoll)
	defer tick.Stop()
	for busy() {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
