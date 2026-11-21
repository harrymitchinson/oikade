package matterbridge

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/clock"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
)

// coalesceWindow is how long writes to a device gather before the latest is sent. Controllers
// send bursts: a swipe in the Home app sends several On/Off commands within 10 ms.
const coalesceWindow = 50 * time.Millisecond

// travelWindow is how long writes to a capability that travels, as a blind's position, gather
// after the last one. The Home app sends a slider's move as it goes: 1% and then 18% a second
// later for one drag of a shutter (OIK-337). Each is a new run on the motor, which stops it to
// start the next, so the shutter stutters. A move starts this much later, which a shutter's travel
// of tens of seconds hides.
const travelWindow = 1500 * time.Millisecond

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

// gathering is a device's burst while its writes gather.
type gathering struct {
	// due is when the burst ends: the latest of each write's time plus its window.
	due time.Time
	// timer fires at due. A write that moves due replaces it, and wakes the burst.
	timer clock.Timer
	wake  chan struct{}
	// over is set once the burst has ended, or Send has ended it, and is waiting to be sent or
	// being sent. A write then still joins it, but doesn't move due.
	over bool
}

// travels says a capability goes from one value to the next over time, as a blind does, rather
// than being at its new value at once.
func travels(name string) bool { return capability.SettleOf(name) > capability.DefaultSettle }

// Coalescer gathers writes from controllers per device and capability, latest wins, and sends
// each once the burst is over. It drops a write that only asks for what the device already has,
// or was just sent, so a burst that ends where it started doesn't chatter a relay.
//
// A burst is over once each of its writes has had its window: Window from when it came, or
// TravelWindow after the last write to a capability that travels.
type Coalescer struct {
	Setter driver.Setter
	// Current returns a capability's current value on the device, if known.
	Current func(dev ident.ID, name string) (any, bool)
	// Window is how long writes gather; 0 means 50 ms.
	Window time.Duration
	// TravelWindow is how long writes to a capability that travels gather after the last; 0
	// means 1.5 s.
	TravelWindow time.Duration
	// Timeout bounds each write.
	Timeout time.Duration
	// Clock is the time writes gather and are compared on; nil is the real clock.
	Clock clock.Clock

	mu      sync.Mutex
	pending map[writeKey]any
	waiting map[ident.ID]*gathering
	// sending holds a device's lock while a burst is sent, so a later burst waits for it.
	sending map[ident.ID]*sync.Mutex
	sent    map[writeKey]sent
	ctx     context.Context
	// flushes counts the bursts gathering or being sent.
	flushes sync.WaitGroup
	// bursts is the same count, which Busy can read while Add is running.
	bursts atomic.Int64
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

// Busy says whether any write is still gathering or being sent.
func (c *Coalescer) Busy() bool { return c.bursts.Load() > 0 }

// Add takes a write. It doesn't block.
func (c *Coalescer) Add(dev ident.ID, name string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx == nil {
		// A write before Start still goes, rather than crashing the host.
		c.ctx = context.Background()
	}
	if c.pending == nil {
		c.pending, c.waiting, c.sent = map[writeKey]any{}, map[ident.ID]*gathering{}, map[writeKey]sent{}
		c.sending = map[ident.ID]*sync.Mutex{}
	}
	c.pending[writeKey{dev, name}] = value
	w := c.window(name)
	due := c.now().Add(w)
	if g := c.waiting[dev]; g != nil {
		if !g.over && due.After(g.due) {
			g.timer.Stop()
			g.due, g.timer = due, c.clock().NewTimer(w)
			g.wakeUp()
		}
		return
	}
	g := &gathering{due: due, timer: c.clock().NewTimer(w), wake: make(chan struct{}, 1)}
	c.waiting[dev] = g
	c.bursts.Add(1)
	c.flushes.Go(func() {
		defer c.bursts.Add(-1)
		c.mu.Lock()
		ctx := c.ctx
		c.mu.Unlock()
		if c.gather(ctx, g) {
			c.flush(dev)
		}
	})
}

// Send ends a device's burst now, rather than when its writes' windows are over: a stop can't
// wait for more of a move. A write after it still joins the burst, without holding it back. It
// doesn't block.
func (c *Coalescer) Send(dev ident.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g := c.waiting[dev]; g != nil {
		g.over = true
		g.wakeUp()
	}
}

// SendAll ends every burst now, as the host stops: a write still gathering would otherwise use up
// the time it has to reach its device.
func (c *Coalescer) SendAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.waiting {
		g.over = true
		g.wakeUp()
	}
}

// Gathering says a write to a device's capability is waiting to be sent: the device hasn't been
// told of it.
func (c *Coalescer) Gathering(dev ident.ID, name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.pending[writeKey{dev, name}]
	return ok
}

func (g *gathering) wakeUp() {
	select {
	case g.wake <- struct{}{}:
	default: // already woken
	}
}

// gather waits for a burst to be over, and says whether it is, rather than shutting down.
func (c *Coalescer) gather(ctx context.Context, g *gathering) bool {
	for {
		c.mu.Lock()
		timer := g.timer
		if g.over || !c.now().Before(g.due) {
			g.over = true
			timer.Stop()
			c.mu.Unlock()
			return true
		}
		c.mu.Unlock()
		select {
		case <-timer.C():
		case <-g.wake:
		case <-ctx.Done():
			c.mu.Lock()
			g.timer.Stop()
			c.mu.Unlock()
			return false
		}
	}
}

// window is how long a write to the capability gathers.
func (c *Coalescer) window(name string) time.Duration {
	switch {
	case travels(name) && c.TravelWindow != 0:
		return c.TravelWindow
	case travels(name):
		return travelWindow
	case c.Window != 0:
		return c.Window
	}
	return coalesceWindow
}

// flush sends a device's latest writes: a level before turning on, and turning off before
// anything else, so a light doesn't flash at its old level.
//
// A device's bursts are sent one at a time, in the order they ended, so a slow device can't take
// an older write after a newer one. A burst waiting for the last to finish still gathers writes.
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
		// A device on its way, as a blind travels, isn't at what it reports, so writing that stops
		// it.
		if !travels(w.name) && reflect.DeepEqual(c.baseline(k), w.value) {
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

// Reported says the device has spoken, so what was last sent no longer says what it's doing:
// anything could have moved it since. A repeated write then counts against the device's own state.
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
