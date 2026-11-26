package host

import (
	"net/url"
	"strings"

	"oikade.dev/internal/control"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/registry"
)

// The host keeps everything of a device's by its key, its hardware, so a rename moves nothing. What
// a person reads names it: these look a name up where it's shown, and a key up where a name is read.

// devKey is how the hub and the engine know a device on record.
func devKey(d registry.Device) ident.ID { return ident.ID{Name: d.Key()} }

// registryNames names devices as the registry does, a removed one by the name it last had.
func registryNames(r *registry.Registry) control.Names {
	if r == nil {
		return nil
	}
	return r.NameOf
}

// keyOfName is the key of the device a person names: a live one, or else the last one removed that
// had the name, for its past.
func keyOfName(r *registry.Registry, name string) (ident.ID, bool) {
	if r == nil {
		return ident.ID{}, false
	}
	if d, ok := r.Get(name); ok {
		return devKey(d), true
	}
	var found registry.Device
	for _, d := range r.List().Devices {
		if d.Removed && d.Name == name {
			found = d
		}
	}
	return devKey(found), found.Name != ""
}

// ruleKeyOf is the key of a rule a person names by a key with a device's name in it: a case's
// "<device> <capability> <case>". A name no device has is a group's or a room's, as it is.
func ruleKeyOf(r *registry.Registry, key string) string {
	parts := strings.Split(key, " ")
	if r == nil || len(parts) != 3 {
		return key
	}
	if dev, ok := keyOfName(r, parts[0]); ok {
		return control.CaseKey(dev.Name, parts[1], parts[2])
	}
	return key
}

// namedRef is a ref with the key in it named: a device's, a capability's or a case's. Any other
// ref, or one whose key has no name, is as it is.
func namedRef(ref string, names control.Names) string {
	kind, rest, ok := strings.Cut(ref, "/")
	if names == nil || !ok || kind != string(ident.Device) && kind != string(ident.Case) {
		return ref
	}
	first, tail, more := strings.Cut(rest, "/")
	key, err := url.PathUnescape(first)
	if err != nil {
		return ref
	}
	name, ok := names(key)
	if !ok {
		return ref
	}
	out := ident.NewRef(ident.Kind(kind), name).String()
	if more {
		out += "/" + tail
	}
	return out
}

// namedEvent is an event as a person reads it: its device, rule and refs named. A rename says the
// name it gave.
func namedEvent(e api.Event, r *registry.Registry) api.Event {
	if r == nil {
		return e
	}
	names := registryNames(r)
	e.Device, e.Rule = names.Device(e.Device), names.Rule(e.Rule)
	e.Ref, e.By = namedRef(e.Ref, names), namedRef(e.By, names)
	e.Result, e.Message = namedText(e.Result, r), namedText(e.Message, r)
	if e.Type == api.EventRename && e.Message != "" {
		e.Device, e.Ref = e.Message, ident.DeviceRef(e.Message).String()
	}
	return e
}

// namedText is text your process wrote, such as why a case is inactive, with the devices it names
// by key named: a ref as it is, or as a why-not spells it, unescaped.
func namedText(text string, r *registry.Registry) string {
	if r == nil || !strings.ContainsAny(text, "/") {
		return text
	}
	var pairs []string
	for _, d := range r.List().Devices {
		k := d.Key()
		if d.Removed || k == d.Name {
			continue
		}
		escaped := ident.DeviceRef(k).String()[len(ident.Device)+1:]
		for _, kind := range []ident.Kind{ident.Device, ident.Case} {
			prefix := string(kind) + "/"
			pairs = append(pairs, prefix+escaped+"/", prefix+d.Name+"/", prefix+k+"/", prefix+d.Name+"/")
		}
	}
	if len(pairs) == 0 {
		return text
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// declaredNames names devices as decls declare them, and the rest as names does, for what a deploy
// says before its declarations are applied. Hardware whose name decls give to other hardware, as a
// swap does, is its key.
func declaredNames(decls []wire.DeviceDeclaration, names control.Names) control.Names {
	declared, taken := map[string]string{}, map[string]bool{}
	for _, d := range wire.Declarations(decls) {
		declared[registry.KeyOf(d.Driver, d.ID, d.Name)], taken[d.Name] = d.Name, true
	}
	return func(key string) (string, bool) {
		if name, ok := declared[key]; ok {
			return name, true
		}
		if names == nil {
			return "", false
		}
		name, ok := names(key)
		return name, ok && !taken[name]
	}
}
