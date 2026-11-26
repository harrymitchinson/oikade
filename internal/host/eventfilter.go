package host

import (
	"fmt"
	"slices"
	"time"

	"oikade.dev/internal/control"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/registry"
)

// eventMatcher says which events an api.EventFilter lets through.
type eventMatcher struct {
	since, until time.Time
	// narrowed is whether the filter has refs. Without them every event is about what it names.
	narrowed bool
	devices  []deviceMatch
	// rules are the rules named, by the key their runs, pauses and calls have, and writers by the ref
	// their writes are by.
	rules, writers map[string]bool
	// refs are the refs events are matched by as they stand: a plugin's or a signal's.
	refs map[string]bool
	// matter is the bridge: its own events, and what controllers wrote or were refused.
	matter bool
}

// deviceMatch is a device named, by its key, or one capability of it. Events from before the log
// kept a device's key have it by name.
type deviceMatch struct {
	key, name string
	// capability, if set, leaves out the device's other capabilities. What happened to the device as a
	// whole stays: it went offline, or was forgotten or renamed.
	capability string
}

// home is what a matcher needs of the home as it is now.
type home struct {
	rooms func(room string) []ident.ID
	// device is the key of the device a person names, a live one or else the last one removed that
	// had the name, or false for none. A name that's no device's matches events kept under that name,
	// from before events kept a device's key.
	device func(name string) (ident.ID, bool)
	// rule is the key of a rule a person names by a key with a device's name in it.
	rule func(key string) string
	// names names the devices in a room, for their events from before the log kept keys.
	names control.Names
}

// newEventMatcher makes a filter's matcher, or says which ref can't narrow events. An event names a
// device by its key, its hardware, so a device's ref finds its past under every name it had, and
// under the name it has now from before events kept keys.
func newEventMatcher(f api.EventFilter, h home) (*eventMatcher, error) {
	m := &eventMatcher{
		since: f.Since, until: f.Until, narrowed: len(f.Refs) > 0,
		rules: map[string]bool{}, writers: map[string]bool{}, refs: map[string]bool{},
	}
	device := func(name, capability string) {
		dev, ok := h.device(name)
		if !ok {
			dev = ident.ID{Name: name}
		}
		m.devices = append(m.devices, deviceMatch{key: dev.Name, name: name, capability: capability})
	}
	for _, ref := range f.Refs {
		kind, parts, err := ident.ParseRef(ref)
		if err != nil {
			return nil, err
		}
		switch {
		case kind == ident.Device && len(parts) > 0:
			device(parts[0], capabilityOf(parts))
		case kind == ident.Room:
			for _, dev := range h.rooms(parts[0]) {
				m.devices = append(m.devices, deviceMatch{key: dev.Name, name: h.names.Device(dev.Name), capability: capabilityOf(parts)})
			}
		case kind == ident.Flow:
			m.rule(parts[0])
		case kind == ident.Case:
			named := control.CaseKey(parts[0], parts[1], parts[2])
			m.rule(h.rule(named))
			m.rule(named)
		case kind == ident.Plugin, kind == ident.Signal:
			m.refs[ident.NewRef(kind, parts...).String()] = true
		case kind == ident.Matter:
			m.matter = true
		default:
			return nil, fmt.Errorf("%s has no events: they're a device's, a capability's, a room's, a case's, a flow's, a "+
				"signal's or a plugin's, or %s's", ref, ident.Matter)
		}
	}
	return m, nil
}

// rule adds a rule to those the filter names, by its key.
func (m *eventMatcher) rule(key string) {
	m.rules[key], m.writers[control.RuleRef(key).String()] = true, true
}

// capabilityOf is the capability a device's or a room's ref ends with, if it has one.
func capabilityOf(parts []string) string {
	if len(parts) > 1 {
		return parts[1]
	}
	return ""
}

// pass says an event is within the filter's times and about what it names.
func (m *eventMatcher) pass(e api.Event) bool {
	if !m.since.IsZero() && e.Time.Before(m.since) || !m.until.IsZero() && e.Time.After(m.until) {
		return false
	}
	return m.about(e)
}

// about says an event is about what the filter names, whenever it happened. The home's own events
// always are: a deploy, or your automations coming or going, is often why the rest happened.
func (m *eventMatcher) about(e api.Event) bool {
	if !m.narrowed || e.Type == api.EventProcess || e.Type == api.EventDeploy {
		return true
	}
	if m.matter && (e.Type == api.EventMatter || e.Type == api.EventRefused || e.By == matterRef) {
		return true
	}
	if m.rules[e.Rule] || m.writers[e.By] || m.refs[e.Ref] {
		return true
	}
	return e.Device != "" && slices.ContainsFunc(m.devices, func(d deviceMatch) bool { return d.covers(e) })
}

// matterRef is what a controller's write is by.
var matterRef = ident.NewRef(ident.Matter).String()

func (d deviceMatch) covers(e api.Event) bool {
	return (e.Device == d.key || e.Device == d.name) && (d.capability == "" || e.Capability == "" || e.Capability == d.capability)
}

// matcher makes the matcher of a call's filter, from the home as it is.
func (s *apiServer) matcher(f api.EventFilter) (*eventMatcher, error) {
	r := s.registry()
	return newEventMatcher(f, home{
		rooms:  s.srv.devices.Room,
		device: func(name string) (ident.ID, bool) { return keyOfName(r, name) },
		rule:   func(key string) string { return ruleKeyOf(r, key) },
		names:  registryNames(r),
	})
}

// named writes events as a person reads them, with their devices named as the registry has them
// now, or a second ago.
type named struct {
	lineWriter
	registry *registry.Registry
	namer    *namer
	made     time.Time
}

func (n *named) write(v any) bool {
	if e, ok := v.(api.Event); ok {
		if now := time.Now(); n.namer == nil || now.Sub(n.made) > time.Second {
			n.namer, n.made = newNamer(n.registry), now
		}
		v = n.namer.event(e)
	}
	return n.lineWriter.write(v)
}
