// Package driver defines how device integrations report to the runtime.
package driver

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
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
	types.NamespacedName
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

// Update is a capability changing value. Drivers only report actual changes.
type Update struct {
	Device     types.NamespacedName
	Capability string
	// Value is bool or float64, validated against the capability registry.
	Value any
	// Previous is nil the first time a value is seen.
	Previous any
	Time     time.Time
}

// Sink receives driver events. Calls arrive on the driver's receive goroutine and must not block.
type Sink interface {
	Discovered(dev types.NamespacedName, info Info)
	Changed(u Update)
	Availability(dev types.NamespacedName, online bool)
	// Seen reports any message from the device, changed or not.
	Seen(dev types.NamespacedName, t time.Time)
	// Undeclared reports how many paired devices have no Device object, per driver instance.
	Undeclared(instance InstanceKey, count int)
}

// Setter writes capabilities.
type Setter interface {
	Set(ctx context.Context, dev types.NamespacedName, capability string, value any) error
}
