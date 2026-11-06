// Package driver defines how device integrations report to the host.
package driver

import (
	"context"
	"time"

	"github.com/harrymitchinson/oikade/internal/ident"
)

// Driver names for InstanceKey.
const (
	Zigbee2MQTT = "zigbee2mqtt"
	Plugin      = "plugin"
)

// InstanceKey identifies a driver instance. Objects of different drivers may share a name, so the
// driver is part of the key.
type InstanceKey struct {
	Driver string
	ident.ID
}

// Label is the instance label value on metrics: the bare name for Zigbee2MQTT, which predates
// other drivers, and <driver>/<name> otherwise.
func (k InstanceKey) Label() string {
	if k.Driver == Zigbee2MQTT {
		return k.Name
	}
	return k.Driver + "/" + k.Name
}

// Info is what a driver knows about a declared device.
type Info struct {
	// Found is false while the device is declared but not paired with the driver.
	Found        bool
	Capabilities []string
	Manufacturer string
	Model        string
	// Error is a driver-reported problem with the device, such as bad plugin parameters.
	Error string
}

// AppDevice is a device a driver instance sees in its app, declared or not: a Zigbee2MQTT
// instance's paired devices, or a plugin's discovered list.
type AppDevice struct {
	// ID is the driver's ID for the device: an IEEE address or a plugin's discovered ID.
	ID           string
	Manufacturer string
	Model        string
	Capabilities []string
	// Name and Room are as named in the app, empty if it has none.
	Name string
	Room string
}

// Update is a capability changing value. Drivers only report actual changes.
type Update struct {
	Device     ident.ID
	Capability string
	// Value is bool or float64, validated against the capability registry.
	Value any
	// Previous is nil the first time a value is seen.
	Previous any
	Time     time.Time
}

// Sink receives driver events. Calls arrive on the driver's receive goroutine and must not block.
type Sink interface {
	Discovered(dev ident.ID, info Info)
	Changed(u Update)
	Availability(dev ident.ID, online bool)
	// Seen reports any message from the device, changed or not.
	Seen(dev ident.ID, t time.Time)
	// Undeclared reports how many paired devices aren't on record, per driver instance.
	Undeclared(instance InstanceKey, count int)
	// Inventory reports every device a driver instance sees in its app, sorted by ID. Nil means
	// the instance doesn't list them, such as a plugin that doesn't discover.
	Inventory(instance InstanceKey, devices []AppDevice)
}

// Setter writes capabilities.
type Setter interface {
	Set(ctx context.Context, dev ident.ID, capability string, value any) error
}

// SetterFunc is a func as a Setter.
type SetterFunc func(ctx context.Context, dev ident.ID, capability string, value any) error

// Set implements Setter.
func (f SetterFunc) Set(ctx context.Context, dev ident.ID, capability string, value any) error {
	return f(ctx, dev, capability, value)
}
