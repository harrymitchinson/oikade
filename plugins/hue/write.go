package hue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"oikade.dev/contract/v1"
	"oikade.dev/sdk/plugin"
)

// errLost is a write still waiting when the session ended.
var errLost = errors.New("lost the Hue bridge before the write went")

// write is one capability of one light, waiting its turn.
type write struct {
	device, light, capability string
	value                     any
	// done is told how the write went, or nil when a newer one replaced it, unless its Set stopped
	// waiting first. It's buffered, so telling it never waits.
	done chan error
	// sending is set once the write has left the queue, and abandoned once its Set has returned
	// without it, so the writer logs how it went instead. The writer's mu guards both.
	sending, abandoned bool
}

func (w write) body() map[string]any {
	if w.capability == contract.Brightness {
		return map[string]any{"dimming": map[string]any{"brightness": w.value}}
	}
	return map[string]any{"on": map[string]any{"on": w.value}}
}

// writer sends a session's writes to the bridge in the order they came, one every pace at most, as
// the bridge asks of light commands. A newer write to a light's capability takes the place of one
// still waiting, so the light ends where it was last set.
type writer struct {
	// pace is the least time between writes, and timeout bounds sending one.
	pace, timeout time.Duration
	log           *slog.Logger
	// lost ends the session, when the bridge doesn't answer a write.
	lost func(err error)

	mu      sync.Mutex
	waiting []*write
	closed  bool
	// wake is signalled when a write is added.
	wake chan struct{}
}

func newWriter(t timing, lost func(error), log *slog.Logger) *writer {
	return &writer{pace: t.pace, timeout: t.write, lost: lost, log: log, wake: make(chan struct{}, 1)}
}

// add queues a write, or replaces the one waiting for its light's capability. The replacement takes
// the older one's place unless something else for the light waits after it, which mustn't then be
// sent after the newer write.
func (w *writer) add(next *write) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errLost
	}
	for i, q := range w.waiting {
		if q.light != next.light || q.capability != next.capability {
			continue
		}
		w.finishLocked(q, nil)
		later := w.waiting[i+1:]
		for _, l := range later {
			if l.light == next.light {
				w.waiting = append(append(w.waiting[:i:i], later...), next)
				return nil
			}
		}
		w.waiting[i] = next
		return nil
	}
	w.waiting = append(w.waiting, next)
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

// finishLocked tells a write's Set how it went, or logs it if its Set has returned. Call with mu
// held.
func (w *writer) finishLocked(q *write, err error) {
	switch {
	case !q.abandoned:
		q.done <- err
	case errors.Is(err, errLost):
		w.log.Warn("Dropped Hue write", "err", err, "device", q.device, "capability", q.capability, "value", q.value)
	case err != nil:
		w.log.Warn("Could not write to Hue light", "err", err, "device", q.device, "capability", q.capability, "value", q.value)
	}
}

// abandon is a write's Set returning before it's done, which it may only while the write waits: once
// it's being sent, the bridge's answer is close.
func (w *writer) abandon(q *write) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if q.sending {
		return false
	}
	q.abandoned = true
	return true
}

func (w *writer) run(ctx context.Context, c *client) {
	var last time.Time
	for {
		w.mu.Lock()
		ready := len(w.waiting) > 0
		w.mu.Unlock()
		if !ready {
			select {
			case <-w.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		if wait := time.Until(last.Add(w.pace)); wait > 0 {
			sleep(ctx, wait)
			if ctx.Err() != nil {
				return
			}
		}
		// The head may have been replaced while the pace held it: the newest goes.
		w.mu.Lock()
		if len(w.waiting) == 0 {
			w.mu.Unlock()
			continue
		}
		next := w.waiting[0]
		w.waiting = w.waiting[1:]
		next.sending = true
		w.mu.Unlock()
		last = time.Now()
		sending, cancel := context.WithTimeout(ctx, w.timeout)
		err := c.put(sending, typeLight, next.light, next.body())
		cancel()
		var refused *apiError
		switch {
		case err != nil && ctx.Err() != nil:
			err = errLost
		case err != nil && !errors.As(err, &refused):
			// A bridge that doesn't answer a write is gone, whatever the stream says: every write
			// after it would wait as long.
			w.lost(err)
		}
		w.mu.Lock()
		w.finishLocked(next, err)
		w.mu.Unlock()
	}
}

// close ends the writer: what's waiting won't go.
func (w *writer) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for _, q := range w.waiting {
		w.finishLocked(q, errLost)
	}
	w.waiting = nil
}

// Set implements plugin.Handler. It queues the write for its turn, and returns once the bridge has
// taken it, before the light has changed: the event stream then shows it. A write still waiting for
// its turn after timing's set returns, and goes in its turn unless a newer one replaces it. It
// fails at once while there's no session.
func (b *bridge) Set(ctx context.Context, _ *plugin.Plugin, cmd contract.SetCommand) error {
	b.mu.Lock()
	var d *device
	if b.home != nil {
		d = b.home.devices[cmd.ID]
	}
	w := b.writer
	b.mu.Unlock()
	switch {
	case w == nil:
		return fmt.Errorf("can't reach %s", b.describe())
	case d == nil:
		return fmt.Errorf("no Hue device %s", cmd.ID)
	case d.light == "":
		return fmt.Errorf("%s has no light to set", cmd.ID)
	}
	switch v := cmd.Value.(type) {
	case bool:
		if cmd.Capability != contract.On {
			return fmt.Errorf("can't set %s to %v on %s", cmd.Capability, v, cmd.ID)
		}
	case float64:
		if cmd.Capability != contract.Brightness {
			return fmt.Errorf("can't set %s to %v on %s", cmd.Capability, v, cmd.ID)
		}
		if v < 0 || v > 100 {
			return fmt.Errorf("can't set brightness to %v on %s: it's from 0 to 100", v, cmd.ID)
		}
	default:
		return fmt.Errorf("can't set %s to %v on %s", cmd.Capability, cmd.Value, cmd.ID)
	}
	next := &write{device: cmd.ID, light: d.light, capability: cmd.Capability, value: cmd.Value, done: make(chan error, 1)}
	if err := w.add(next); err != nil {
		return err
	}
	t := time.NewTimer(b.timing.set)
	defer t.Stop()
	select {
	case err := <-next.done:
		return err
	case <-t.C:
		if w.abandon(next) {
			return nil
		}
	case <-ctx.Done():
		if w.abandon(next) {
			return ctx.Err()
		}
	}
	// It's being sent, which timing's write bounds.
	return <-next.done
}
