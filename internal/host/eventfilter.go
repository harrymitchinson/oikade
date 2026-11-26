package host

import (
	"fmt"
	"slices"
	"time"

	"oikade.dev/internal/control"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/ident"
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

// deviceMatch is a device named, by its key, or one capability of it.
type deviceMatch struct {
	key string
	// capability, if set, leaves out the device's other capabilities. What happened to the device as a
	// whole stays: it went offline, or was forgotten or renamed.
	capability string
}

// newEventMatcher makes a filter's matcher, or says which ref can't narrow events. An event names a
// device by its key, its hardware, so a device's ref finds its whole past. rooms are the devices in
// a room now.
func newEventMatcher(f api.EventFilter, rooms func(room string) []ident.ID) (*eventMatcher, error) {
	m := &eventMatcher{
		since: f.Since, until: f.Until, narrowed: len(f.Refs) > 0,
		rules: map[string]bool{}, writers: map[string]bool{}, refs: map[string]bool{},
	}
	for _, ref := range f.Refs {
		kind, parts, err := ident.ParseRef(ref)
		if err != nil {
			return nil, err
		}
		key, rest, isDevice := ident.DeviceOf(kind, parts)
		switch {
		case isDevice && kind != ident.Case:
			m.devices = append(m.devices, deviceMatch{key: key, capability: capabilityOf(rest)})
		case kind == ident.Room:
			for _, dev := range rooms(parts[0]) {
				m.devices = append(m.devices, deviceMatch{key: dev.Name, capability: capabilityOf(parts[1:])})
			}
		case kind == ident.Flow:
			m.rule(parts[0])
		case kind == ident.Case:
			m.rule(control.CaseKey(key, rest[0], rest[1]))
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

// capabilityOf is the capability that what's after a device or a room in its ref names, if any.
func capabilityOf(rest []string) string {
	if len(rest) > 0 {
		return rest[0]
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
	return e.Device == d.key && (d.capability == "" || e.Capability == "" || e.Capability == d.capability)
}

// matcher makes the matcher of a call's filter, from the home as it is.
func (s *apiServer) matcher(f api.EventFilter) (*eventMatcher, error) {
	return newEventMatcher(f, s.srv.devices.Room)
}
