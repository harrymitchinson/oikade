package zigbee2mqtt

import (
	"reflect"
	"slices"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
)

// bridgeDevice is an entry of <base>/bridge/devices, with only the fields the plugin reads.
type bridgeDevice struct {
	IEEEAddress  string      `json:"ieee_address"`
	FriendlyName string      `json:"friendly_name"`
	Type         string      `json:"type"`
	Definition   *definition `json:"definition"`
	// SoftwareBuildID is the device's firmware, as its Basic cluster says, nil if it didn't say. It's
	// whatever the device sent, so that one that isn't a string costs only itself, not the list.
	SoftwareBuildID any `json:"software_build_id"`
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

// Zigbee2MQTT expose types, and the feature that switches a light or a switch.
const (
	exposeBinary  = "binary"
	exposeNumeric = "numeric"
	exposeLight   = "light"
	exposeSwitch  = "switch"
	exposeCover   = "cover"
	exposeLock    = "lock"
	featureState  = "state"
)

// binding maps one Zigbee2MQTT property to a canonical capability.
type binding struct {
	capability string
	property   string
	// valueOn and valueOff are a binary property's raw values for true and false.
	valueOn, valueOff any
	// invert flips a boolean (Zigbee2MQTT's contact is true when closed).
	invert bool
	// scale is the raw maximum that maps to 100% (brightness).
	scale float64
	// reverse is a percentage the other way round: Oikade's is 100 minus Zigbee2MQTT's (position,
	// which Zigbee2MQTT has at 100 when open).
	reverse bool
	// states read an enum property as a bool (a lock's lock_state). A /set or /get names setProperty
	// instead, with valueOn or valueOff, since Zigbee2MQTT has no converter for lock_state.
	states      map[string]bool
	setProperty string
	// get is whether the property can be read with a /get.
	get bool
}

// lockStates are a lock's lock_state: only a bolt that's fully thrown is locked. Any other value,
// such as a firmware's own or an empty one, isn't, so a jammed lock never reads locked.
var lockStates = map[string]bool{"locked": true}

// setter is the binding as a /set or /get names it: setProperty, if it has one, with valueOn or
// valueOff.
func (b binding) setter() binding {
	if b.setProperty != "" {
		b.property, b.states, b.setProperty = b.setProperty, nil, ""
	}
	return b
}

// bindings derives capability bindings from a device's exposes. Features of an endpoint, such as a
// multi-gang switch's state_l2, are skipped.
func bindings(exposes []expose) []binding {
	var out []binding
	for _, e := range exposes {
		if e.Endpoint != "" {
			continue
		}
		switch e.Type {
		case exposeLight:
			for _, f := range e.Features {
				switch {
				case f.Endpoint != "":
				case f.Name == featureState && f.Type == exposeBinary:
					out = append(out, binding{capability: capability.On, property: f.Property, valueOn: f.ValueOn, valueOff: f.ValueOff, get: f.gettable()})
				case f.Name == "brightness" && f.Type == exposeNumeric:
					max := 254.0
					if f.ValueMax != nil && *f.ValueMax > 0 {
						max = *f.ValueMax
					}
					out = append(out, binding{capability: capability.Brightness, property: f.Property, scale: max, get: f.gettable()})
				}
			}
		case exposeSwitch:
			for _, f := range e.Features {
				if f.Endpoint == "" && f.Name == featureState && f.Type == exposeBinary {
					out = append(out, binding{capability: capability.On, property: f.Property, valueOn: f.ValueOn, valueOff: f.ValueOff, get: f.gettable()})
				}
			}
		case exposeCover:
			// A cover with no position only opens and closes, which position can't say.
			for _, f := range e.Features {
				if f.Endpoint == "" && f.Name == "position" && f.Type == exposeNumeric {
					out = append(out, binding{capability: capability.Position, property: f.Property, reverse: true, get: f.gettable()})
				}
			}
		case exposeLock:
			out = append(out, lockBindings(e.Features)...)
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

// lockBindings binds locked to a lock's state, which a /set writes LOCK or UNLOCK to. It's read from
// lock_state where the lock has one, since state says what it was told, and lock_state where the
// bolt is.
func lockBindings(features []expose) []binding {
	var state, lockState *expose
	for i, f := range features {
		switch {
		case f.Endpoint != "":
		case f.Name == featureState && f.Type == exposeBinary:
			state = &features[i]
		case f.Name == "lock_state":
			lockState = &features[i]
		}
	}
	if state == nil {
		return nil
	}
	b := binding{capability: capability.Locked, property: state.Property, valueOn: state.ValueOn, valueOff: state.ValueOff, get: state.gettable()}
	if lockState != nil {
		// A /get of state reads the lock, which reports lock_state with it.
		b.property, b.states, b.setProperty = lockState.Property, lockStates, state.Property
	}
	return []binding{b}
}

// deviceType is what a device is, from the expose it's switched by: the first at its root with a
// state, as bindings takes. A light is an on/off or a dimmable light, by whether it has a
// brightness. A switch says nothing, because a relay can't tell what it switches.
func deviceType(exposes []expose) devicetype.Type {
	for _, e := range exposes {
		if e.Endpoint != "" || (e.Type != exposeLight && e.Type != exposeSwitch) {
			continue
		}
		if !slices.ContainsFunc(e.Features, func(f expose) bool {
			return f.Endpoint == "" && f.Name == featureState && f.Type == exposeBinary
		}) {
			continue
		}
		if e.Type == exposeSwitch {
			return ""
		}
		if slices.ContainsFunc(e.Features, func(f expose) bool {
			return f.Endpoint == "" && f.Name == "brightness" && f.Type == exposeNumeric
		}) {
			return devicetype.DimmableLight
		}
		return devicetype.OnOffLight
	}
	return ""
}

func (e expose) gettable() bool { return e.Access&accessGet != 0 }

var numeric = map[string]string{
	"temperature": capability.Temperature,
	"humidity":    capability.Humidity,
	"battery":     capability.Battery,
	"linkquality": capability.LinkQuality,
}

// decode converts a raw property value to a canonical one, validated against the registry.
func (b binding) decode(raw any) (any, error) {
	var v any
	switch {
	case b.states != nil:
		s, ok := raw.(string)
		if !ok {
			return capability.Validate(b.capability, raw)
		}
		v = b.states[s]
	case b.reverse:
		f, ok := raw.(float64)
		if !ok {
			return capability.Validate(b.capability, raw)
		}
		v = 100 - f
	case b.scale > 0:
		f, ok := raw.(float64)
		if !ok {
			return capability.Validate(b.capability, raw)
		}
		v = roundTo(f*100/b.scale, 1)
	case b.valueOn != nil:
		switch {
		case same(raw, b.valueOn):
			v = true
		case same(raw, b.valueOff):
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

// same says whether two values decoded from JSON are the same scalar. An object or an array is
// never the same as anything: comparing two of them with == panics, and a device's definition and
// its payloads both come from the network.
func same(a, b any) bool {
	return reflect.ValueOf(a).Comparable() && reflect.ValueOf(b).Comparable() && a == b
}

// encode converts a canonical value to the raw property value for a /set payload.
func (b binding) encode(v any) any {
	switch {
	case b.reverse:
		return int(roundTo(100-capability.Float(v), 0))
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
