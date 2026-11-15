package overkiz

import (
	"math"
	"strings"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// model is what Oikade serves of one kind of Overkiz device, its widget: a binding per capability.
type model struct {
	bindings []binding
}

// binding is one capability: the Overkiz state it's read from, on the device or one of its
// sub-devices, and the commands that write it.
type binding struct {
	capability string
	// sub is the sub-device the state is on, such as "2" for #2, or "" for the device itself.
	sub   string
	state string
	// read turns the state's value into the capability's, or says it can't.
	read func(any) (any, bool)
	// write is the commands that set the capability to v, nil if it's read-only.
	write func(v any) []apiCommand
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
}

// closure is a cover with core:ClosureState, set with setClosure.
var closure = model{bindings: []binding{{
	capability: contractv1.Position, state: "core:ClosureState", read: percent,
	write: func(v any) []apiCommand {
		f, _ := v.(float64)
		return []apiCommand{{Name: "setClosure", Parameters: []any{int(math.Round(f))}}}
	},
}}}

// percent reads a number from 0 to 100.
func percent(v any) (any, bool) {
	f, ok := v.(float64)
	if !ok || f < 0 || f > 100 {
		return nil, false
	}
	return f, true
}

// capabilities lists a model's capabilities, in order.
func (m model) capabilities() []string {
	out := make([]string, 0, len(m.bindings))
	for _, b := range m.bindings {
		out = append(out, b.capability)
	}
	return out
}

// binding returns the binding for a capability.
func (m model) binding(capability string) (binding, bool) {
	for _, b := range m.bindings {
		if b.capability == capability {
			return b, true
		}
	}
	return binding{}, false
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
// as '-'. It leaves out the gateway, so a replaced hub keeps its devices. ok is false if it's too
// long to be an ID.
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
