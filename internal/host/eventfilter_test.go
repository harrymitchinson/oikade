package host

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// aLog is one of each thing a log holds, each named by a word a test asks for it by.
func aLog(at time.Time) (events []api.Event, names []string) {
	add := func(name string, e api.Event) {
		e.Time = at.Add(time.Duration(len(events)) * time.Minute)
		if e.Ref == "" {
			e.Ref = eventRef(e)
		}
		events, names = append(events, e), append(names, name)
	}
	add("connected", api.Event{Type: api.EventProcess, Message: "connected"})
	add("relay on", api.Event{Type: api.EventChange, Device: "relay-3", Capability: "on", Value: true})
	add("renamed", api.Event{Type: api.EventRename, Device: "hall-lamp", Previous: "relay-3"})
	add("lamp on", api.Event{Type: api.EventChange, Device: "hall-lamp", Capability: "on", Value: true, Previous: false})
	add("lamp dimmed", api.Event{Type: api.EventChange, Device: "hall-lamp", Capability: "brightness", Value: 40.0})
	add("lamp offline", api.Event{Type: api.EventAvailability, Device: "hall-lamp", Message: api.AvailabilityOffline})
	add("dusk wrote", api.Event{Type: api.EventWrite, Device: "porch-light", Capability: "on", Value: true, By: "case/porch-light/on/dusk"})
	add("porch on", api.Event{Type: api.EventChange, Device: "porch-light", Capability: "on", Value: true})
	add("morning ran", api.Event{Type: api.EventRule, Rule: "morning", Result: "succeeded"})
	add("morning called", api.Event{Type: api.EventCall, Rule: "morning", Message: "forecast", Value: true})
	add("morning wrote", api.Event{Type: api.EventWrite, Device: "office-ac", Capability: "mode", Value: "heat", By: "flow/morning"})
	add("morning paused", api.Event{Type: api.EventPause, Rule: "morning", Message: "paused"})
	add("dusk ran", api.Event{Type: api.EventRule, Rule: "porch-light on dusk", Result: "succeeded"})
	add("tap", api.Event{Type: api.EventWrite, Device: "office-ac", Capability: "setpoint", Value: 21.0, By: "matter"})
	add("tap refused", api.Event{Type: api.EventRefused, Device: "valve", Capability: "on", Value: true, Message: "held"})
	add("paired", api.Event{Type: api.EventMatter, Message: api.MatterControllerPaired, Value: 1.0})
	add("set", api.Event{Type: api.EventWrite, Device: "office-ac", Capability: "on", Value: false, By: "command"})
	add("ac off", api.Event{Type: api.EventChange, Device: "office-ac", Capability: "on", Value: false})
	add("login refused", api.Event{Type: api.EventPlugin, Ref: "plugin/overkiz", Message: "login refused"})
	add("deployed", api.Event{Type: api.EventDeploy, Message: "f113018"})
	add("valve forgotten", api.Event{Type: api.EventForget, Device: "valve"})
	return events, names
}

// rooms is a home whose hall has the lamp, and whose office has the AC.
func rooms(room string) []ident.ID {
	return map[string][]ident.ID{"hall": {{Name: "hall-lamp"}}, "office": {{Name: "office-ac"}}}[room]
}

// Each kind of ref lets through its own events and no others, with the home's own: a deploy, and
// your automations connecting.
func TestEachKindOfRefHasItsEventsAndTheHomes(t *testing.T) {
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	log, names := aLog(at)
	var renames []api.Event
	for _, e := range log {
		if e.Type == api.EventRename {
			renames = append(renames, e)
		}
	}
	// home is what a ref lets through, with the home's own events around it, as the log has them.
	home := func(names ...string) []string {
		return slices.Concat([]string{"connected"}, names, []string{"deployed"})
	}
	for _, c := range []struct {
		name string
		f    api.EventFilter
		want []string
	}{
		{"nothing", api.EventFilter{}, names},
		{"a device, back through its rename", api.EventFilter{Refs: []string{"device/hall-lamp"}},
			home("relay on", "renamed", "lamp on", "lamp dimmed", "lamp offline")},
		{"a device by a name it left", api.EventFilter{Refs: []string{"device/relay-3"}}, home("relay on", "renamed")},
		{"a capability, and what happened to its device", api.EventFilter{Refs: []string{"device/hall-lamp/brightness"}},
			home("renamed", "lamp dimmed", "lamp offline")},
		{"a room", api.EventFilter{Refs: []string{"room/office"}}, home("morning wrote", "tap", "set", "ac off")},
		{"a room's capability", api.EventFilter{Refs: []string{"room/office/on"}}, home("set", "ac off")},
		{"a room that isn't", api.EventFilter{Refs: []string{"room/attic"}}, home()},
		{"a flow", api.EventFilter{Refs: []string{"flow/morning"}},
			home("morning ran", "morning called", "morning wrote", "morning paused")},
		{"a case", api.EventFilter{Refs: []string{"case/porch-light/on/dusk"}}, home("dusk wrote", "dusk ran")},
		{"matter", api.EventFilter{Refs: []string{"matter"}}, home("tap", "tap refused", "paired")},
		{"a plugin", api.EventFilter{Refs: []string{"plugin/overkiz"}}, home("login refused")},
		{"a device no longer on record", api.EventFilter{Refs: []string{"device/valve"}},
			[]string{"connected", "tap refused", "deployed", "valve forgotten"}},
		{"two refs", api.EventFilter{Refs: []string{"plugin/overkiz", "device/porch-light"}},
			home("dusk wrote", "porch on", "login refused")},
		{"a time", api.EventFilter{Since: at.Add(2 * time.Minute), Until: at.Add(4 * time.Minute)},
			[]string{"renamed", "lamp on", "lamp dimmed"}},
		{"a ref and a time", api.EventFilter{Refs: []string{"device/hall-lamp"}, Since: at.Add(3 * time.Minute)},
			[]string{"lamp on", "lamp dimmed", "lamp offline", "deployed"}},
	} {
		m, err := newEventMatcher(c.f, rooms, func() ([]api.Event, error) { return renames, nil })
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var got []string
		for i, e := range log {
			if m.pass(e) {
				got = append(got, names[i])
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s lets through %q, want %q", c.name, got, c.want)
		}
	}
}

// A ref that events can't be about is refused with the kinds that they can, and the log's renames are
// only read for a ref that names a device.
func TestARefWithNoEventsIsRefused(t *testing.T) {
	unread := func() ([]api.Event, error) { return nil, errors.New("read the log") }
	for ref, want := range map[string]string{
		"secret/token":         "secret/token has no events",
		"client/laptop":        "client/laptop has no events",
		"hardware/overkiz/123": "hardware/overkiz/123 has no events",
		"group/lamps":          "group/lamps has no events",
		"command":              "command has no events",
		"device":               "device has no events",
		"porch-light":          "isn't a ref",
		"flow/a/b":             "isn't a ref",
	} {
		_, err := newEventMatcher(api.EventFilter{Refs: []string{ref}}, rooms, unread)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", ref, err, want)
		}
	}
	if _, err := newEventMatcher(api.EventFilter{Refs: []string{"flow/morning", "matter", "plugin/overkiz"}}, rooms, unread); err != nil {
		t.Errorf("refs that name no device read the log's renames: %v", err)
	}
	if _, err := newEventMatcher(api.EventFilter{Refs: []string{"device/fan"}}, rooms, unread); err == nil {
		t.Error("a log that can't be read was taken for one with no renames")
	}
}

// A name a device had is its only until the deploy that renamed it: another device may have it since.
func TestANameIsADevicesUntilItWasRenamed(t *testing.T) {
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	renames := []api.Event{
		{Time: at.Add(time.Hour), Type: api.EventRename, Device: "spare", Previous: "relay-1"},
		{Time: at.Add(2 * time.Hour), Type: api.EventRename, Device: "hall-lamp", Previous: "spare"},
	}
	m, err := newEventMatcher(api.EventFilter{Refs: []string{"device/hall-lamp"}}, rooms, func() ([]api.Event, error) { return renames, nil })
	if err != nil {
		t.Fatal(err)
	}
	change := func(device string, after time.Duration) api.Event {
		return api.Event{Time: at.Add(after), Type: api.EventChange, Device: device, Capability: "on", Value: true}
	}
	for _, c := range []struct {
		e    api.Event
		want bool
	}{
		{change("relay-1", 0), true},
		{change("spare", 90*time.Minute), true},
		{change("hall-lamp", 3*time.Hour), true},
		// Another relay took the name the lamp left.
		{change("relay-1", 90*time.Minute), false},
		{change("spare", 3*time.Hour), false},
	} {
		if got := m.pass(c.e); got != c.want {
			t.Errorf("%s at %s passes %t, want %t", c.e.Device, c.e.Time.Format("15:04"), got, c.want)
		}
	}
}
