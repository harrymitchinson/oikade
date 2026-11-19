package host

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// eventLog reads the host's event log, as GET /v1/events sends it.
func (r *rig) eventLog() []api.Event {
	r.t.Helper()
	var events []api.Event
	err := api.Events(r.t.Context(), api.Local{DataDir: r.dir},
		func(api.EventLog) error { return nil },
		func(e api.Event) error { events = append(events, e); return nil })
	if err != nil {
		r.t.Fatalf("reading the event log: %v", err)
	}
	return events
}

// online has a simulated device go offline, or come back.
func (r *rig) online(device string, online bool) {
	r.t.Helper()
	raw, _ := json.Marshal(map[string]any{"device": device, "online": online})
	if _, err := r.stdin.Write(append(raw, '\n')); err != nil {
		r.t.Fatal(err)
	}
}

// logged finds the first event in the log that matches, at or after from, and fails without one.
func logged(t *testing.T, log []api.Event, from int, what string, match func(api.Event) bool) int {
	t.Helper()
	i := slices.IndexFunc(log[from:], match)
	if i < 0 {
		t.Fatalf("the event log has no %s after event %d: %+v", what, from, log)
	}
	return from + i
}

// What a device's history needs is in the tail as it happens, and in the event log after: a device
// going offline and coming back, a device forgotten, and the bridge's window and controllers. A
// change keeps the value before it.
func TestADevicesHistoryIsInTheTailAndTheLog(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	r := c.r
	r.sync()
	matter := func(message string, fabric any) func(api.Event) bool {
		return func(e api.Event) bool {
			return e.Type == api.EventMatter && e.Ref == "matter" && e.Message == message && e.Value == fabric
		}
	}
	// The node tells of its window and fabrics once it has advertised them, a moment after pairing.
	r.until("the pairing to be in the log", func() bool {
		return slices.ContainsFunc(r.eventLog(), matter(api.MatterControllerPaired, 1.0))
	})
	events := r.tailing()
	tailStarted(r, events)
	availability := func(message string) func(api.Event) bool {
		return func(e api.Event) bool {
			return e.Type == api.EventAvailability && e.Device == "fan" && e.Ref == "device/fan" && e.Message == message
		}
	}
	forgotten := func(e api.Event) bool { return e.Type == api.EventForget && e.Device == "fan" && e.Ref == "device/fan" }

	r.online("fan", false)
	awaitEvent(t, events, availability(api.AvailabilityOffline))
	// Saying it again isn't going offline again.
	r.online("fan", false)
	r.online("fan", true)
	awaitEvent(t, events, availability(api.AvailabilityOnline))
	r.change("bathroom-sensor", humidityCap, 91)
	r.stdout(nil, "devices", "remove", "fan")
	awaitEvent(t, events, forgotten)

	log := r.eventLog()
	opened := logged(t, log, 0, "window opening", matter(api.MatterWindowOpened, nil))
	logged(t, log, opened, "window closing", matter(api.MatterWindowClosed, nil))
	paired := logged(t, log, opened, "controller pairing", matter(api.MatterControllerPaired, 1.0))
	offline := logged(t, log, paired, "device going offline", availability(api.AvailabilityOffline))
	online := logged(t, log, offline, "device coming back", availability(api.AvailabilityOnline))
	went := slices.DeleteFunc(slices.Clone(log), func(e api.Event) bool { return e.Type != api.EventAvailability })
	if len(went) != 2 {
		t.Errorf("availability events %+v, want the two", went)
	}
	logged(t, log, online, "change with the value before it", func(e api.Event) bool {
		return e.Type == api.EventChange && e.Device == "bathroom-sensor" && e.Value == 91.0 && e.Previous != nil
	})
	logged(t, log, online, "forgotten device", forgotten)
}

// A device a deploy renames leaves the name it had, so its history can follow it back.
func TestARenameIsInTheTailAndTheLog(t *testing.T) {
	t.Parallel()
	r := startPlugins(t)
	events := r.tailing()
	tailStarted(r, events)
	renamed := func(e api.Event) bool {
		return e.Type == api.EventRename && e.Device == "mains-valve" && e.Ref == "device/mains-valve" &&
			e.Previous == "valve"
	}
	r.mustDeploy("plugins-mains")
	awaitEvent(t, events, renamed)
	logged(t, r.eventLog(), 0, "rename", renamed)
}
