package matterbridge

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
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
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	pending map[writeKey]any
	timers  map[ident.ID]*time.Timer
	sent    map[writeKey]sent
	ctx     context.Context
}

// Start sets the context writes run in. Call it before Add.
func (c *Coalescer) Start(ctx context.Context) { c.ctx = ctx }

func (c *Coalescer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Add takes a write. It doesn't block.
func (c *Coalescer) Add(dev ident.ID, name string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending, c.timers, c.sent = map[writeKey]any{}, map[ident.ID]*time.Timer{}, map[writeKey]sent{}
	}
	c.pending[writeKey{dev, name}] = value
	if c.timers[dev] == nil {
		w := c.Window
		if w == 0 {
			w = coalesceWindow
		}
		c.timers[dev] = time.AfterFunc(w, func() { c.flush(dev) })
	}
}

// flush sends a device's latest writes: a level before turning on, and turning off before
// anything else, so a light doesn't flash at its old level.
func (c *Coalescer) flush(dev ident.ID) {
	type write struct {
		name  string
		value any
	}
	c.mu.Lock()
	delete(c.timers, dev)
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
	log := logr.FromContextOrDiscard(c.ctx)
	for _, w := range writes {
		k := writeKey{dev, w.name}
		if reflect.DeepEqual(c.baseline(k), w.value) {
			log.V(1).Info("Skipped Matter write that changes nothing", "device", dev, "capability", w.name)
			continue
		}
		c.mu.Lock()
		c.sent[k] = sent{w.value, c.now()}
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(c.ctx, c.Timeout)
		if err := c.Setter.Set(ctx, dev, w.name, w.value); err != nil {
			log.Error(err, "Failed to set capability from Matter", "device", dev, "capability", w.name)
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
