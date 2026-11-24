package main

import (
	"math"
	"strings"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/cli"
	"oikade.dev/internal/ident"
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

// named is a thing an argument names: its ref, and the kind and parts the ref has.
type named struct {
	ref   string
	kind  ident.Kind
	parts []string
}

// refsOf is what a command's arguments name, each by its ref, as every output prints it, so that
// output pastes back in. A bare word is a device's name, as a shorthand, but for matter, which is
// always the Matter bridge: a device called matter is device/matter. An argument that's neither is
// a command typed wrong.
func refsOf(args []string) ([]named, error) {
	out := make([]named, 0, len(args))
	for _, arg := range args {
		ref := arg
		if !strings.Contains(arg, "/") && arg != string(ident.Matter) {
			if !ident.ValidName(arg) {
				return nil, cli.UsageErrorf("%q isn't a device's name, or a ref such as device/desk-plug or flow/morning", arg)
			}
			ref = ident.DeviceRef(arg).String()
		}
		if strings.HasPrefix(ref, "secret/") {
			return nil, cli.UsageErrorf("%s isn't a ref: a secret is one of your values now", arg).WithHint("%s", valuesHint)
		}
		kind, parts, err := ident.ParseRef(ref)
		if err != nil {
			return nil, cli.UsageErrorf("%s", err)
		}
		out = append(out, named{ref: ident.NewRef(kind, parts...).String(), kind: kind, parts: parts})
	}
	return out, nil
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
	return cli.UsageErrorf("%s isn't a rule's ref: oikade %s takes a case's, case/DEVICE/CAPABILITY/CASE, "+
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
