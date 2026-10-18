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

package zigbee2mqtt

import (
	"github.com/harrymitchinson/oikade/internal/capability"
)

// Exposes, as published in <base>/bridge/devices. Only the fields Oikade uses.
type bridgeDevice struct {
	IEEEAddress  string      `json:"ieee_address"`
	FriendlyName string      `json:"friendly_name"`
	Type         string      `json:"type"`
	Definition   *definition `json:"definition"`
}

type definition struct {
	Vendor  string   `json:"vendor"`
	Model   string   `json:"model"`
	Exposes []expose `json:"exposes"`
}

type expose struct {
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Property string   `json:"property"`
	Endpoint string   `json:"endpoint"`
	ValueOn  any      `json:"value_on"`
	ValueOff any      `json:"value_off"`
	ValueMax *float64 `json:"value_max"`
	Features []expose `json:"features"`
}

// Zigbee2MQTT expose types.
const (
	exposeBinary  = "binary"
	exposeNumeric = "numeric"
)

// binding maps one Zigbee2MQTT property to a canonical capability.
type binding struct {
	capability string
	property   string
	// For binary properties: the raw values meaning on and off.
	valueOn, valueOff any
	// invert flips a boolean (Zigbee2MQTT's contact is true when closed).
	invert bool
	// scale is the raw maximum that maps to 100% (brightness).
	scale float64
}

// bindings derives capability bindings from a device's exposes. Endpoint-specific features
// (e.g. state_l2 on a multi-gang switch) aren't supported yet and are skipped.
func bindings(exposes []expose) []binding {
	var out []binding
	for _, e := range exposes {
		if e.Endpoint != "" {
			continue
		}
		switch e.Type {
		case "light":
			for _, f := range e.Features {
				switch {
				case f.Endpoint != "":
				case f.Name == "state" && f.Type == exposeBinary:
					out = append(out, binding{capability: capability.LightOn, property: f.Property, valueOn: f.ValueOn, valueOff: f.ValueOff})
				case f.Name == "brightness" && f.Type == exposeNumeric:
					max := 254.0
					if f.ValueMax != nil && *f.ValueMax > 0 {
						max = *f.ValueMax
					}
					out = append(out, binding{capability: capability.LightLevel, property: f.Property, scale: max})
				}
			}
		case "switch":
			for _, f := range e.Features {
				if f.Endpoint == "" && f.Name == "state" && f.Type == exposeBinary {
					out = append(out, binding{capability: capability.SwitchOn, property: f.Property, valueOn: f.ValueOn, valueOff: f.ValueOff})
				}
			}
		case exposeNumeric:
			if c, ok := numeric[e.Name]; ok {
				out = append(out, binding{capability: c, property: e.Property})
			}
		case exposeBinary:
			switch e.Name {
			case "contact":
				out = append(out, binding{capability: capability.ContactOpen, property: e.Property, invert: true})
			case "occupancy":
				out = append(out, binding{capability: capability.Occupancy, property: e.Property})
			}
		}
	}
	return out
}

var numeric = map[string]string{
	"temperature": capability.Temperature,
	"humidity":    capability.Humidity,
	"battery":     capability.Battery,
	"linkquality": capability.LinkQuality,
}

// decode converts a raw property value to a canonical value.
func (b binding) decode(raw any) (any, error) {
	var v any
	switch {
	case b.scale > 0:
		f, ok := raw.(float64)
		if !ok {
			return capability.Validate(b.capability, raw)
		}
		v = roundTo(f*100/b.scale, 1)
	case b.valueOn != nil:
		switch raw {
		case b.valueOn:
			v = true
		case b.valueOff:
			v = false
		default:
			v = raw
		}
	case b.invert:
		bv, ok := raw.(bool)
		if !ok {
			return capability.Validate(b.capability, raw)
		}
		v = !bv
	default:
		v = raw
	}
	return capability.Validate(b.capability, v)
}

// encode converts a canonical value to the raw property value for a /set payload.
func (b binding) encode(v any) any {
	switch {
	case b.scale > 0:
		return int(roundTo(capability.Float(v)*b.scale/100, 0))
	case b.valueOn != nil:
		if v == true {
			return b.valueOn
		}
		return b.valueOff
	case b.invert:
		return v != true
	}
	return v
}

func roundTo(f float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	if f < 0 {
		return float64(int64(f*p-0.5)) / p
	}
	return float64(int64(f*p+0.5)) / p
}
