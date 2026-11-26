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
// "<device> <capability> <case>". A name no live device has is a group's or a room's, as it is, even
// if a device forgotten had it.
func ruleKeyOf(r *registry.Registry, key string) string {
	parts := strings.Split(key, " ")
	if r == nil || len(parts) != 3 {
		return key
	}
	if d, ok := r.Get(parts[0]); ok {
		return control.CaseKey(d.Key(), parts[1], parts[2])
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
func namedEvent(e api.Event, r *registry.Registry) api.Event { return newNamer(r).event(e) }

// namedText is text your process wrote, such as why a case is inactive, with the devices it names
// by key named.
func namedText(text string, r *registry.Registry) string { return newNamer(r).text(text) }

// namer names devices as the registry had them when it was made, for many events at a time.
type namer struct {
	// names are the devices' names by key, a removed one's the last it had.
	names map[string]string
	// refs names the live devices' keys in text: in a ref as it is, or as a why-not spells it,
	// unescaped. Nil if every key is its name.
	refs *strings.Replacer
}

// newNamer is the registry's namer, or nil, which names nothing, without one.
func newNamer(r *registry.Registry) *namer {
	if r == nil {
		return nil
	}
	snap := r.List()
	n := &namer{names: make(map[string]string, len(snap.Devices))}
	live := map[string]bool{}
	var pairs []string
	for _, d := range snap.Devices {
		k := d.Key()
		if !live[k] {
			n.names[k], live[k] = d.Name, !d.Removed
		}
		if d.Removed || k == d.Name {
			continue
		}
		escaped := ident.DeviceRef(k).String()[len(ident.Device)+1:]
		for _, kind := range []ident.Kind{ident.Device, ident.Case} {
			prefix := string(kind) + "/"
			pairs = append(pairs, prefix+escaped+"/", prefix+d.Name+"/", prefix+k+"/", prefix+d.Name+"/")
		}
	}
	if len(pairs) > 0 {
		n.refs = strings.NewReplacer(pairs...)
	}
	return n
}

// Names are the namer's names, or nil from a nil namer.
func (n *namer) Names() control.Names {
	if n == nil {
		return nil
	}
	return func(key string) (string, bool) {
		name, ok := n.names[key]
		return name, ok
	}
}

func (n *namer) event(e api.Event) api.Event {
	if n == nil {
		return e
	}
	names := n.Names()
	e.Device, e.Rule = names.Device(e.Device), names.Rule(e.Rule)
	e.Ref, e.By = namedRef(e.Ref, names), namedRef(e.By, names)
	e.Result, e.Message = n.text(e.Result), n.text(e.Message)
	if e.Type == api.EventRename && e.Message != "" {
		e.Device, e.Ref = e.Message, ident.DeviceRef(e.Message).String()
	}
	return e
}

func (n *namer) text(text string) string {
	if n == nil || n.refs == nil || !strings.Contains(text, "/") {
		return text
	}
	return n.refs.Replace(text)
}

// declaredNames names devices as decls declare them, and the rest as names does, for what a deploy
// says before its declarations are applied. Hardware whose name decls give to other hardware, as a
// swap does, is its key.
func declaredNames(decls []wire.DeviceDeclaration, names control.Names) control.Names {
	declared, taken := map[string]string{}, map[string]bool{}
	for _, d := range wire.Declarations(decls) {
		if d.Name != "" {
			declared[d.Key()], taken[d.Name] = d.Name, true
		}
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
