package zigbee2mqtt

import (
	"slices"

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
	Access   int      `json:"access"`
	Features []expose `json:"features"`
}

// accessGet is the bit of an expose's access that says the property can be read with a /get
// (https://www.zigbee2mqtt.io/guide/usage/exposes.html#access). Zigbee2MQTT sets access on the leaf
// features, not on a light or a switch. A sleeping sensor's properties have it clear, since it
// can't answer.
const accessGet = 0b100

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
	// get is whether the property can be read with a /get.
	get bool
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
					out = append(out, binding{capability: capability.On, property: f.Property, valueOn: f.ValueOn, valueOff: f.ValueOff, get: f.gettable()})
				case f.Name == "brightness" && f.Type == exposeNumeric:
					max := 254.0
					if f.ValueMax != nil && *f.ValueMax > 0 {
						max = *f.ValueMax
					}
					out = append(out, binding{capability: capability.Brightness, property: f.Property, scale: max, get: f.gettable()})
				}
			}
		case "switch":
			for _, f := range e.Features {
				if f.Endpoint == "" && f.Name == "state" && f.Type == exposeBinary {
					out = append(out, binding{capability: capability.On, property: f.Property, valueOn: f.ValueOn, valueOff: f.ValueOff, get: f.gettable()})
				}
			}
		case exposeNumeric:
			if c, ok := numeric[e.Name]; ok {
				out = append(out, binding{capability: c, property: e.Property, get: e.gettable()})
			}
		case exposeBinary:
			switch e.Name {
			case "contact":
				out = append(out, binding{capability: capability.Open, property: e.Property, invert: true, get: e.gettable()})
			case "occupancy":
				out = append(out, binding{capability: capability.Occupied, property: e.Property, get: e.gettable()})
			}
		}
	}
	// A device with a light and a switch at its root has two properties for on. The first is the
	// one it's switched by and reports, so the other can't flip the value back and forth.
	seen := map[string]bool{}
	return slices.DeleteFunc(out, func(b binding) bool {
		dup := seen[b.capability]
		seen[b.capability] = true
		return dup
	})
}

func (e expose) gettable() bool { return e.Access&accessGet != 0 }

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
