package matterbridge

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
)

// coalesceWindow is how long writes to a device gather before the latest is sent. Controllers
// send bursts: a swipe in the Home app sends several On/Off commands within 10 ms.
const coalesceWindow = 50 * time.Millisecond

// sentFor is how long a sent value, rather than the device's report, is what a later write is
// compared with: long enough for a device to report the change.
const sentFor = 10 * time.Second

type writeKey struct {
	dev  ident.ID
	name string
}

type sent struct {
	value any
	at    time.Time
}

// Coalescer gathers writes from controllers per device and capability, latest wins, and sends
// each once the burst is over. It drops a write that only asks for what the device already has,
// or was just sent, so a burst that ends where it started doesn't chatter a relay.
type Coalescer struct {
	Setter driver.Setter
	// Current returns a capability's current value on the device, if known.
	Current func(dev ident.ID, name string) (any, bool)
	// Window is how long writes gather; 0 means 50 ms.
	Window time.Duration
	// Timeout bounds each write.
	Timeout time.Duration
	// Clock is the time writes gather and are compared on; nil is the real clock.
	Clock clock.Clock

	mu      sync.Mutex
	pending map[writeKey]any
	waiting map[ident.ID]bool
	// sending holds a device's lock while a burst is sent, so a later burst waits for it.
	sending map[ident.ID]*sync.Mutex
	sent    map[writeKey]sent
	ctx     context.Context
	// flushes counts the bursts gathering or being sent.
	flushes sync.WaitGroup
	// ended, if set, hears a burst end, before it waits for the last to be sent. Tests use it.
	ended func(ident.ID)
}

// Start sets the context writes run in. Call it before Add.
func (c *Coalescer) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ctx = ctx
}

func (c *Coalescer) clock() clock.Clock {
	if c.Clock != nil {
		return c.Clock
	}
	return clock.Real{}
}

func (c *Coalescer) now() time.Time { return c.clock().Now() }

// Add takes a write. It doesn't block.
func (c *Coalescer) Add(dev ident.ID, name string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx == nil {
		// A write before Start still goes, rather than crashing the host.
		c.ctx = context.Background()
	}
	if c.pending == nil {
		c.pending, c.waiting, c.sent = map[writeKey]any{}, map[ident.ID]bool{}, map[writeKey]sent{}
		c.sending = map[ident.ID]*sync.Mutex{}
	}
	c.pending[writeKey{dev, name}] = value
	if c.waiting[dev] {
		return
	}
	c.waiting[dev] = true
	w := c.Window
	if w == 0 {
		w = coalesceWindow
	}
	timer := c.clock().NewTimer(w)
	c.flushes.Go(func() {
		defer timer.Stop()
		c.mu.Lock()
		ctx := c.ctx
		c.mu.Unlock()
		select {
		case <-timer.C():
			c.flush(dev)
		case <-ctx.Done():
		}
	})
}

// flush sends a device's latest writes: a level before turning on, and turning off before
// anything else, so a light doesn't flash at its old level.
//
// A device's bursts are sent one at a time, in the order they ended, so a slow device can't take an
// older write after a newer one. A burst waiting for the last to finish still gathers writes.
func (c *Coalescer) flush(dev ident.ID) {
	type write struct {
		name  string
		value any
	}
	c.mu.Lock()
	sending := c.sending[dev]
	if sending == nil {
		sending = &sync.Mutex{}
		c.sending[dev] = sending
	}
	c.mu.Unlock()
	if c.ended != nil {
		c.ended(dev)
	}
	sending.Lock()
	defer sending.Unlock()
	if c.ctx.Err() != nil {
		return // shutting down: a write now would only fail
	}
	c.mu.Lock()
	delete(c.waiting, dev)
	var writes []write
	for k, v := range c.pending {
		if k.dev == dev {
			writes = append(writes, write{k.name, v})
			delete(c.pending, k)
		}
	}
	c.mu.Unlock()
	rank := func(w write) int {
		on, isBool := w.value.(bool)
		switch {
		case isBool && !on:
			return 0
		case isBool:
			return 2
		}
		return 1
	}
	slices.SortFunc(writes, func(a, b write) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a.name, b.name)
	})
	log := logging.FromContext(c.ctx)
	for _, w := range writes {
		k := writeKey{dev, w.name}
		if reflect.DeepEqual(c.baseline(k), w.value) {
			log.Debug("Skipped Matter write that changes nothing", "device", dev, "capability", w.name)
			continue
		}
		c.mu.Lock()
		c.sent[k] = sent{w.value, c.now()}
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(c.ctx, c.Timeout)
		if err := c.Setter.Set(ctx, dev, w.name, w.value); err != nil {
			if refused, ok := errors.AsType[*driver.Refused](err); ok {
				log.Info("Refused Matter write", "device", dev, "capability", w.name, "value", w.value,
					"claim", refused.Holder, "claimLevel", refused.Level)
			} else {
				log.Error("Failed to set capability from Matter", "err", err, "device", dev, "capability", w.name)
			}
			c.mu.Lock()
			delete(c.sent, k)
			c.mu.Unlock()
		}
		cancel()
	}
}

// Reported says the device has spoken, so what was last sent no longer says what it's doing: anything
// could have moved it since. A repeated write then counts against the device's own state.
func (c *Coalescer) Reported(dev ident.ID, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sent, writeKey{dev, name})
}

// baseline is what a write is compared with: the value last sent, while the device may not have
// reported it yet, or else the device's state.
func (c *Coalescer) baseline(k writeKey) any {
	c.mu.Lock()
	s, ok := c.sent[k]
	c.mu.Unlock()
	if ok && c.now().Sub(s.at) < sentFor {
		return s.value
	}
	if v, ok := c.Current(k.dev, k.name); ok {
		return v
	}
	return nil
}
