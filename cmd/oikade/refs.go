package main

import (
	"math"
	"strings"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/cli"
	"oikade.dev/internal/ident"
)

// pluginName is the plugin an argument names: its ref, such as plugin/hue, or, as a shorthand, its
// bare name. No name has a '/', so one with a '/' is a ref.
func pluginName(arg string) (string, error) {
	if !strings.Contains(arg, "/") {
		return arg, nil
	}
	k := ident.Plugin
	got, parts, err := ident.ParseRef(arg)
	if err != nil && strings.HasPrefix(arg, string(k)+"/") {
		return "", cli.UsageErrorf("%s", err) // Of the right kind, so what's wrong is its name.
	}
	if err != nil || got != k || len(parts) != 1 {
		return "", cli.UsageErrorf("%s isn't a %s's ref: give one as %s/NAME, or its name alone", arg, k, k)
	}
	return parts[0], nil
}

// named is a thing an argument names: its ref, and the kind and parts the ref has. A device's or a
// capability's is of kind device, and a case's, with the device's key, its hardware, as their first
// part.
type named struct {
	ref   string
	kind  ident.Kind
	parts []string
}

// refsOf is what a command's arguments name, each by its ref, as every output prints it, so that
// output pastes back in: a device by its hardware's, as hardware/hue/0x2. A bare word is matter,
// the Matter bridge, and any other is a command typed wrong.
func refsOf(args []string) ([]named, error) {
	out := make([]named, 0, len(args))
	for _, arg := range args {
		if !strings.Contains(arg, "/") && arg != string(ident.Matter) {
			return nil, cli.UsageErrorf("%q isn't a ref: a device's is its hardware's, such as hardware/hue/0x2, as "+
				"oikade list devices shows it", arg)
		}
		if strings.HasPrefix(arg, "secret/") {
			return nil, cli.UsageErrorf("%s isn't a ref: a secret is one of your values now", arg).WithHint("%s", valuesHint)
		}
		n, err := parseNamed(arg)
		if err != nil {
			return nil, cli.UsageErrorf("%s", err)
		}
		out = append(out, n)
	}
	return out, nil
}

// parseNamed reads a ref, with what it names of a device by the device's key.
func parseNamed(arg string) (named, error) {
	kind, parts, err := ident.ParseRef(arg)
	if err != nil {
		return named{}, err
	}
	key, rest, ok := ident.DeviceOf(kind, parts)
	switch {
	case ok && kind == ident.Case:
		return named{ref: ident.CaseRef(key, rest[0], rest[1]).String(), kind: kind, parts: append([]string{key}, rest...)},
			nil
	case ok && len(rest) == 0:
		return named{ref: ident.DeviceRef(key).String(), kind: ident.Device, parts: []string{key}}, nil
	case ok:
		return named{ref: ident.CapabilityRef(key, rest[0]).String(), kind: ident.Device, parts: []string{key, rest[0]}}, nil
	}
	return named{ref: ident.NewRef(kind, parts...).String(), kind: kind, parts: parts}, nil
}

// eventRefs is the refs history and tail take: what the log has events of. A kind with none is
// refused, with the kinds that have.
func eventRefs(command string, args []string) ([]named, error) {
	refs, err := refsOf(args)
	if err != nil {
		return nil, err
	}
	for _, n := range refs {
		switch {
		case n.kind == ident.Device && len(n.parts) > 0, n.kind == ident.Room, n.kind == ident.Case, n.kind == ident.Flow,
			n.kind == ident.Signal, n.kind == ident.Plugin, n.kind == ident.Matter:
		default:
			return nil, cli.UsageErrorf("%s has no events", n.ref).
				WithHint("oikade %s takes a device, a capability, a room, a case, a flow, a signal, a plugin or matter",
					command)
		}
	}
	return refs, nil
}

// capabilityOf is the key of the device and the capability an argument names by its ref, such as
// hardware/hue/0x2/on.
func capabilityOf(arg string) (device, name string, err error) {
	n, err := parseNamed(arg)
	if err != nil && strings.HasPrefix(arg, string(ident.Hardware)+"/") && strings.Count(arg, "/") == 3 {
		return "", "", cli.UsageErrorf("%s", err)
	}
	if err != nil || n.kind != ident.Device || len(n.parts) != 2 {
		return "", "", cli.UsageErrorf("%s isn't a capability's ref: give one as hardware/PLUGIN/ID/CAPABILITY, "+
			"such as hardware/hue/0x2/on", arg)
	}
	return n.parts[0], n.parts[1], nil
}

// ruleOf checks that an argument is a rule's ref, a case's or a flow's, as command takes one, and not
// another kind's, such as matter. A bare word goes to the host, which can say whose rule's name it is.
func ruleOf(command, arg string) error {
	k, _, err := ident.ParseRef(arg)
	switch {
	case err == nil && (k == ident.Case || k == ident.Flow):
		return nil
	case err != nil && !strings.Contains(arg, "/"):
		// A rule's name on its own: the host knows if a rule has it, and answers with its ref.
		return nil
	case err != nil && (strings.HasPrefix(arg, string(ident.Case)+"/") || strings.HasPrefix(arg, string(ident.Flow)+"/")):
		return cli.UsageErrorf("%s", err)
	}
	return cli.UsageErrorf("%s isn't a rule's ref: oikade %s takes a case's, case/PLUGIN/ID/CAPABILITY/CASE, "+
		"or a flow's, flow/FLOW", arg, command)
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
		return nil, cli.Errorf("%s is read-only: %s reports it, and nothing sets it", ref, ident.DeviceRef(device))
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
