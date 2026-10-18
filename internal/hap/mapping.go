/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package hap exposes Devices to HomeKit through a HAP bridge.
package hap

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
)

// lowBattery is the charge below which HomeKit shows a low battery warning.
const lowBattery = 20

// SetFunc sends a HomeKit write to the device.
type SetFunc func(name string, value any) error

// Device is everything needed to build an accessory.
type Device struct {
	// Key is the Device object; it doesn't affect the accessory.
	Key          types.NamespacedName
	AID          uint64
	Name         string
	Serial       string
	Manufacturer string
	Model        string
	Capabilities []string
	// Service overrides the default service for the device's main capability.
	Service homev1alpha1.HomeKitService
	Values  map[string]any
}

// Accessory is a built accessory and how to push capability changes into it.
type Accessory struct {
	A      *accessory.A
	update map[string]func(any)
}

// Update pushes a capability value to HomeKit. Capabilities that aren't mapped are ignored.
func (a *Accessory) Update(name string, v any) {
	if f := a.update[name]; f != nil {
		f(v)
	}
}

// Build maps a Device to an accessory. It returns nil when no capability maps to HomeKit.
// Services are always added in the same order so that instance IDs stay stable.
func Build(d Device, set SetFunc) *Accessory {
	has := func(c string) bool { return slices.Contains(d.Capabilities, c) }
	out := &Accessory{update: map[string]func(any){}}
	var services []*service.S
	typ := accessory.TypeSensor

	// The main actuator, if any.
	actuator := ""
	for _, c := range []string{capability.LightOn, capability.SwitchOn, capability.OutletOn} {
		if has(c) {
			actuator = c
			break
		}
	}
	if actuator != "" {
		kind := d.Service
		if kind != homev1alpha1.HomeKitServiceLightbulb && kind != homev1alpha1.HomeKitServiceSwitch &&
			kind != homev1alpha1.HomeKitServiceOutlet {
			kind = map[string]homev1alpha1.HomeKitService{
				capability.LightOn:  homev1alpha1.HomeKitServiceLightbulb,
				capability.SwitchOn: homev1alpha1.HomeKitServiceSwitch,
				capability.OutletOn: homev1alpha1.HomeKitServiceOutlet,
			}[actuator]
		}
		var on *characteristic.On
		switch kind {
		case homev1alpha1.HomeKitServiceLightbulb:
			s := service.NewLightbulb()
			on, typ = s.On, accessory.TypeLightbulb
			if actuator == capability.LightOn && has(capability.LightLevel) {
				b := characteristic.NewBrightness()
				s.AddC(b.C)
				bindInt(out, b.Int, capability.LightLevel, d.Values, set)
			}
			services = append(services, s.S)
		case homev1alpha1.HomeKitServiceOutlet:
			s := service.NewOutlet()
			// Zigbee plugs don't report whether something is plugged in.
			s.OutletInUse.SetValue(true)
			on, typ = s.On, accessory.TypeOutlet
			services = append(services, s.S)
		default:
			s := service.NewSwitch()
			on, typ = s.On, accessory.TypeSwitch
			services = append(services, s.S)
		}
		bindBool(out, on.Bool, actuator, d.Values, set)
	}

	if has(capability.Occupancy) {
		if d.Service == homev1alpha1.HomeKitServiceOccupancySensor {
			s := service.NewOccupancySensor()
			bindFlag(out, s.OccupancyDetected.Int, capability.Occupancy, d.Values, false)
			services = append(services, s.S)
		} else {
			s := service.NewMotionSensor()
			bindBool(out, s.MotionDetected.Bool, capability.Occupancy, d.Values, nil)
			services = append(services, s.S)
		}
	}
	if has(capability.ContactOpen) {
		s := service.NewContactSensor()
		// HomeKit's ContactSensorState is 1 when contact is not detected, i.e. open.
		bindFlag(out, s.ContactSensorState.Int, capability.ContactOpen, d.Values, false)
		services = append(services, s.S)
	}
	if has(capability.Temperature) {
		s := service.NewTemperatureSensor()
		s.CurrentTemperature.SetMinValue(-100)
		s.CurrentTemperature.SetMaxValue(100)
		bindFloat(out, s.CurrentTemperature.Float, capability.Temperature, d.Values)
		services = append(services, s.S)
	}
	if has(capability.Humidity) {
		s := service.NewHumiditySensor()
		bindFloat(out, s.CurrentRelativeHumidity.Float, capability.Humidity, d.Values)
		services = append(services, s.S)
	}
	if has(capability.Battery) {
		s := service.NewBatteryService()
		level, low := s.BatteryLevel.Int, s.StatusLowBattery.Int
		apply := func(v any) {
			pct := int(math.Round(capability.Float(v)))
			_ = level.SetValue(pct)
			_ = low.SetValue(boolInt(pct < lowBattery))
		}
		out.update[capability.Battery] = apply
		if v, ok := d.Values[capability.Battery]; ok {
			apply(v)
		}
		services = append(services, s.S)
	}

	if len(services) == 0 {
		return nil
	}
	a := accessory.New(accessory.Info{
		Name: d.Name, SerialNumber: d.Serial, Manufacturer: orDash(d.Manufacturer), Model: orDash(d.Model),
	}, typ)
	a.Id = d.AID
	for _, s := range services {
		a.AddS(s)
	}
	out.A = a
	return out
}

func bindBool(out *Accessory, c *characteristic.Bool, name string, values map[string]any, set SetFunc) {
	out.update[name] = func(v any) {
		if b, ok := v.(bool); ok {
			c.SetValue(b)
		}
	}
	if v, ok := values[name]; ok {
		out.update[name](v)
	}
	if set != nil {
		c.OnSetRemoteValue(func(v bool) error { return set(name, v) })
	}
}

func bindInt(out *Accessory, c *characteristic.Int, name string, values map[string]any, set SetFunc) {
	out.update[name] = func(v any) { _ = c.SetValue(int(math.Round(capability.Float(v)))) }
	if v, ok := values[name]; ok {
		out.update[name](v)
	}
	c.OnSetRemoteValue(func(v int) error { return set(name, float64(v)) })
}

func bindFloat(out *Accessory, c *characteristic.Float, name string, values map[string]any) {
	out.update[name] = func(v any) { c.SetValue(capability.Float(v)) }
	if v, ok := values[name]; ok {
		out.update[name](v)
	}
}

// bindFlag maps a bool capability to a 0/1 integer characteristic.
func bindFlag(out *Accessory, c *characteristic.Int, name string, values map[string]any, invert bool) {
	out.update[name] = func(v any) {
		if b, ok := v.(bool); ok {
			_ = c.SetValue(boolInt(b != invert))
		}
	}
	if v, ok := values[name]; ok {
		out.update[name](v)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Signature summarises what HomeKit sees of a device. A change means the bridge must be rebuilt.
func (d Device) Signature() string {
	return fmt.Sprintf("%d|%s|%s|%s|%s|%s", d.AID, d.Name, d.Manufacturer, d.Model, d.Service,
		strings.Join(d.Capabilities, ","))
}

// Pin converts a setup code (XXX-XX-XXX) to the digits HAP expects.
func Pin(code string) string {
	return strings.ReplaceAll(code, "-", "")
}
