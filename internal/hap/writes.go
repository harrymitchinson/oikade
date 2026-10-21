package hap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// WritesRoot is where bridges announce Home app writes, at <root><namespace>/<bridge>. No
// namespace starts with "_", so it can't collide with a plugin's root. The messages aren't
// retained: a write is an event, and one replayed on a later start would be taken for a new one.
const WritesRoot = "oikade/v1/_homekit/"

// announceTimeout bounds how long a write waits to be announced, so a slow broker leaves the write
// itself most of setTimeout.
const announceTimeout = time.Second

// Write is a Home app write, as a bridge announces it.
type Write struct {
	// Bridge is the Bridge that took the write, in the device's namespace.
	Bridge     string `json:"bridge"`
	Device     string `json:"device"`
	Capability string `json:"capability"`
	// Value is bool or float64.
	Value any `json:"value"`
}

// Announcer is a driver.Setter that announces each write to an arbitrated capability on the bus
// before passing it to Setter, so the runtime can tell a Home app write from other outside
// changes. The write doesn't depend on the announcement, or on the runtime: it reaches the device
// even when neither gets through.
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
// device reports the change: the bus delivers messages to it in the order the broker took them.
func (a *Announcer) Set(ctx context.Context, dev types.NamespacedName, name string, value any) error {
	if a.announces(dev, name, value) {
		raw, err := json.Marshal(Write{Bridge: a.Bridge.Name, Device: dev.Name, Capability: name, Value: value})
		if err == nil {
			actx, cancel := context.WithTimeout(ctx, announceTimeout)
			err = a.Bus.Publish(actx, WritesRoot+dev.Namespace+"/"+a.Bridge.Name, raw, false)
			cancel()
		}
		if err != nil {
			logf.FromContext(ctx).Error(err, "Failed to announce HomeKit write", "device", dev, "capability", name)
		}
	}
	return a.Setter.Set(ctx, dev, name, value)
}

// announces reports whether a write is worth announcing: a valid value for an arbitrated
// capability of a device that isn't offline. The others can't reach the device, and announcing
// them could pass a later change off as the Home app's.
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

// SubscribeWrites calls f with every Home app write any bridge announces. f runs on the bus's
// receive goroutine, so it must not block. Malformed announcements are logged to ctx's logger
// and dropped.
func SubscribeWrites(ctx context.Context, bus mqtt.Bus, f func(dev types.NamespacedName, name string, value any)) (func(), error) {
	log := logf.FromContext(ctx)
	return bus.Subscribe(ctx, WritesRoot+"+/+", func(m mqtt.Message) {
		dev, name, value, err := parseWrite(m)
		if err != nil {
			// Any client can publish here, so this is verbose.
			log.V(1).Info("Ignored malformed HomeKit write", "topic", m.Topic, "error", err.Error())
			return
		}
		f(dev, name, value)
	})
}

func parseWrite(m mqtt.Message) (types.NamespacedName, string, any, error) {
	namespace, bridge, _ := strings.Cut(strings.TrimPrefix(m.Topic, WritesRoot), "/")
	var w Write
	if err := json.Unmarshal(m.Payload, &w); err != nil {
		return types.NamespacedName{}, "", nil, err
	}
	switch {
	case w.Bridge != bridge:
		return types.NamespacedName{}, "", nil, errors.New("bridge doesn't match the topic")
	case w.Device == "":
		return types.NamespacedName{}, "", nil, errors.New("no device")
	}
	v, err := capability.Validate(w.Capability, w.Value)
	if err != nil {
		return types.NamespacedName{}, "", nil, err
	}
	return types.NamespacedName{Namespace: namespace, Name: w.Device}, w.Capability, v, nil
}
