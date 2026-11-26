package control

import (
	"fmt"
	"strings"

	"oikade.dev/internal/ident"
)

// The claim sources that aren't rules, as the engine keeps them.
const (
	SourceMatter  = "Matter"
	SourceDevice  = "Device"
	SourceCommand = "Command"
	// SourceRule is the prefix of a rule's claims' source, before its key.
	SourceRule = "Rule/"
)

// RuleRef is a rule's ref, from its key: case/<plugin>/<id>/<capability>/<case> for a case's
// ("<device> <capability> <case>"), and flow/<name> for a flow's.
func RuleRef(key string) ident.Ref {
	if parts := strings.Split(key, " "); len(parts) == 3 {
		return ident.CaseRef(parts[0], parts[1], parts[2])
	}
	return ident.NewRef(ident.Flow, key)
}

// RuleKey is the key of the rule ref names, a case's or a flow's, as the host keeps it.
func RuleKey(ref string) (string, error) {
	k, parts, err := ident.ParseRef(ref)
	switch {
	case err == nil && k == ident.Case:
		device, rest, _ := ident.DeviceOf(k, parts)
		return CaseKey(device, rest[0], rest[1]), nil
	case err == nil && k == ident.Flow:
		return parts[0], nil
	case err == nil:
		err = fmt.Errorf("%s isn't a rule", ref)
	}
	return "", fmt.Errorf("%w: a rule is a case, as case/PLUGIN/ID/CAPABILITY/CASE, or a flow, as flow/FLOW", err)
}

// SameRule reports whether ref is the ref of the rule whose key is key.
func SameRule(ref, key string) bool {
	got, err := RuleKey(ref)
	return err == nil && got == key
}

// SourceRef is a claim's source as its ref: a rule's, or matter, device or command.
func SourceRef(source string) ident.Ref {
	if key, ok := strings.CutPrefix(source, SourceRule); ok {
		return RuleRef(key)
	}
	switch source {
	case SourceMatter:
		return ident.NewRef(ident.Matter)
	case SourceDevice:
		return ident.NewRef(ident.Device)
	case SourceCommand:
		return ident.NewRef(ident.Command)
	}
	return ident.Ref(source)
}
