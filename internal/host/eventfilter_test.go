package host

import (
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/host/api"
	"oikade.dev/internal/ident"
)

// aLog is one of each thing a log holds, each named by a word a test asks for it by. An event has a
// device by its key: the hall lamp is hue/0x1, which was relay-3, the AC toshibaac/ac1, the valve
// ewelink/v1 and the porch light, simulated, porch-light.
func aLog(at time.Time) (events []api.Event, names []string) {
	add := func(name string, e api.Event) {
		e.Time = at.Add(time.Duration(len(events)) * time.Minute)
		if e.Ref == "" {
			e.Ref = eventRef(e)
		}
		events, names = append(events, e), append(names, name)
	}
	add("connected", api.Event{Type: api.EventProcess, Message: "connected"})
	add("relay on", api.Event{Type: api.EventChange, Device: "hue/0x1", Capability: "on", Value: true})
	add("renamed", api.Event{Type: api.EventRename, Device: "hue/0x1", Previous: "relay-3", Message: "hall-lamp"})
	add("lamp on", api.Event{Type: api.EventChange, Device: "hue/0x1", Capability: "on", Value: true, Previous: false})
	add("lamp dimmed", api.Event{Type: api.EventChange, Device: "hue/0x1", Capability: "brightness", Value: 40.0})
	add("lamp offline", api.Event{Type: api.EventAvailability, Device: "hue/0x1", Message: api.AvailabilityOffline})
	add("dusk wrote", api.Event{Type: api.EventWrite, Device: "porch-light", Capability: "on", Value: true, By: "case/porch-light/on/dusk"})
	add("porch on", api.Event{Type: api.EventChange, Device: "porch-light", Capability: "on", Value: true})
	add("morning ran", api.Event{Type: api.EventRule, Rule: "morning", Result: "succeeded"})
	add("morning called", api.Event{Type: api.EventCall, Rule: "morning", Message: "forecast", Value: true})
	add("morning wrote", api.Event{Type: api.EventWrite, Device: "toshibaac/ac1", Capability: "mode", Value: "heat", By: "flow/morning"})
	add("morning paused", api.Event{Type: api.EventPause, Rule: "morning", Message: "paused"})
	add("dusk ran", api.Event{Type: api.EventRule, Rule: "porch-light on dusk", Result: "succeeded"})
	add("tap", api.Event{Type: api.EventWrite, Device: "toshibaac/ac1", Capability: "setpoint", Value: 21.0, By: "matter"})
	add("tap refused", api.Event{Type: api.EventRefused, Device: "ewelink/v1", Capability: "on", Value: true, Message: "held"})
	add("paired", api.Event{Type: api.EventMatter, Message: api.MatterControllerPaired, Value: 1.0})
	add("set", api.Event{Type: api.EventWrite, Device: "toshibaac/ac1", Capability: "on", Value: false, By: "command"})
	add("ac off", api.Event{Type: api.EventChange, Device: "toshibaac/ac1", Capability: "on", Value: false})
	add("login refused", api.Event{Type: api.EventPlugin, Ref: "plugin/overkiz", Message: "login refused"})
	add("lamp dusk ran", api.Event{Type: api.EventRule, Rule: "hue/0x1 on dusk", Result: "succeeded"})
	add("lamp dusk wrote", api.Event{Type: api.EventWrite, Device: "hue/0x1", Capability: "on", Value: false, By: "case/hue%2F0x1/on/dusk"})
	add("deployed", api.Event{Type: api.EventDeploy, Message: "f113018"})
	add("valve forgotten", api.Event{Type: api.EventForget, Device: "ewelink/v1"})
	return events, names
}

// aHome is a home whose hall has the lamp, and whose office has the AC. The valve is on record no
// longer, and relay-3 is no device's name.
func aHome() home {
	keys := map[string]string{"hall-lamp": "hue/0x1", "office-ac": "toshibaac/ac1", "porch-light": "porch-light", "valve": "ewelink/v1"}
	return home{
		rooms: func(room string) []ident.ID {
			return map[string][]ident.ID{"hall": {{Name: "hue/0x1"}}, "office": {{Name: "toshibaac/ac1"}}}[room]
		},
		device: func(name string) (ident.ID, bool) {
			k, ok := keys[name]
			return ident.ID{Name: k}, ok
		},
		rule: func(key string) string {
			if dev, rest, ok := strings.Cut(key, " "); ok && keys[dev] != "" {
				return keys[dev] + " " + rest
			}
			return key
		},
		names: func(key string) (string, bool) {
			for name, k := range keys {
				if k == key {
					return name, true
				}
			}
			return "", false
		},
	}
}

// Each kind of ref lets through its own events and no others, with the home's own: a deploy, and
// your automations connecting.
func TestEachKindOfRefHasItsEventsAndTheHomes(t *testing.T) {
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	log, names := aLog(at)
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
			home("relay on", "renamed", "lamp on", "lamp dimmed", "lamp offline", "lamp dusk wrote")},
		{"a device by a name it left", api.EventFilter{Refs: []string{"device/relay-3"}}, home()},
		{"a capability, and what happened to its device", api.EventFilter{Refs: []string{"device/hall-lamp/brightness"}},
			home("renamed", "lamp dimmed", "lamp offline")},
		{"a room", api.EventFilter{Refs: []string{"room/office"}}, home("morning wrote", "tap", "set", "ac off")},
		{"a room's capability", api.EventFilter{Refs: []string{"room/office/on"}}, home("set", "ac off")},
		{"a room that isn't", api.EventFilter{Refs: []string{"room/attic"}}, home()},
		{"a flow", api.EventFilter{Refs: []string{"flow/morning"}},
			home("morning ran", "morning called", "morning wrote", "morning paused")},
		{"a case", api.EventFilter{Refs: []string{"case/porch-light/on/dusk"}}, home("dusk wrote", "dusk ran")},
		{"a case, by its device's name", api.EventFilter{Refs: []string{"case/hall-lamp/on/dusk"}},
			home("lamp dusk ran", "lamp dusk wrote")},
		{"matter", api.EventFilter{Refs: []string{"matter"}}, home("tap", "tap refused", "paired")},
		{"a plugin", api.EventFilter{Refs: []string{"plugin/overkiz"}}, home("login refused")},
		{"a device no longer on record", api.EventFilter{Refs: []string{"device/valve"}},
			[]string{"connected", "tap refused", "deployed", "valve forgotten"}},
		{"two refs", api.EventFilter{Refs: []string{"plugin/overkiz", "device/porch-light"}},
			home("dusk wrote", "porch on", "login refused")},
		{"a time", api.EventFilter{Since: at.Add(2 * time.Minute), Until: at.Add(4 * time.Minute)},
			[]string{"renamed", "lamp on", "lamp dimmed"}},
		{"a ref and a time", api.EventFilter{Refs: []string{"device/hall-lamp"}, Since: at.Add(3 * time.Minute)},
			[]string{"lamp on", "lamp dimmed", "lamp offline", "lamp dusk wrote", "deployed"}},
	} {
		m, err := newEventMatcher(c.f, aHome())
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

// A ref that events can't be about is refused with the kinds that they can.
func TestARefWithNoEventsIsRefused(t *testing.T) {
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
		if _, err := newEventMatcher(api.EventFilter{Refs: []string{ref}}, aHome()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", ref, err, want)
		}
	}
}

// A device's ref is its hardware's events, whatever it was called when they happened, and a tail
// that follows it carries on through a rename. Another device that takes a name it left isn't it.
func TestADevicesRefIsItsHardware(t *testing.T) {
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	m, err := newEventMatcher(api.EventFilter{Refs: []string{"device/hall-lamp/on"}}, aHome())
	if err != nil {
		t.Fatal(err)
	}
	on := func(key string, after time.Duration) api.Event {
		return api.Event{Time: at.Add(after), Type: api.EventChange, Device: key, Capability: "on", Value: true}
	}
	events := []api.Event{
		on("hue/0x1", 0), on("hue/0x2", 0),
		{Time: at.Add(time.Hour), Type: api.EventRename, Device: "hue/0x1", Previous: "relay-3", Message: "hall-lamp"},
		{Time: at.Add(time.Hour), Type: api.EventRename, Device: "hue/0x2", Previous: "hall-lamp", Message: "spare"},
		on("hue/0x1", 2*time.Hour), on("hue/0x2", 2*time.Hour),
	}
	got := make([]bool, 0, len(events))
	for _, e := range events {
		got = append(got, m.pass(e))
	}
	if want := []bool{true, false, true, false, true, false}; !slices.Equal(got, want) {
		t.Errorf("let through %v, want %v", got, want)
	}
}

// An event from before the log kept a device's key has its name, which its ref finds too.
func TestADevicesRefFindsItsEventsFromBeforeKeys(t *testing.T) {
	old := []api.Event{
		{Type: api.EventChange, Device: "hall-lamp", Capability: "on", Value: true},
		{Type: api.EventRule, Rule: "hall-lamp on dusk"},
		{Type: api.EventWrite, Device: "hall-lamp", Capability: "on", By: "case/hall-lamp/on/dusk"},
		{Type: api.EventChange, Device: "office-ac", Capability: "on", Value: true},
	}
	for ref, want := range map[string][]bool{
		"device/hall-lamp":       {true, false, true, false},
		"room/hall":              {true, false, true, false},
		"case/hall-lamp/on/dusk": {false, true, true, false},
	} {
		m, err := newEventMatcher(api.EventFilter{Refs: []string{ref}}, aHome())
		if err != nil {
			t.Fatal(err)
		}
		got := make([]bool, 0, len(old))
		for _, e := range old {
			got = append(got, m.pass(e))
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s let through %v, want %v", ref, got, want)
		}
	}
}
