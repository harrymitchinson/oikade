// Package driver defines how device integrations report to the host.
package driver

import (
	"cmp"
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/harrymitchinson/oikade/internal/devicetype"
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

// Label is the instance label value on metrics: the bare name for Zigbee2MQTT, and <driver>/<name>
// otherwise.
func (k InstanceKey) Label() string {
	if k.Driver == Zigbee2MQTT {
		return k.Name
	}
	return k.Driver + "/" + k.Name
}

// Product is what a driver says a device is, each field empty if it doesn't say. Its JSON is in the
// registry, and so in oikade list devices --json: add to it, and don't rename.
type Product struct {
	Manufacturer    string `json:"manufacturer,omitempty"`
	Model           string `json:"model,omitempty"`
	SerialNumber    string `json:"serialNumber,omitempty"`
	FirmwareVersion string `json:"firmwareVersion,omitempty"`
	HardwareVersion string `json:"hardwareVersion,omitempty"`
}

// Or is p, with each field it leaves empty taken from q.
func (p Product) Or(q Product) Product {
	return Product{
		Manufacturer: cmp.Or(p.Manufacturer, q.Manufacturer), Model: cmp.Or(p.Model, q.Model),
		SerialNumber: cmp.Or(p.SerialNumber, q.SerialNumber), FirmwareVersion: cmp.Or(p.FirmwareVersion, q.FirmwareVersion),
		HardwareVersion: cmp.Or(p.HardwareVersion, q.HardwareVersion),
	}
}

// Clean is p with each field on one line: no control characters, and no space around it, so a field
// that's only those is empty.
func (p Product) Clean() Product {
	return Product{
		Manufacturer: oneLine(p.Manufacturer), Model: oneLine(p.Model), SerialNumber: oneLine(p.SerialNumber),
		FirmwareVersion: oneLine(p.FirmwareVersion), HardwareVersion: oneLine(p.HardwareVersion),
	}
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, s))
}

// Info is what a driver knows about a declared device.
type Info struct {
	// Found is false while the device is declared but not paired with the driver.
	Found        bool
	Capabilities []string
	// DeviceType is what the device is, one of devicetype.OnOff, if the driver knows. A relay
	// doesn't, as it can't tell what it switches.
	DeviceType devicetype.Type
	Product
	// Error is a problem the driver reports with the device, such as a missing key or settings the
	// plugin can't use.
	Error string
}

// AppDevice is a device a driver instance sees in its app, declared or not: a Zigbee2MQTT
// instance's paired devices, or a plugin's discovered list.
type AppDevice struct {
	// ID is the driver's ID for the device: an IEEE address or a plugin's discovered ID.
	ID string
	Product
	Capabilities []string
	// DeviceType is what the device is, if the driver knows, as in Info.
	DeviceType devicetype.Type
	// Name and Room are as named in the app, empty if it has none.
	Name string
	Room string
}

// Update is a capability changing value. Drivers only report actual changes.
type Update struct {
	Device     ident.ID
	Capability string
	// Value is a bool, a float64 or, for a KindName capability such as mode, a string, validated
	// against the capability registry.
	Value any
	// Previous is nil the first time a value is seen.
	Previous any
	Time     time.Time
}

// Sink receives driver events. Calls arrive on the driver's receive goroutine and must not block.
type Sink interface {
	Discovered(dev ident.ID, info Info)
	// Changed reports what one report from a device changed, all of it in one call, so nothing
	// downstream acts on part of a report: a rule sees every value it carried at once.
	Changed(changes ...Update)
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

// writerKey is where a context says whose write a Set under it is.
type writerKey struct{}

// WithWriter returns ctx saying whose write a Set under it is, by its claim source: a rule's
// (Rule/<key>), or a Matter controller's or oikade set's. A Setter passes the write on whoever's it
// is, and the host's event log reads this to say who wrote.
func WithWriter(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, writerKey{}, source)
}

// Writer is whose write ctx says a Set under it is, as WithWriter was given it, and empty when
// nothing said.
func Writer(ctx context.Context) string {
	source, _ := ctx.Value(writerKey{}).(string)
	return source
}

// Refused is a write that arbitration turned away because a claim holds the capability: the rule
// working, not a fault. Callers recognise it with errors.AsType.
type Refused struct {
	// Holder is the ref of the claim that won, such as case/desk-plug/on/hall-too-humid, and Level
	// its level.
	Holder, Level string
}

func (e *Refused) Error() string { return e.Holder + " holds it at " + e.Level }
