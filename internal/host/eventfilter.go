package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// deviceNames are the names a device has had, each for the time it had it.
type deviceNames []namedFor

// namedFor is a name a device had from one deploy's rename until another's. A zero time is no
// limit: it was paired under the name, or has it still. Another device may have the name outside.
type namedFor struct {
	name        string
	from, until time.Time
}

func (n deviceNames) has(name string, at time.Time) bool {
	return slices.ContainsFunc(n, func(s namedFor) bool {
		return s.name == name && (s.from.IsZero() || !at.Before(s.from)) && (s.until.IsZero() || !at.After(s.until))
	})
}

// namesOf follows a device back through the log's renames, oldest first, to the names it had
// before, from the name it has now, or, for one that isn't onRecord, the name it last had.
func namesOf(name string, onRecord bool, renames []api.Event) deviceNames {
	var names deviceNames
	var until time.Time
	for _, r := range slices.Backward(renames) {
		before, ok := r.Previous.(string)
		switch {
		case !ok || !until.IsZero() && r.Time.After(until):
		case r.Device == name:
			// How it got the name: before this it had another.
			names = append(names, namedFor{name: name, from: r.Time, until: until})
			name, until = before, r.Time
		case before == name && until.IsZero() && !onRecord && len(names) == 0:
			// Nothing has the name now, so the device asked for is the one that last left it.
			until = r.Time
		case before == name:
			// Another device left the name then, so this one has had it only since.
			return append(names, namedFor{name: name, from: r.Time, until: until})
		}
	}
	return append(names, namedFor{name: name, until: until})
}

// renamed takes a rename that happens as a tail follows: a device the filter names carries on under
// its new name, and the name it left is its no longer.
func (m *eventMatcher) renamed(e api.Event) {
	before, ok := e.Previous.(string)
	if !ok || e.Type != api.EventRename {
		return
	}
	for i, d := range m.devices {
		for j, n := range d.names {
			if n.name == before && n.until.IsZero() && (n.from.IsZero() || !e.Time.Before(n.from)) {
				d.names[j].until = e.Time
				m.devices[i].names = append(d.names, namedFor{name: e.Device, from: e.Time})
				break
			}
		}
	}
}

// home is what a matcher needs of the home as it is now.
type home struct {
	rooms    func(room string) []ident.ID
	onRecord func(name string) bool
	// renames gives the log's rename events, oldest first. It's only asked if a ref names a device.
	renames func() ([]api.Event, error)
}

// errLogUnread is a matcher that couldn't be made because the log couldn't be read, which is the
// host's failure and not the filter's.
var errLogUnread = errors.New("reading the event log")

// newEventMatcher makes a filter's matcher, or says which ref can't narrow events.
func newEventMatcher(f api.EventFilter, h home) (*eventMatcher, error) {
	m := &eventMatcher{
		since: f.Since, until: f.Until, narrowed: len(f.Refs) > 0,
		rules: map[string]bool{}, writers: map[string]bool{}, refs: map[string]bool{},
	}
	var renamed []api.Event
	var asked bool
	names := func(device string) (deviceNames, error) {
		if !asked {
			var err error
			if renamed, err = h.renames(); err != nil {
				return nil, fmt.Errorf("%w: %w", errLogUnread, err)
			}
			asked = true
		}
		return namesOf(device, h.onRecord(device), renamed), nil
	}
	device := func(name, capability string) error {
		had, err := names(name)
		m.devices = append(m.devices, deviceMatch{names: had, capability: capability})
		return err
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
			for _, dev := range h.rooms(parts[0]) {
				if err = device(dev.Name, capabilityOf(parts)); err != nil {
					break
				}
			}
		case kind == ident.Flow:
			m.rule(parts[0])
		case kind == ident.Case:
			// A case is keyed by its device's name, so it had another key under each name before.
			var had deviceNames
			had, err = names(parts[0])
			for _, n := range had {
				m.rule(control.CaseKey(n.name, parts[1], parts[2]))
			}
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
	named := d.names.has(e.Device, e.Time)
	if before, ok := e.Previous.(string); ok && e.Type == api.EventRename {
		// The name it left is its too: a device asked for by that name is shown where it went.
		named = named || d.names.has(before, e.Time)
	}
	return named && (d.capability == "" || e.Capability == "" || e.Capability == d.capability)
}

// matcher makes the matcher of a call's filter, from the home as it is and the log's renames.
func (s *apiServer) matcher(ctx context.Context, f api.EventFilter) (*eventMatcher, error) {
	return newEventMatcher(f, home{
		rooms:    s.srv.devices.Room,
		onRecord: func(name string) bool { _, ok := s.srv.devices.Get(key(name)); return ok },
		renames:  func() ([]api.Event, error) { return s.srv.events.renames(ctx) },
	})
}

// filterStatus is the status a filter that couldn't be used is answered with: refused, unless what
// failed was the host reading its log.
func filterStatus(err error) int {
	if errors.Is(err, errLogUnread) {
		return http.StatusInternalServerError
	}
	return http.StatusUnprocessableEntity
}
