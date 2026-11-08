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

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
)

// SetFunc takes a write from a controller to a capability. It must not block: it runs under the
// Node's lock.
type SetFunc func(name string, value any)

// onOffTypes are the Matter device types of a device that's on or off.
var onOffTypes = map[devicetype.Type]im.DeviceType{
	devicetype.OnOffLight:                 clusters.DeviceOnOffLight,
	devicetype.DimmableLight:              clusters.DeviceDimmableLight,
	devicetype.OnOffPlugInUnit:            clusters.DeviceOnOffPlug,
	devicetype.DimmablePlugInUnit:         clusters.DeviceDimmablePlug,
	devicetype.MountedOnOffControl:        clusters.DeviceMountedOnOffControl,
	devicetype.MountedDimmableLoadControl: clusters.DeviceMountedDimmableLoad,
}

// Device is everything needed to map a device to a bridged endpoint.
type Device struct {
	Key      ident.ID
	Endpoint uint16
	Name     string
	// Capabilities are the device's capability names.
	Capabilities []string
	// Type is what a device that's on or off is, as devicetype.OnOffOf says. A dimmable type has
	// Level Control, and an on/off one doesn't, even with a brightness. Empty is what its
	// capabilities say.
	Type devicetype.Type
	// Unique is the bridged device's UniqueID. Empty derives it from Key, which suits a Key that never
	// changes.
	Unique string
	// Absent is a device with no driver reporting it yet: it's on the bridge, and unreachable.
	Absent bool
}

// uniqueID is the bridged device's UniqueID.
func (d Device) uniqueID() string {
	if d.Unique != "" {
		return d.Unique
	}
	return UniqueID(d.Key.String())
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

// mapped are the capabilities that make a device worth showing.
var mapped = append([]string{capability.On}, sensors...)

// sensors are the read-only capabilities the mapping shows.
var sensors = []string{capability.Occupied, capability.Open, capability.Temperature, capability.Humidity}

// Supported reports whether any of a Device's capabilities map to a Matter device type. Battery
// alone doesn't: it's a power source, not a device.
func Supported(caps []string) bool {
	for _, c := range mapped {
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

	if has(capability.On) {
		typ := devicetype.OnOffOf(d.Type, "", d.Capabilities)
		dimmable := devicetype.Dimmable(typ)
		m.Types = append(m.Types, onOffTypes[typ])

		var onoff *clusters.OnOff
		if dimmable || devicetype.Light(typ) {
			onoff = clusters.NewLightOnOff()
		} else {
			onoff = clusters.NewOnOff()
		}
		onoff.OnChange = func(on bool) { set(capability.On, on) }
		m.Clusters = append(m.Clusters, onoff.Cluster())
		m.update[capability.On] = func(v any) {
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
					set(capability.On, false)
					return
				}
				set(capability.Brightness, percent)
				if withOnOff {
					onoff.Set(true)
					set(capability.On, true)
				}
			}
			m.Clusters = append(m.Clusters, level.Cluster())
			m.update[capability.Brightness] = func(v any) { level.Set(capability.Float(v)) }
		}
	}

	if has(capability.Occupied) {
		c := clusters.NewOccupancy()
		m.Types = append(m.Types, clusters.DeviceOccupancySensor)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.Occupied] = func(v any) {
			if b, ok := v.(bool); ok {
				c.Set(b)
			}
		}
	}
	if has(capability.Open) {
		c := clusters.NewBooleanState()
		m.Types = append(m.Types, clusters.DeviceContactSensor)
		m.Clusters = append(m.Clusters, c.Cluster())
		// Boolean State is true for contact: closed, the inverse of open.
		m.update[capability.Open] = func(v any) {
			if b, ok := v.(bool); ok {
				c.Set(!b)
			}
		}
	}
	if has(capability.Temperature) {
		c := clusters.NewTemperature()
		m.Types = append(m.Types, clusters.DeviceTemperatureSensor)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.Temperature] = func(v any) { c.Set(capability.Float(v)) }
	}
	if has(capability.Humidity) {
		c := clusters.NewHumidity()
		m.Types = append(m.Types, clusters.DeviceHumiditySensor)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.Humidity] = func(v any) { c.Set(capability.Float(v)) }
	}
	if has(capability.Battery) {
		c := clusters.NewBattery(d.Endpoint)
		m.Types = append(m.Types, clusters.DevicePowerSource)
		m.Clusters = append(m.Clusters, c.Cluster())
		m.update[capability.Battery] = func(v any) { c.Set(capability.Float(v)) }
	}
	return m
}

// Label returns a Device's name cut to the 32 bytes a NodeLabel holds.
func Label(name string) string {
	for len(name) > 32 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// UniqueID is a bridged device's stable UniqueID: 32 hex digits from what names it.
func UniqueID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return strings.ToUpper(hex.EncodeToString(sum[:16]))
}

// Signature summarises the shape controllers see of a Device. A change means its endpoint is
// rebuilt. The name isn't part of it: a new name is set in place, as the NodeLabel.
func (d Device) Signature() string {
	return fmt.Sprintf("%d|%s|%s", d.Endpoint, d.Type, strings.Join(slices.Sorted(slices.Values(d.Capabilities)), ","))
}
