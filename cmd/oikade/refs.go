package main

import (
	"math"
	"strings"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// nameOf is the name an argument gives a thing of kind k: its ref, such as device/desk-plug, or,
// as a shorthand, its bare name. No name has a '/', so one with a '/' is a ref.
func nameOf(k ident.Kind, arg string) (string, error) {
	if !strings.Contains(arg, "/") {
		return arg, nil
	}
	got, parts, err := ident.ParseRef(arg)
	if err != nil && strings.HasPrefix(arg, string(k)+"/") {
		return "", cli.UsageErrorf("%s", err) // Of the right kind, so what's wrong is its name.
	}
	if err != nil || got != k || len(parts) != 1 {
		return "", cli.UsageErrorf("%s isn't a %s's ref: give one as %s/NAME, or its name alone", arg, k, k)
	}
	return parts[0], nil
}

// namesOf is nameOf for each argument.
func namesOf(k ident.Kind, args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		name, err := nameOf(k, arg)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

// capabilityOf is the device and capability an argument names by its ref, such as
// device/desk-plug/on.
func capabilityOf(arg string) (device, name string, err error) {
	k, parts, err := ident.ParseRef(arg)
	if err != nil && strings.HasPrefix(arg, string(ident.Device)+"/") && strings.Count(arg, "/") == 2 {
		return "", "", cli.UsageErrorf("%s", err)
	}
	if err != nil || k != ident.Device || len(parts) != 2 {
		return "", "", cli.UsageErrorf("%s isn't a capability's ref: give one as device/DEVICE/CAPABILITY, "+
			"such as device/desk-plug/on", arg)
	}
	return parts[0], parts[1], nil
}

// ruleOf checks that an argument with a '/' is a rule's ref, a case's or a flow's, as command takes
// one. A bare word goes to the host, which can say whose rule's name it is.
func ruleOf(command, arg string) error {
	if !strings.Contains(arg, "/") {
		return nil
	}
	k, _, err := ident.ParseRef(arg)
	if err != nil && (strings.HasPrefix(arg, string(ident.Case)+"/") || strings.HasPrefix(arg, string(ident.Flow)+"/")) {
		return cli.UsageErrorf("%s", err)
	}
	if err != nil || (k != ident.Case && k != ident.Flow) {
		return cli.UsageErrorf("%s isn't a rule's ref: oikade %s takes a case's, case/DEVICE/CAPABILITY/CASE, "+
			"or a flow's, flow/FLOW", arg, command)
	}
	return nil
}

// valueOf is the value raw, as it was typed, gives the capability, or why it can't take it, in the
// user's words, before anything is sent: true, false, on or off for a bool, a number in range, or one
// of a capability's names.
func valueOf(device, name, raw string) (any, error) {
	ref := ident.CapabilityRef(device, name)
	spec, ok := capability.Lookup(name)
	switch {
	case !ok:
		return parseValue(raw), nil // A newer host's, perhaps, which says what it takes.
	case !spec.Writable:
		return nil, cli.Errorf("%s is read-only: %s reports it, and nothing sets it", ref, device)
	}
	value := parseValue(raw)
	switch spec.Kind {
	case capability.KindBool:
		if _, ok := value.(bool); !ok {
			return nil, cli.Errorf("%s takes true or false, not %s", ref, raw)
		}
	case capability.KindName:
		if _, err := capability.Validate(name, raw); err != nil {
			return nil, cli.Errorf("%s takes %s, not %s", ref, orList(spec.Names), raw)
		}
		return raw, nil
	default:
		f, ok := value.(float64)
		switch {
		case !ok:
			return nil, cli.Errorf("%s takes a number, such as %g, not %s", ref, math.Round((spec.Min+spec.Max)/2), raw)
		case math.IsNaN(f) || f < spec.Min || f > spec.Max:
			return nil, cli.Errorf("%s takes %g%s to %g%s, not %s", ref, spec.Min, spec.Unit, spec.Max, spec.Unit, raw)
		}
	}
	return value, nil
}

// orList is "a, b or c".
func orList(names []string) string {
	switch len(names) {
	case 0:
		return "a name"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
