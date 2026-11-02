package matterbridge

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// Root is where Matter bridges publish, under <root><namespace>/<bridge>/. No namespace starts
// with "_", so it can't collide with a plugin's root.
const Root = "oikade/v1/_matter/"

// Topics under a bridge's root.
const (
	// writesTopic carries writes from controllers. They aren't retained: a write is an event,
	// and one replayed on a later start would be taken for a new one.
	writesTopic = "writes"
	// fabricsTopic carries the bridge's fabrics, retained, for the runtime to put in status.
	fabricsTopic = "fabrics"
)

// announceTimeout bounds how long a write waits to be announced.
const announceTimeout = time.Second

// Write is a write from a Matter controller, as a bridge announces it.
type Write struct {
	// Bridge is the Bridge that took the write, in the device's namespace.
	Bridge     string `json:"bridge"`
	Device     string `json:"device"`
	Capability string `json:"capability"`
	// Value is bool or float64.
	Value any `json:"value"`
}

// Announcer is a driver.Setter that announces each write to an arbitrated capability on the bus
// before passing it to Setter, so the runtime can tell a write from a Matter controller from
// other outside changes. The write doesn't depend on the announcement, or on the runtime.
type Announcer struct {
	Setter driver.Setter
	Bus    mqtt.Bus
	// Store says which capabilities are arbitrated. Only writes to those are announced, since the
	// runtime ignores the rest. Nil announces every write.
	Store *hub.Store
	// Bridge is the Bridge the writes come from.
	Bridge types.NamespacedName
}

var _ driver.Setter = (*Announcer)(nil)

// Set implements driver.Setter. The announcement goes first, so the runtime has it before the
// device reports the change.
func (a *Announcer) Set(ctx context.Context, dev types.NamespacedName, name string, value any) error {
	if a.announces(dev, name, value) {
		raw, err := json.Marshal(Write{Bridge: a.Bridge.Name, Device: dev.Name, Capability: name, Value: value})
		if err == nil {
			actx, cancel := context.WithTimeout(ctx, announceTimeout)
			err = a.Bus.Publish(actx, Root+dev.Namespace+"/"+a.Bridge.Name+"/"+writesTopic, raw, false)
			cancel()
		}
		if err != nil {
			logf.FromContext(ctx).Error(err, "Failed to announce Matter write", "device", dev, "capability", name)
		}
	}
	return a.Setter.Set(ctx, dev, name, value)
}

func (a *Announcer) announces(dev types.NamespacedName, name string, value any) bool {
	if _, err := capability.Validate(name, value); err != nil {
		return false
	}
	if a.Store == nil {
		return true
	}
	snap, ok := a.Store.Get(dev)
	return ok && (snap.Online == nil || *snap.Online) && snap.Controls(name)
}

// SubscribeWrites calls f with every write from a Matter controller that any bridge announces. f
// runs on the bus's receive goroutine, so it must not block.
func SubscribeWrites(ctx context.Context, bus mqtt.Bus, f func(dev types.NamespacedName, name string, value any)) (func(), error) {
	log := logf.FromContext(ctx)
	return bus.Subscribe(ctx, Root+"+/+/"+writesTopic, func(m mqtt.Message) {
		dev, name, value, err := parseWrite(m)
		if err != nil {
			log.V(1).Info("Ignored malformed Matter write", "topic", m.Topic, "error", err.Error())
			return
		}
		f(dev, name, value)
	})
}

func parseWrite(m mqtt.Message) (types.NamespacedName, string, any, error) {
	parts := strings.Split(strings.TrimPrefix(m.Topic, Root), "/")
	if len(parts) != 3 {
		return types.NamespacedName{}, "", nil, errors.New("unexpected topic")
	}
	var w Write
	if err := json.Unmarshal(m.Payload, &w); err != nil {
		return types.NamespacedName{}, "", nil, err
	}
	switch {
	case w.Bridge != parts[1]:
		return types.NamespacedName{}, "", nil, errors.New("bridge doesn't match the topic")
	case w.Device == "":
		return types.NamespacedName{}, "", nil, errors.New("no device")
	}
	v, err := capability.Validate(w.Capability, w.Value)
	if err != nil {
		return types.NamespacedName{}, "", nil, err
	}
	return types.NamespacedName{Namespace: parts[0], Name: w.Device}, w.Capability, v, nil
}

// coalesceWindow is how long writes to a device gather before the latest is sent. Controllers
// send bursts: a swipe in the Home app sends several On/Off commands within 10 ms.
const coalesceWindow = 50 * time.Millisecond

type writeKey struct {
	dev  types.NamespacedName
	name string
}

// Coalescer gathers writes from controllers per device and capability, latest wins, and sends
// each once the burst is over. It drops a write that only asks for the device's current state,
// so a burst that ends where it started doesn't chatter a relay.
type Coalescer struct {
	Setter driver.Setter
	// Current returns a capability's current value on the device, if known.
	Current func(dev types.NamespacedName, name string) (any, bool)
	// Window is how long writes gather; 0 means 50 ms.
	Window time.Duration
	// Timeout bounds each write.
	Timeout time.Duration

	mu      sync.Mutex
	pending map[writeKey]any
	timers  map[types.NamespacedName]*time.Timer
	ctx     context.Context
}

// Start sets the context writes run in. Call it before Add.
func (c *Coalescer) Start(ctx context.Context) { c.ctx = ctx }

// Add takes a write. It doesn't block.
func (c *Coalescer) Add(dev types.NamespacedName, name string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending, c.timers = map[writeKey]any{}, map[types.NamespacedName]*time.Timer{}
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

// flush sends a device's latest writes.
func (c *Coalescer) flush(dev types.NamespacedName) {
	c.mu.Lock()
	delete(c.timers, dev)
	var keys []writeKey
	var values []any
	for k, v := range c.pending {
		if k.dev == dev {
			keys, values = append(keys, k), append(values, v)
			delete(c.pending, k)
		}
	}
	c.mu.Unlock()
	log := logf.FromContext(c.ctx)
	for i, k := range keys {
		if cur, ok := c.Current(dev, k.name); ok && reflect.DeepEqual(cur, values[i]) {
			log.V(1).Info("Skipped Matter write that changes nothing", "device", dev, "capability", k.name)
			continue
		}
		ctx, cancel := context.WithTimeout(c.ctx, c.Timeout)
		if err := c.Setter.Set(ctx, dev, k.name, values[i]); err != nil {
			log.Error(err, "Failed to set capability from Matter", "device", dev, "capability", k.name)
		}
		cancel()
	}
}
