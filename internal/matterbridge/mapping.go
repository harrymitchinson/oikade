// Package matterbridge exposes Devices on a Matter bridge: it maps their capabilities to Matter
// device types and clusters, keeps the clusters in step with the bus, and turns commands from
// Matter controllers into writes.
package matterbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
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
	Key          ident.ID
	Endpoint     uint16
	Name         string
	Capabilities []string
	// Type is what a device that's on or off is, as devicetype.OnOffOf says. A dimmable type has
	// Level Control, and an on/off one doesn't, even with a brightness. Empty is what its
	// capabilities say.
	Type devicetype.Type
	// Unique is the bridged device's UniqueID. Empty derives it from Key, which suits a Key that
	// never changes.
	Unique string
	// Absent is a device with no driver reporting it yet: it's on the bridge, and unreachable.
	Absent bool
	// Product is what the device is, which the bridged device's Bridged Device Basic Information
	// serves.
	Product driver.Product
	// After runs f, under the Node's lock, once d has passed, for a move that settles, unless the
	// function it returns is called first. Nil settles a move only by the device's report.
	After func(d time.Duration, f func()) (stop func())
	// Command, if set, hears each command a controller sends to a cluster that says what it was
	// asked, as a Window Covering does: the command's name, then key-value pairs. It runs under the
	// Node's lock, and must not block.
	Command func(command string, args ...any)
	// Send, if set, sends what a controller has written to the device without waiting for more, as
	// a stop must. It must not block.
	Send func()
	// Gathering, if set, says a controller's write to a capability hasn't been sent to the device
	// yet. It must not block.
	Gathering func(name string) bool
}

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
	// cancel ends what a controller's write started, for one that didn't happen.
	cancel map[string]func()
}

// Update pushes a capability value into the clusters. Call it inside Node.Update. Capabilities
// that aren't mapped are ignored.
func (m *Mapped) Update(name string, v any) {
	if f := m.update[name]; f != nil {
		f(v)
	}
}

// Cancel undoes what a controller's write to a capability started, for a write that didn't
// happen, such as a blind's move, and says whether there was anything to undo. Call it inside
// Node.Update.
func (m *Mapped) Cancel(name string) bool {
	f := m.cancel[name]
	if f != nil {
		f()
	}
	return f != nil
}

// mapped are the capabilities that make a device worth showing.
var mapped = append([]string{capability.On, capability.Setpoint, capability.Locked, capability.Position}, sensors...)

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
	m := &Mapped{update: map[string]func(any){}, cancel: map[string]func(){}}

	// A thermostat's on and temperature are its Thermostat cluster's, not an On/Off or a sensor. An
	// air conditioner's on is its On/Off cluster too.
	climate := devicetype.ClimateOf(d.Capabilities)
	thermostat := climate != ""
	switch climate {
	case devicetype.RoomAirConditioner:
		mapAirConditioner(m, set)
	case devicetype.Thermostat:
		mapHeater(m, has(capability.On), set)
	}

	if has(capability.On) && !thermostat {
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

	if has(capability.Locked) {
		l := clusters.NewLock()
		l.OnCommand = func(locked bool) { set(capability.Locked, locked) }
		m.Types = append(m.Types, clusters.DeviceDoorLock)
		m.Clusters = append(m.Clusters, l.Cluster())
		m.update[capability.Locked] = func(v any) {
			if b, ok := v.(bool); ok {
				l.Set(b)
			}
		}
	}
	if has(capability.Position) {
		mapCovering(m, d, set)
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
	if has(capability.Temperature) && !thermostat {
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

// mapCovering makes the device a Window Covering, whose lift is its position. Both are 0 when
// open, so Matter's hundredths are the position times 100. A move settles after position's Settle.
func mapCovering(m *Mapped, d Device, set SetFunc) {
	c := clusters.NewCovering()
	c.OnMove = func(percent float64) { set(capability.Position, percent) }
	c.OnStop = d.Send
	if d.Gathering != nil {
		c.Unsent = func() bool { return d.Gathering(capability.Position) }
	}
	if d.Command != nil {
		c.OnCommand = func(command string, target *float64) {
			if target == nil {
				d.Command(command, "target", "none")
				return
			}
			d.Command(command, "target", *target)
		}
	}
	if d.After != nil {
		settle := capability.SettleOf(capability.Position)
		c.AfterSettle = func(f func()) func() { return d.After(settle, f) }
	}
	m.Types = append(m.Types, clusters.DeviceWindowCovering)
	m.Clusters = append(m.Clusters, c.Cluster())
	m.update[capability.Position] = func(v any) { c.SetCurrent(capability.Float(v)) }
	m.cancel[capability.Position] = c.Cancel
}

// mapHeater makes the device a Thermostat with only the Heating feature. SystemMode Off and Heat
// are its on, if it has one. The Thermostat cluster's LocalTemperature is its temperature.
func mapHeater(m *Mapped, switchable bool, set SetFunc) {
	h := clusters.NewHeater(switchable)
	h.OnSetpoint = func(celsius float64) { set(capability.Setpoint, celsius) }
	h.OnMode = func(heat bool) { set(capability.On, heat) }
	m.Types = append(m.Types, clusters.DeviceThermostat)
	m.Clusters = append(m.Clusters, h.Cluster())
	m.update[capability.Setpoint] = func(v any) { h.SetSetpoint(capability.Float(v)) }
	m.update[capability.Temperature] = func(v any) { h.SetLocal(capability.Float(v)) }
	if switchable {
		m.update[capability.On] = func(v any) {
			if b, ok := v.(bool); ok {
				h.SetOn(b)
			}
		}
	}
}

// systemModes are the SystemMode each mode shows as, and modeNames the other way round.
var systemModes = map[string]uint64{
	contractv1.ModeAuto: clusters.SystemModeAuto,
	contractv1.ModeHeat: clusters.SystemModeHeat,
	contractv1.ModeCool: clusters.SystemModeCool,
	contractv1.ModeDry:  clusters.SystemModeDry,
	contractv1.ModeFan:  clusters.SystemModeFanOnly,
}

var modeNames = func() map[uint64]string {
	out := map[uint64]string{}
	for name, v := range systemModes {
		out[v] = name
	}
	return out
}()

// mapAirConditioner makes the device a Room Air Conditioner: a Thermostat with Heating, Cooling and
// Auto, whose SystemMode is its on and its mode, and an On/Off cluster for its on. Both occupied
// setpoints are its setpoint, and LocalTemperature is its temperature.
//
// SystemMode Cool is two writes, mode cool and on true, and the Coalescer sends the mode first, so
// the unit doesn't start in its old mode. Off is on false alone, so the unit keeps its mode.
func mapAirConditioner(m *Mapped, set SetFunc) {
	a := clusters.NewAirConditioner()
	onoff := clusters.NewOnOff()
	// Off is the unit's dead front: SystemMode Off, with its mode and setpoint kept.
	onoff.Cluster().FeatureMap |= clusters.FeatureDeadFront
	a.OnSetpoint = func(celsius float64) { set(capability.Setpoint, celsius) }
	a.OnSystemMode = func(on bool, mode uint64) {
		onoff.Set(on)
		if name, ok := modeNames[mode]; on && ok {
			set(capability.Mode, name)
		}
		set(capability.On, on)
	}
	onoff.OnChange = func(on bool) {
		a.SetOn(on)
		set(capability.On, on)
	}
	m.Types = append(m.Types, clusters.DeviceRoomAirConditioner)
	m.Clusters = append(m.Clusters, onoff.Cluster(), a.Cluster())
	m.update[capability.Setpoint] = func(v any) { a.SetSetpoint(capability.Float(v)) }
	m.update[capability.Temperature] = func(v any) { a.SetLocal(capability.Float(v)) }
	m.update[capability.On] = func(v any) {
		if b, ok := v.(bool); ok {
			onoff.Set(b)
			a.SetOn(b)
		}
	}
	m.update[capability.Mode] = func(v any) {
		if name, ok := v.(string); ok {
			if mode, ok := systemModes[name]; ok {
				a.SetMode(mode)
			}
		}
	}
}

// CheckBridgeName says what's wrong with a name for the bridge itself, its NodeLabel.
func CheckBridgeName(name string) error {
	switch {
	case len(name) > maxLabel:
		return fmt.Errorf("the Matter bridge's name %q is %d bytes, and it's at most %d", name, len(name), maxLabel)
	case !utf8.ValidString(name):
		return errors.New("the Matter bridge's name isn't valid UTF-8")
	}
	return nil
}

// maxLabel is the most a NodeLabel holds, in bytes.
const maxLabel = 32

// Label returns a Device's name cut to the 32 bytes a NodeLabel holds.
func Label(name string) string { return clusters.Fit(name, 32) }

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
