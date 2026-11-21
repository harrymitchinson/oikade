package overkiz

import (
	"fmt"
	"math"
	"slices"
	"strings"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// stateKey names one Overkiz state of a device: on the device itself (sub "") or one of its
// sub-devices, such as "7" for #7.
type stateKey struct{ sub, name string }

// states are every state a device and its sub-devices have reported, as the API gave them.
type states map[stateKey]any

func (s states) number(sub, name string) (float64, bool) {
	f, ok := s[stateKey{sub, name}].(float64)
	return f, ok
}

func (s states) text(sub, name string) string {
	t, _ := s[stateKey{sub, name}].(string)
	return t
}

// model is what Oikade serves of one kind of Overkiz device, its widget.
type model struct {
	capabilities []string
	// values reads the capabilities from the device's states, leaving out any it can't.
	values func(s states) contractv1.State
	// write is the commands that set a capability to v, and the sub-device that takes them. No
	// commands means there's nothing to do. It refuses a value it can't send.
	write func(capability string, v any, s states) (sub string, commands []apiCommand, err error)
}

// models is every kind of device the plugin serves, by widget. A widget that isn't here isn't
// served, and is logged once by name.
//
// RTS blinds (UpDownRollerShutter and the like) aren't here on purpose: they report nothing back,
// and the contract can't say a position is an estimate.
var models = map[string]model{
	// Somfy io roller shutters and screens report how far closed they are, 0 open to 100 closed, as
	// Oikade's position does.
	"PositionableRollerShutter":                       closure,
	"PositionableRollerShutterWithLowSpeedManagement": closure,
	"PositionableScreen":                              closure,
	// Atlantic, Thermor and Sauter towel radiators, over Cozytouch.
	"AtlanticElectricalTowelDryer": towelDryer,
}

// closure is a cover with core:ClosureState, set with setClosure.
var closure = model{
	capabilities: []string{contractv1.Position},
	values: func(s states) contractv1.State {
		out := contractv1.State{}
		if f, ok := s.number("", "core:ClosureState"); ok && f >= 0 && f <= 100 {
			out[contractv1.Position] = f
		}
		return out
	},
	write: func(capability string, v any, _ states) (string, []apiCommand, error) {
		f, err := within(capability, v, 0, 100)
		if err != nil {
			return "", nil, err
		}
		return "", []apiCommand{{Name: "setClosure", Parameters: []any{int(math.Round(f))}}}, nil
	},
}

// A towel radiator's operating modes, in core:OperatingModeState and setTowelDryerOperatingMode.
const (
	modeStandby = "standby"
	// modeExternal is manual: it holds core:TargetTemperatureState.
	modeExternal = "external"
	// modeInternal is its own schedule, and modeAuto its learning mode. Both aim for
	// io:EffectiveTemperatureSetpointState, and a setpoint written in them is an override.
	modeInternal = "internal"
	modeAuto     = "auto"
)

// towelSensor is the sub-device with the room temperature, core:TemperatureState: #7, as Home
// Assistant's mapping of AtlanticElectricalTowelDryer reads it.
const towelSensor = "7"

// towelDryer is an Atlantic towel radiator, from Home Assistant's mapping of the widget. Standby is
// off and manual is heat. Its schedule and auto modes, and any it doesn't know, count as on, and are
// left as they are: a setpoint written in them is the override they take, and turning it on leaves
// them be. Boost and drying aren't served, and a radiator in one reports the setpoint it's aiming for.
var towelDryer = model{
	capabilities: []string{contractv1.On, contractv1.Setpoint, contractv1.Temperature},
	values: func(s states) contractv1.State {
		out := contractv1.State{}
		mode := s.text("", "core:OperatingModeState")
		if mode != "" {
			// Only standby is off: a mode it doesn't know is still heating.
			out[contractv1.On] = mode != modeStandby
		}
		// Manual holds its target. Any other mode aims for its effective setpoint. Either falls back on
		// the other until the one it reads has been reported, rather than keeping a value from
		// before the mode changed.
		read := []string{"io:EffectiveTemperatureSetpointState", "core:TargetTemperatureState"}
		if mode == modeExternal || mode == modeStandby {
			slices.Reverse(read)
		}
		for _, name := range read {
			if f, ok := s.number("", name); ok && f >= 0 && f <= 50 {
				out[contractv1.Setpoint] = f
				break
			}
		}
		if f, ok := s.number(towelSensor, "core:TemperatureState"); ok && f > -273.15 && f < 1000 {
			out[contractv1.Temperature] = f
		}
		return out
	},
	write: func(capability string, v any, s states) (string, []apiCommand, error) {
		mode := s.text("", "core:OperatingModeState")
		switch capability {
		case contractv1.On:
			on, ok := v.(bool)
			switch {
			case !ok:
				return "", nil, fmt.Errorf("on %v is not a bool", v)
			case !on:
				return "", []apiCommand{{Name: "setTowelDryerOperatingMode", Parameters: []any{modeStandby}}}, nil
			case mode != modeStandby && mode != "":
				return "", nil, nil
			}
			return "", []apiCommand{{Name: "setTowelDryerOperatingMode", Parameters: []any{modeExternal}}}, nil
		case contractv1.Setpoint:
			f, err := within(capability, v, 0, 50)
			if err != nil {
				return "", nil, err
			}
			command := "setDerogatedTargetTemperature"
			if mode == modeExternal || mode == modeStandby || mode == "" {
				command = "setTargetTemperature"
			}
			return "", []apiCommand{{Name: command, Parameters: []any{f}}}, nil
		}
		return "", nil, fmt.Errorf("%s can't be set", capability)
	},
}

// within reads v as a number from lowest to highest.
func within(capability string, v any, lowest, highest float64) (float64, error) {
	f, ok := v.(float64)
	if !ok || f < lowest || f > highest {
		return 0, fmt.Errorf("%s %v is not a number from %v to %v", capability, v, lowest, highest)
	}
	return f, nil
}

// deviceURL is a parsed Overkiz device URL, <protocol>://<gateway>/<address>[#<sub>].
type deviceURL struct {
	protocol, gateway, address, sub string
}

// parseURL parses a device URL. ok is false for one that isn't in that form.
func parseURL(s string) (deviceURL, bool) {
	protocol, rest, ok := strings.Cut(s, "://")
	if !ok || protocol == "" {
		return deviceURL{}, false
	}
	rest, sub, _ := strings.Cut(rest, "#")
	gateway, address, ok := strings.Cut(rest, "/")
	if !ok || gateway == "" || address == "" {
		return deviceURL{}, false
	}
	// The device itself is sometimes #1, with its sensors at #2 and on.
	if sub == "1" {
		sub = ""
	}
	return deviceURL{protocol: protocol, gateway: gateway, address: address, sub: sub}, true
}

// base is the URL of the device the sub-device belongs to: the URL without its #sub.
func (u deviceURL) base() string { return u.protocol + "://" + u.gateway + "/" + u.address }

// id is the device's Oikade ID, <protocol>.<address>, with every character the contract doesn't allow
// as '-'. It leaves out the gateway, so a replaced hub keeps its devices. ok is false if it still
// isn't a valid ID, as when it's too long.
func (u deviceURL) id() (string, bool) {
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		}
		return '-'
	}, u.protocol+"."+u.address)
	return id, contractv1.ValidID(id)
}
