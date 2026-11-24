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
	// done is told how the write went, or nil when a newer one replaced it. It's buffered, so
	// telling it never waits for a Set that stopped waiting.
	done chan error
}

// body is the change a write sends the bridge.
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
	pace time.Duration
	log  *slog.Logger

	mu      sync.Mutex
	waiting []*write
	closed  bool
	// wake is signalled when a write is added, or the writer closes.
	wake chan struct{}
}

func newWriter(pace time.Duration, log *slog.Logger) *writer {
	return &writer{pace: pace, log: log, wake: make(chan struct{}, 1)}
}

// add queues a write, or replaces the one waiting for its light's capability.
func (w *writer) add(next *write) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errLost
	}
	for i, q := range w.waiting {
		if q.light == next.light && q.capability == next.capability {
			q.done <- nil
			w.waiting[i] = next
			return nil
		}
	}
	w.waiting = append(w.waiting, next)
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

// run sends the writes until ctx ends.
func (w *writer) run(ctx context.Context, c *client) {
	var last time.Time
	for {
		w.mu.Lock()
		var next *write
		if len(w.waiting) > 0 {
			next = w.waiting[0]
		}
		w.mu.Unlock()
		if next == nil {
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
		next = w.waiting[0]
		w.waiting = w.waiting[1:]
		w.mu.Unlock()
		last = time.Now()
		err := c.put(ctx, typeLight, next.light, next.body())
		switch {
		case ctx.Err() != nil:
			err = errLost
		case err != nil:
			w.log.Warn("Could not write to Hue light", "err", err, "device", next.device, "capability", next.capability, "value", next.value)
		}
		next.done <- err
	}
}

// close ends the writer: what's waiting won't go, and its Set, if it still waits, says so.
func (w *writer) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for _, q := range w.waiting {
		q.done <- errLost
		w.log.Warn("Dropped Hue write", "err", errLost, "device", q.device, "capability", q.capability, "value", q.value)
	}
	w.waiting = nil
}

// Set implements plugin.Handler. It queues the write for its turn, and returns once the bridge has
// taken it, before the light has changed: the event stream then shows it. A write that's still
// waiting after timing's set returns, and goes in its turn unless a newer one replaces it. It fails
// at once while there's no session.
func (b *bridge) Set(ctx context.Context, _ *plugin.Plugin, cmd contract.SetCommand) error {
	b.mu.Lock()
	var d *device
	if b.home != nil {
		d = b.home.devices[cmd.ID]
	}
	w := b.writer
	b.mu.Unlock()
	switch {
	case d == nil:
		return fmt.Errorf("no Hue device %s", cmd.ID)
	case w == nil:
		return fmt.Errorf("can't reach %s", b.describe())
	case d.light == "":
		return fmt.Errorf("%s has no light to set", cmd.ID)
	}
	switch v := cmd.Value.(type) {
	case bool:
		if cmd.Capability != contract.On {
			return fmt.Errorf("%s takes a number, not %v", cmd.Capability, v)
		}
	case float64:
		if cmd.Capability != contract.Brightness || v < 0 || v > 100 {
			return fmt.Errorf("can't set %s to %v on %s: brightness is from 0 to 100", cmd.Capability, v, cmd.ID)
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
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
