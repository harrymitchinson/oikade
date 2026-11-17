package main

import (
	"fmt"
	"strings"

	"github.com/harrymitchinson/oikade/internal/ident"
)

// nameOf is the name an argument gives a thing of kind k: its ref, such as device/desk-plug, or,
// as a shorthand, its bare name. No name has a '/', so one with a '/' is a ref.
func nameOf(k ident.Kind, arg string) (string, error) {
	if !strings.Contains(arg, "/") {
		return arg, nil
	}
	got, parts, err := ident.ParseRef(arg)
	if err != nil {
		return "", err
	}
	if got != k || len(parts) != 1 {
		return "", fmt.Errorf("%s isn't a %s: give its name, or its ref, such as %s/NAME", arg, k, k)
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
func capabilityOf(arg string) (device, capability string, err error) {
	k, parts, err := ident.ParseRef(arg)
	if err != nil {
		return "", "", err
	}
	if k != ident.Device || len(parts) != 2 {
		return "", "", fmt.Errorf("%s isn't a capability: give one as device/DEVICE/CAPABILITY", arg)
	}
	return parts[0], parts[1], nil
}
