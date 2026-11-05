// Package matterbridge exposes Devices on a Matter bridge: it maps their capabilities to Matter
// device types and clusters, keeps the clusters in step with the bus, and turns commands from
// Matter controllers into writes.
package matterbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
)

// SetFunc takes a write from a controller to a capability. It must not block: it runs under the
// Node's lock.
type SetFunc func(name string, value any)

// Device is everything needed to map a Device to a bridged endpoint.
type Device struct {
	Key      types.NamespacedName
	Endpoint uint16
	Name     string
	// Capabilities are the Device's capability names, from its status.
	Capabilities []string
	// Type overrides the device type family of an on-off Device.
	Type homev1alpha1.MatterDeviceType
}

// Mapped is a Device as a bridged endpoint: its device types and clusters, and how bus state
// reaches them.
type Mapped struct {
	Types    []im.DeviceType
	Clusters []*im.Cluster
	update   map[string]func(any)
}

// Update pushes a capability value into the clusters. Call it inside Node.Update. Capabilities
// that aren't mapped are ignored.
func (m *Mapped) Update(name string, v any) {
	if f := m.update[name]; f != nil {
		f(v)
	}
}

// sensors are the read-only capabilities the mapping shows.
var sensors = []string{capability.Occupancy, capability.Contact, capability.Temperature, capability.RelativeHumidity}

// Supported reports whether any of a Device's capabilities map to a Matter device type. Battery
// alone doesn't: it's a power source, not a device.
func Supported(caps []string) bool {
	for _, c := range append([]string{capability.OnOff}, sensors...) {
		if slices.Contains(caps, c) {
			return true
		}
	}
	return false
}

// Map maps a Device. It returns nil when nothing about the Device maps to Matter. Device types
// and clusters are always added in the same order.
func Map(d Device, set SetFunc) *Mapped {
	if !Supported(d.Capabilities) {
		return nil
	}
	has := func(c string) bool { return slices.Contains(d.Capabilities, c) }
	m := &Mapped{update: map[string]func(any){}}

	if has(capability.OnOff) {
		dimmable := has(capability.CurrentLevel)
		family := d.Type
		if family == "" {
			family = homev1alpha1.MatterDeviceTypePlugInUnit
			if dimmable {
				family = homev1alpha1.MatterDeviceTypeLight
			}
		}
		m.Types = append(m.Types, deviceType(family, dimmable))

		var onoff *clusters.OnOff
		if dimmable || family == homev1alpha1.MatterDeviceTypeLight {
			onoff = clusters.NewLightOnOff()
		} else {
			onoff = clusters.NewOnOff()
		}
		onoff.OnChange = func(on bool) { set(capability.OnOff, on) }
		m.Clusters = append(m.Clusters, onoff.Cluster())
		m.update[capability.OnOff] = func(v any) {
			if b, ok := v.(bool); ok {
				onoff.Set(b)
			}
		}
		if dimmable {
			level := clusters.NewLevel()
			level.OnChange = func(percent float64, withOnOff, atMinimum bool) {
				// The WithOnOff commands turn the device off at the minimum level, and on above it.
				if withOnOff && atMinimum {
					onoff.Set(false)
					set(capability.OnOff, false)
					return
				}
				set(capability.CurrentLevel, percent)
				if withOnOff {
					onoff.Set(true)
					set(capability.OnOff, true)
				}
			}
			m.Clusters = append(m.Clusters, level.Cluster())
			m.update[capability.CurrentLevel] = func(v any) { level.Set(capability.Float(v)) }
		}
	}

	if has(capability.Occupancy) {
		c := clusters.NewOccupancy()
		m.Types = append(m.Types, clusters.DeviceOccupancySensor)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.Occupancy] = func(v any) {
			if b, ok := v.(bool); ok {
				c.Set(b)
			}
		}
	}
	if has(capability.Contact) {
		c := clusters.NewBooleanState()
		m.Types = append(m.Types, clusters.DeviceContactSensor)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.Contact] = func(v any) {
			if b, ok := v.(bool); ok {
				c.Set(b)
			}
		}
	}
	if has(capability.Temperature) {
		c := clusters.NewTemperature()
		m.Types = append(m.Types, clusters.DeviceTemperatureSensor)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.Temperature] = func(v any) { c.Set(capability.Float(v)) }
	}
	if has(capability.RelativeHumidity) {
		c := clusters.NewHumidity()
		m.Types = append(m.Types, clusters.DeviceHumiditySensor)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.RelativeHumidity] = func(v any) { c.Set(capability.Float(v)) }
	}
	if has(capability.BatPercentRemaining) {
		c := clusters.NewBattery(d.Endpoint)
		m.Types = append(m.Types, clusters.DevicePowerSource)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.BatPercentRemaining] = func(v any) { c.Set(capability.Float(v)) }
	}
	return m
}

func deviceType(family homev1alpha1.MatterDeviceType, dimmable bool) im.DeviceType {
	switch {
	case family == homev1alpha1.MatterDeviceTypeLight && dimmable:
		return clusters.DeviceDimmableLight
	case family == homev1alpha1.MatterDeviceTypeLight:
		return clusters.DeviceOnOffLight
	case family == homev1alpha1.MatterDeviceTypeMountedControl && dimmable:
		return clusters.DeviceMountedDimmableLoad
	case family == homev1alpha1.MatterDeviceTypeMountedControl:
		return clusters.DeviceMountedOnOffControl
	case dimmable:
		return clusters.DeviceDimmablePlug
	default:
		return clusters.DeviceOnOffPlug
	}
}

// Label returns a Device's name cut to the 32 bytes a NodeLabel holds.
func Label(name string) string {
	for len(name) > 32 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// UniqueID is a bridged device's stable UniqueID: 32 hex digits from its namespace and name.
func UniqueID(key types.NamespacedName) string {
	sum := sha256.Sum256([]byte(key.String()))
	return strings.ToUpper(hex.EncodeToString(sum[:16]))
}

// Signature summarises the shape controllers see of a Device. A change means its endpoint is
// rebuilt. The name isn't part of it: a new name is set in place, as the NodeLabel.
func (d Device) Signature() string {
	return fmt.Sprintf("%d|%s|%s", d.Endpoint, d.Type, strings.Join(slices.Sorted(slices.Values(d.Capabilities)), ","))
}
