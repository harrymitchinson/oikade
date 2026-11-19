package host

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
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
	// refs are the refs events are matched by as they stand: a plugin's.
	refs map[string]bool
	// matter is the bridge: its own events, and what controllers wrote or were refused.
	matter bool
}

// deviceMatch is a device named, or one capability of it.
type deviceMatch struct {
	names deviceNames
	// capability, if set, leaves out the device's other capabilities. What happened to the device as a
	// whole stays: it went offline, or was forgotten or renamed.
	capability string
}

// deviceNames are the names a device has had: the one it has now, with no time, and each one before,
// to when a deploy renamed it. Another device may have that name since.
type deviceNames map[string]time.Time

func (n deviceNames) has(name string, at time.Time) bool {
	until, ok := n[name]
	return ok && (until.IsZero() || !at.After(until))
}

// namesOf follows a device back through renames, oldest first, to the names it had before.
func namesOf(name string, renames []api.Event) deviceNames {
	names := deviceNames{name: {}}
	for _, r := range slices.Backward(renames) {
		before, ok := r.Previous.(string)
		if !ok || !names.has(r.Device, r.Time) {
			continue
		}
		if _, known := names[before]; !known {
			names[before] = r.Time
		}
	}
	return names
}

// newEventMatcher makes a filter's matcher, or says which ref can't narrow events. rooms gives the devices in
// a room now, and renames the log's rename events, which is only asked for if a ref names a device.
func newEventMatcher(f api.EventFilter, rooms func(string) []ident.ID, renames func() ([]api.Event, error)) (*eventMatcher, error) {
	m := &eventMatcher{
		since: f.Since, until: f.Until, narrowed: len(f.Refs) > 0,
		rules: map[string]bool{}, writers: map[string]bool{}, refs: map[string]bool{},
	}
	var renamed []api.Event
	var asked bool
	device := func(name, capability string) error {
		if !asked {
			var err error
			if renamed, err = renames(); err != nil {
				return err
			}
			asked = true
		}
		m.devices = append(m.devices, deviceMatch{names: namesOf(name, renamed), capability: capability})
		return nil
	}
	for _, ref := range f.Refs {
		kind, parts, err := ident.ParseRef(ref)
		if err != nil {
			return nil, err
		}
		switch {
		case kind == ident.Device && len(parts) > 0:
			err = device(parts[0], capabilityOf(parts))
		case kind == ident.Room:
			for _, dev := range rooms(parts[0]) {
				if err = device(dev.Name, capabilityOf(parts)); err != nil {
					break
				}
			}
		case kind == ident.Case || kind == ident.Flow:
			key, _ := control.RuleKey(ref)
			m.rules[key], m.writers[control.RuleRef(key).String()] = true, true
		case kind == ident.Plugin:
			m.refs[ident.NewRef(kind, parts...).String()] = true
		case kind == ident.Matter:
			m.matter = true
		default:
			err = fmt.Errorf("%s has no events: they're a device's, a capability's, a room's, a case's, a flow's or a "+
				"plugin's, or %s's", ref, ident.Matter)
		}
		if err != nil {
			return nil, err
		}
	}
	return m, nil
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
	named := d.names.has(e.Device, e.Time)
	if before, ok := e.Previous.(string); ok && e.Type == api.EventRename {
		// The name it left is its too: a device asked for by that name is shown where it went.
		named = named || d.names.has(before, e.Time)
	}
	return named && (d.capability == "" || e.Capability == "" || e.Capability == d.capability)
}

// matcher makes the matcher of a call's filter, from the rooms as they are and the log's renames.
func (s *apiServer) matcher(ctx context.Context, f api.EventFilter) (*eventMatcher, error) {
	return newEventMatcher(f, s.srv.devices.Room, func() ([]api.Event, error) { return s.srv.events.renames(ctx) })
}
