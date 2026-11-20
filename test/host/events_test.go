package host

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// eventLog reads the host's event log, as GET /v1/events sends it.
func (r *rig) eventLog() []api.Event {
	r.t.Helper()
	_, events := r.events(api.EventFilter{})
	return events
}

// events reads what of the host's event log passes f, and its opening.
func (r *rig) events(f api.EventFilter) (api.EventLog, []api.Event) {
	r.t.Helper()
	var head api.EventLog
	var events []api.Event
	err := api.Events(r.t.Context(), api.Local{DataDir: r.dir}, f,
		func(log api.EventLog) error { head = log; return nil },
		func(e api.Event) error { events = append(events, e); return nil })
	if err != nil {
		r.t.Fatalf("reading the event log: %v", err)
	}
	return head, events
}

// online has a simulated device go offline, or come back.
func (r *rig) online(device string, online bool) {
	r.t.Helper()
	raw, _ := json.Marshal(map[string]any{"device": device, "online": online})
	if _, err := r.stdin.Write(append(raw, '\n')); err != nil {
		r.t.Fatal(err)
	}
}

// bridgeSaid matches one of the Matter bridge's own events.
func bridgeSaid(message string, fabric any) func(api.Event) bool {
	return func(e api.Event) bool {
		return e.Type == api.EventMatter && e.Ref == "matter" && e.Message == message && e.Value == fabric
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
	// The node tells of its window and fabrics once it has advertised them, a moment after pairing.
	r.until("the pairing to be in the log", func() bool {
		return slices.ContainsFunc(r.eventLog(), bridgeSaid(api.MatterControllerPaired, 1.0))
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
	opened := logged(t, log, 0, "window opening", bridgeSaid(api.MatterWindowOpened, nil))
	logged(t, log, opened, "window closing", bridgeSaid(api.MatterWindowClosed, nil))
	paired := logged(t, log, opened, "controller pairing", bridgeSaid(api.MatterControllerPaired, 1.0))
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

// A window still open when the host stops closes in the log, so the next host's is a new one, and
// each opening has its closing.
func TestAWindowOpenWhenTheHostStopsClosesInTheLog(t *testing.T) {
	t.Parallel()
	r := prepare(t)
	launch := func(flag string) { r.launchAttached(flag) }
	opened, closed := bridgeSaid(api.MatterWindowOpened, nil), bridgeSaid(api.MatterWindowClosed, nil)
	windows := func() []api.Event {
		return slices.DeleteFunc(r.eventLog(), func(e api.Event) bool { return e.Type != api.EventMatter })
	}
	port := r.serveMatter(freeUDP(t), launch)
	r.until("the window to be in the log", func() bool { return len(windows()) == 1 })
	r.stop()
	r.serveMatter(port, launch)
	r.until("the next host's window to be in the log", func() bool { return len(windows()) == 3 })
	if log := windows(); !opened(log[0]) || !closed(log[1]) || !opened(log[2]) {
		t.Errorf("the bridge's events are %+v, want a window opened, closed as the host stopped, and opened", log)
	}
}

// A filtered read of the host's log is the whole log filtered by hand, for each kind of ref and for a
// time, with the home's own events, and its opening says when the log starts whatever the filter. A
// tail with a ref and a since has that past, then only that ref's events as they happen.
func TestTheHostFiltersItsEvents(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	r := c.r
	r.sync()
	r.until("the pairing to be in the log", func() bool {
		return slices.ContainsFunc(r.eventLog(), bridgeSaid(api.MatterControllerPaired, 1.0))
	})
	r.change("bathroom-sensor", humidityCap, 80)
	r.wrote("fan", true)
	r.contact("study-go", true)
	r.until("the flow's write", func() bool { return r.count(wrote, "device", "study-heater") > 0 })
	c.command(c.endpoint("claimed"), turnOff)
	r.until("the controller's write", func() bool { return onOf(t, r.status(), "claimed").Value == false })
	r.stdout(nil, "set", "device/lamp-0/on", "true")
	// Two devices go offline, so a filter has one to leave out.
	r.online("fan", false)
	r.online("lamp-1", false)
	r.sync()

	head, all := r.events(api.EventFilter{})
	if head.Start.IsZero() || !head.Start.Equal(all[0].Time) {
		t.Errorf("the log says it starts at %s, and its first event is from %s", head.Start, all[0].Time)
	}
	byHand := func(about func(api.Event) bool) []api.Event {
		return slices.DeleteFunc(slices.Clone(all), func(e api.Event) bool {
			return e.Type != api.EventProcess && e.Type != api.EventDeploy && !about(e)
		})
	}
	homes := len(byHand(func(api.Event) bool { return false }))
	if homes == 0 {
		t.Fatalf("the log has none of the home's own events: %+v", all)
	}
	for ref, about := range map[string]func(api.Event) bool{
		"device/fan":         func(e api.Event) bool { return e.Device == "fan" },
		"device/fan/on":      func(e api.Event) bool { return e.Device == "fan" },
		"case/fan/on/fan":    func(e api.Event) bool { return e.Rule == "fan on fan" || e.By == "case/fan/on/fan" },
		"flow/study-setback": func(e api.Event) bool { return e.Rule == "study-setback" || e.By == "flow/study-setback" },
		"matter":             func(e api.Event) bool { return e.Type == api.EventMatter || e.By == "matter" },
		"room/nowhere":       func(api.Event) bool { return false },
	} {
		want := byHand(about)
		if ref != "room/nowhere" && len(want) <= homes {
			t.Errorf("the log has nothing of %s to filter for: %+v", ref, all)
		}
		opening, got := r.events(api.EventFilter{Refs: []string{ref}})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the host sent %+v, want %+v", ref, got, want)
		}
		if !opening.Start.Equal(head.Start) {
			t.Errorf("%s: the log says it starts at %s, not %s", ref, opening.Start, head.Start)
		}
	}
	since, until := all[len(all)/3].Time, all[2*len(all)/3].Time
	_, got := r.events(api.EventFilter{Since: since, Until: until})
	want := slices.DeleteFunc(slices.Clone(all), func(e api.Event) bool {
		return e.Time.Before(since) || e.Time.After(until)
	})
	if !reflect.DeepEqual(got, want) || len(want) == 0 || len(want) == len(all) {
		t.Errorf("from %s to %s the host sent %d events, want %d of %d", since, until, len(got), len(want), len(all))
	}
	err := api.Events(t.Context(), api.Local{DataDir: r.dir}, api.EventFilter{Refs: []string{"secret/token"}},
		func(api.EventLog) error { return nil }, func(api.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "secret/token has no events") {
		t.Errorf("a secret's events: %v", err)
	}

	// The lamp's past, then the lamp as it happens, and nothing of the fan either way.
	lamp := func(e api.Event) bool { return e.Device == "lamp-0" }
	past := byHand(lamp)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var tailed []api.Event
	following := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- api.Tail(ctx, api.Local{DataDir: r.dir}, api.EventFilter{Refs: []string{"device/lamp-0"}, Since: head.Start},
			func() { close(following) }, func(e api.Event) error {
				mu.Lock()
				defer mu.Unlock()
				tailed = append(tailed, e)
				return nil
			})
	}()
	select {
	case <-following:
	case err := <-ended:
		t.Fatalf("the tail ended: %v", err)
	case <-time.After(waitLimit):
		t.Fatal("the tail never said it was following")
	}
	mu.Lock()
	if !reflect.DeepEqual(tailed, past) {
		t.Errorf("the tail's past is %+v, want %+v", tailed, past)
	}
	mu.Unlock()
	r.change("bathroom-sensor", humidityCap, 40)
	r.stdout(nil, "set", "device/lamp-0/on", "false")
	r.until("the tail to have the lamp's write", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(tailed) > len(past) && tailed[len(tailed)-1].Type == api.EventChange
	})
	mu.Lock()
	defer mu.Unlock()
	for _, e := range tailed[len(past):] {
		if !lamp(e) {
			t.Errorf("the tail of the lamp has %+v", e)
		}
	}
	if n := len(tailed) - len(past); n != 2 {
		t.Errorf("the tail has %d events since it began, want the write and the change: %+v", n, tailed[len(past):])
	}
}

// oikade history of a device shows what a rule wrote to it, by the rule's ref, then the change that
// made, and nothing of another device. A device nothing has on record has an empty history that says
// so. oikade tail with a ref and --since prints that past, then follows.
func TestHistoryShowsWhoWroteADevice(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("bathroom-sensor", humidityCap, 80)
	r.wrote("fan", true)
	r.sync()

	for _, device := range []string{"device/fan", "fan"} {
		out := string(r.stdout(nil, "history", device, "--since", "1h"))
		wrote := strings.Index(out, "device/fan/on  wrote true  by case/fan/on/fan")
		changed := strings.Index(out, "device/fan/on  true  was false")
		if wrote < 0 || changed < wrote || strings.Contains(out, "bathroom-sensor") || !strings.Contains(out, ", today\n") {
			t.Errorf("oikade history %s --since 1h:\n%s", device, out)
		}
	}
	var by []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(r.stdout(nil, "history", "fan", "--json"))), "\n") {
		var e api.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("history --json printed %q: %v", line, err)
		}
		if e.Type == api.EventWrite {
			by = append(by, e.By)
		}
	}
	if !slices.Equal(by, []string{"case/fan/on/fan"}) {
		t.Errorf("the fan's writes are by %q", by)
	}
	if out := string(r.stdout(nil, "history", "fan", "--since", "1h", "--until", "30m")); out != "" {
		t.Errorf("the fan's history up to half an hour ago:\n%s", out)
	}
	stderr, err := r.oikade(nil, "history", "device/nothing")
	if err != nil || !strings.Contains(stderr, "No events of device/nothing since ") ||
		!strings.Contains(stderr, "There's no device/nothing on record: oikade devices lists them.") {
		t.Errorf("a history of nothing: %v: %s", err, stderr)
	}

	events := r.tailing("fan", "--since", "1h")
	fanWrote := func(value bool, by string) func(api.Event) bool {
		return func(e api.Event) bool {
			if e.Device != "" && e.Device != "fan" {
				t.Errorf("the fan's tail has %+v", e)
			}
			return e.Type == api.EventWrite && e.Value == value && e.By == by
		}
	}
	awaitEvent(t, events, fanWrote(true, "case/fan/on/fan"))
	// With the case over, no rule's claim decides the fan: it goes back to its default, which is nobody's.
	r.change("bathroom-sensor", humidityCap, 40)
	awaitEvent(t, events, fanWrote(false, ""))
}
