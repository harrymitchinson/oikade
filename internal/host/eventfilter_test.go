package host

import (
	"cmp"
	"errors"
	"net/http"
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

// aHome is a home whose hall has the lamp, and whose office has the AC, with these renames in its
// log. The valve and the relay's old name are on record no longer.
func aHome(renames []api.Event, err error) home {
	return home{
		rooms: func(room string) []ident.ID {
			return map[string][]ident.ID{"hall": {{Name: "hall-lamp"}}, "office": {{Name: "office-ac"}}}[room]
		},
		onRecord: func(name string) bool { return name != "valve" && name != "relay-3" && name != "relay-1" },
		renames:  func() ([]api.Event, error) { return renames, err },
	}
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
		m, err := newEventMatcher(c.f, aHome(renames, nil))
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
	unread := aHome(nil, errors.New("the disk is gone"))
	for ref, want := range map[string]string{
		"secret/token":         "isn't a ref",
		"client/laptop":        "client/laptop has no events",
		"hardware/overkiz/123": "hardware/overkiz/123 has no events",
		"group/lamps":          "group/lamps has no events",
		"command":              "command has no events",
		"device":               "device has no events",
		"porch-light":          "isn't a ref",
		"flow/a/b":             "isn't a ref",
	} {
		_, err := newEventMatcher(api.EventFilter{Refs: []string{ref}}, unread)
		if err == nil || !strings.Contains(err.Error(), want) || filterStatus(err) != http.StatusUnprocessableEntity {
			t.Errorf("%s: %v, want %q, refused", ref, err, want)
		}
	}
	if _, err := newEventMatcher(api.EventFilter{Refs: []string{"flow/morning", "matter", "plugin/overkiz"}}, unread); err != nil {
		t.Errorf("refs that name no device read the log's renames: %v", err)
	}
	// A log that can't be read is the host's failure, not the filter's.
	for _, ref := range []string{"device/fan", "case/fan/on/humid", "room/hall"} {
		_, err := newEventMatcher(api.EventFilter{Refs: []string{ref}}, unread)
		if err == nil || filterStatus(err) != http.StatusInternalServerError || err.Error() != "reading the event log: the disk is gone" {
			t.Errorf("%s with a log that can't be read: %v", ref, err)
		}
	}
}

// A name is a device's only for as long as it had it: from the deploy that gave it the name until
// the one that took it away. Another device may have had it before, or have it since.
func TestANameIsADevicesOnlyWhileItHadIt(t *testing.T) {
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	change := func(device string, after time.Duration) api.Event {
		return api.Event{Time: at.Add(after), Type: api.EventChange, Device: device, Capability: "on", Value: true}
	}
	rename := func(from, to string, after time.Duration) api.Event {
		return api.Event{Time: at.Add(after), Type: api.EventRename, Device: to, Previous: from}
	}
	type passes struct {
		e    api.Event
		want bool
	}
	for _, c := range []struct {
		name    string
		ref     string
		renames []api.Event
		events  []passes
	}{
		{
			"a device renamed twice", "device/hall-lamp",
			[]api.Event{rename("relay-1", "spare", time.Hour), rename("spare", "hall-lamp", 2*time.Hour)},
			[]passes{
				{change("relay-1", 0), true},
				{change("spare", 90*time.Minute), true},
				{change("hall-lamp", 3*time.Hour), true},
				// Another relay took the name the lamp left.
				{change("relay-1", 90*time.Minute), false},
				{change("spare", 3*time.Hour), false},
			},
		},
		{
			// The lamp became the desk lamp, and then the spare took its name.
			"a name another device had before", "device/lamp",
			[]api.Event{rename("lamp", "desk-lamp", time.Hour), rename("spare", "lamp", 2*time.Hour)},
			[]passes{
				{change("lamp", 0), false},
				{rename("lamp", "desk-lamp", time.Hour), false},
				{change("spare", 90*time.Minute), true},
				{rename("spare", "lamp", 2*time.Hour), true},
				{change("lamp", 3*time.Hour), true},
				{change("desk-lamp", 3*time.Hour), false},
			},
		},
		{
			// Paired under a name another device had left.
			"a name left before this device was paired", "device/lamp",
			[]api.Event{rename("lamp", "desk-lamp", time.Hour)},
			[]passes{{change("lamp", 0), false}, {change("lamp", 2*time.Hour), true}},
		},
		{
			"a name nothing has now", "device/relay-1",
			[]api.Event{rename("relay-0", "relay-1", time.Hour), rename("relay-1", "hall-lamp", 2*time.Hour)},
			[]passes{
				{change("relay-0", 0), true},
				{change("relay-1", 90*time.Minute), true},
				{rename("relay-1", "hall-lamp", 2*time.Hour), true},
				{change("hall-lamp", 3*time.Hour), false},
			},
		},
		{
			"a case, by each name its device had", "case/hall-lamp/on/dusk",
			[]api.Event{rename("relay-1", "hall-lamp", time.Hour)},
			[]passes{
				{api.Event{Time: at, Type: api.EventRule, Rule: "relay-1 on dusk"}, true},
				{api.Event{Time: at, Type: api.EventWrite, Device: "relay-1", Capability: "on", By: "case/relay-1/on/dusk"}, true},
				{api.Event{Time: at.Add(2 * time.Hour), Type: api.EventRule, Rule: "hall-lamp on dusk"}, true},
				{api.Event{Time: at, Type: api.EventRule, Rule: "relay-1 on dawn"}, false},
				{change("relay-1", 0), false},
			},
		},
	} {
		m, err := newEventMatcher(api.EventFilter{Refs: []string{c.ref}}, aHome(c.renames, nil))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		for _, p := range c.events {
			if got := m.pass(p.e); got != p.want {
				t.Errorf("%s: %s %s at %s passes %t, want %t", c.name, p.e.Type, cmp.Or(p.e.Device, p.e.Rule),
					p.e.Time.Format("15:04"), got, p.want)
			}
		}
	}
}

// A tail carries on with a device a deploy renames as it follows, under its new name, and the name it
// left is another device's from then on.
func TestATailFollowsADeviceThroughARename(t *testing.T) {
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	m, err := newEventMatcher(api.EventFilter{Refs: []string{"device/porch-light/on", "device/fan"}}, aHome(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	on := func(device string, after time.Duration) api.Event {
		return api.Event{Time: at.Add(after), Type: api.EventChange, Device: device, Capability: "on", Value: true}
	}
	renamed := api.Event{Time: at.Add(time.Hour), Type: api.EventRename, Device: "porch-lamp", Previous: "porch-light"}
	events := []api.Event{
		on("porch-light", 0), on("porch-lamp", 0), renamed, on("porch-lamp", 2*time.Hour), on("porch-light", 2*time.Hour),
		on("fan", 2*time.Hour),
	}
	got := make([]bool, 0, len(events))
	for _, e := range events {
		m.renamed(e)
		got = append(got, m.about(e) && m.pass(e))
	}
	if want := []bool{true, false, true, true, false, true}; !slices.Equal(got, want) {
		t.Errorf("the tail let through %v, want %v", got, want)
	}
}
