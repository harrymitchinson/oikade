package simhome

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

const (
	// echoWindow is how long after a write a change to the same capability counts as the device
	// answering it. A replay's device answers by itself, so those changes aren't fed in.
	echoWindow = 10 * time.Second
	// writeTolerance is how far apart two of the same write can be and still be the same one. A real
	// write lags what caused it by a moment, and a simulated one doesn't.
	writeTolerance = 5 * time.Second
	// seenEvery is how often replayed devices count as seen. The log has their changes and not each
	// report, and a budget refuses a reading that has gone stale.
	seenEvery = 10 * time.Minute
	// downFor is the shortest time your automations were down worth saying so.
	downFor = time.Minute
	// startingFor is how long from the log's start a device's first report counts as how it started.
	startingFor = time.Minute
)

// hostZone names the fixed offset used for a host that can't name its time zone.
const hostZone = "host"

// ReplayHeader is the first line of the file oikade replay hands your binary. Its events follow, a
// line of JSON each.
type ReplayHeader struct {
	Log api.EventLog `json:"log"`
	// Since is where the comparison starts. Your code runs from the log's start regardless, so that
	// it reaches Since in the state it would have been in.
	Since time.Time `json:"since"`
	// Devices are the host's.
	Devices []Device `json:"devices"`
}

// Report is what a replay found. Its JSON is what oikade replay --json prints, so scripts rely on it:
// add to it, and don't rename.
type Report struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Changes is how many outside changes went in.
	Changes int `json:"changes"`
	// Deployed and Yours are how many writes each made, from From to To.
	Deployed int `json:"deployed"`
	Yours    int `json:"yours"`
	// Removed are writes the deployed version made and your code doesn't, and Added the reverse.
	Removed []Write `json:"removed"`
	Added   []Write `json:"added"`
	// Skipped counts flows' outside calls, by name, with no recorded answer. No call is ever made: the
	// rest take the answers the deployed version got.
	Skipped map[string]int `json:"skipped,omitempty"`
	// Notes are what could explain a difference that isn't your change.
	Notes []string `json:"notes,omitempty"`
}

// ReplayFile runs a home against the file oikade replay wrote. o says what the home is, and which
// devices it has beyond the host's.
func ReplayFile(o Options, path string) (Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = f.Close() }()
	lines := json.NewDecoder(bufio.NewReader(f))
	var head ReplayHeader
	if err := lines.Decode(&head); err != nil {
		return Report{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var events []api.Event
	for lines.More() {
		var e api.Event
		if err := lines.Decode(&e); err != nil {
			return Report{}, fmt.Errorf("reading %s: %w", path, err)
		}
		events = append(events, e)
	}
	o.Devices = slices.Concat(head.Devices, o.Devices)
	return Replay(o, head, events)
}

// Replay runs your code against what happened, on a fake clock, and compares its writes with the
// ones the deployed version made. It's a Sim like any other, so a write goes nowhere but into a list:
// there's no socket, no plugin and no device here to send one to.
func Replay(o Options, head ReplayHeader, events []api.Event) (Report, error) {
	// The log goes on being read after the host says when it started, and nothing runs that far.
	if i := slices.IndexFunc(events, func(e api.Event) bool { return e.Time.After(head.Log.Now) }); i >= 0 {
		events = events[:i]
	}
	o.Dry, o.Location, o.Calls = true, location(head.Log), callsOf(events)
	// A second before the first event, so that what your code does on starting comes before it.
	o.Start = head.Log.Now
	if len(events) > 0 {
		o.Start = events[0].Time.Add(-time.Second)
	}
	o.Start = o.Start.In(o.Location)
	o.Devices = replayDevices(o.Devices, head.Log.Baseline, events)
	outside := withoutEchoes(events)
	outside = startingValues(o.Devices, outside, o.Start)
	sim, err := New(o)
	if err != nil {
		return Report{}, err
	}
	defer sim.Close()

	r := Report{From: o.Start, To: head.Log.Now}
	if head.Since.After(r.From) {
		r.From = head.Since
	}
	// A device's changes at the same moment came in one report, which the host took in as a whole.
	for len(outside) > 0 {
		e, n := outside[0], 1
		for n < len(outside) && outside[n].Device == e.Device && outside[n].Time.Equal(e.Time) {
			n++
		}
		report := map[string]any{}
		for _, c := range outside[:n] {
			report[c.Capability] = c.Value
		}
		outside = outside[n:]
		if err := sim.replayTo(e.Time); err != nil {
			return Report{}, err
		}
		if err := sim.Report(e.Device, report); err != nil {
			return Report{}, fmt.Errorf("replaying %s at %s: %w", e.Device, e.Time, err)
		}
		if e.Time.After(r.From) {
			r.Changes += n
		}
	}
	if err := sim.replayTo(r.To); err != nil {
		return Report{}, err
	}

	var deployed, yours []Write
	for _, e := range events {
		if e.Type == api.EventWrite && e.Time.After(r.From) {
			deployed = append(deployed, Write{Time: e.Time, Device: e.Device, Capability: e.Capability, Value: e.Value})
		}
	}
	for _, w := range sim.Writes() {
		if w.Time.After(r.From) {
			yours = append(yours, w)
		}
	}
	r.Deployed, r.Yours = len(deployed), len(yours)
	r.Removed, r.Added = diffWrites(deployed, yours)
	sim.mu.Lock()
	r.Skipped = maps.Clone(sim.skipped)
	sim.mu.Unlock()
	r.Notes = notes(events, r.From, r.To, o.Location)
	if o.Location.String() == hostZone {
		r.Notes = append(r.Notes, "The host didn't name its time zone, so a clock change in the log throws times of day out")
	}
	if head.Since.Before(o.Start) && len(events) > 0 {
		r.Notes = append(r.Notes, "The event log only goes back to "+stamp(o.Start, o.Location))
	}
	return r, nil
}

// replayTo moves the clock on, with every device still reporting. A device that went quiet in the
// real home looks alive here, which the log can't tell apart from one with nothing new to say.
func (s *Sim) replayTo(to time.Time) error {
	for s.clock.Now().Before(to) {
		if err := s.Advance(min(to.Sub(s.clock.Now()), seenEvery)); err != nil {
			return err
		}
		if now := s.clock.Now(); now.Sub(s.swept) >= seenEvery {
			s.swept = now
			for _, d := range s.devices {
				s.store.Seen(key(d.Name), now)
			}
		}
	}
	return nil
}

// CallKey is where a flow's outside call waits for its answer. Two flows may use one name.
func CallKey(rule, name string) string { return rule + "/" + name }

func callsOf(events []api.Event) map[string][]api.Event {
	calls := map[string][]api.Event{}
	for _, e := range events {
		if e.Type == api.EventCall {
			calls[CallKey(e.Rule, e.Message)] = append(calls[CallKey(e.Rule, e.Message)], e)
		}
	}
	return calls
}

func location(log api.EventLog) *time.Location {
	if log.Zone != "" {
		if l, err := time.LoadLocation(log.Zone); err == nil {
			return l
		}
	}
	return time.FixedZone(hostZone, log.Offset)
}

// replayDevices is every device anything names: the host's, your code's, and any in the log since
// gone. Each one takes the capabilities the log shows, and its values before the log began.
func replayDevices(known []Device, baseline map[string]map[string]any, events []api.Event) []Device {
	byName := map[string]*Device{}
	var names []string
	device := func(name string) *Device {
		d := byName[name]
		if d == nil {
			d = &Device{Name: name, Values: map[string]any{}}
			byName[name] = d
			names = append(names, name)
		}
		return d
	}
	for _, k := range known {
		d := device(k.Name)
		d.Room = cmp.Or(d.Room, k.Room)
		d.Capabilities = append(d.Capabilities, k.Capabilities...)
	}
	for name, values := range baseline {
		d := device(name)
		for c, v := range values {
			d.Values[c] = v
			d.Capabilities = append(d.Capabilities, c)
		}
	}
	for _, e := range events {
		if e.Type == api.EventChange || e.Type == api.EventWrite {
			d := device(e.Device)
			d.Capabilities = append(d.Capabilities, e.Capability)
		}
	}
	slices.Sort(names)
	out := make([]Device, 0, len(names))
	for _, name := range names {
		d := byName[name]
		slices.Sort(d.Capabilities)
		d.Capabilities = slices.Compact(d.Capabilities)
		out = append(out, *d)
	}
	return out
}

// startingValues moves what devices first report as the log begins into the devices, and returns the
// changes left. Those aren't things changing: your automations hadn't started when the host heard
// them, and a simulated arbiter would take one for someone overriding the device. Later on a first
// report is a real one, from a device paired since.
func startingValues(devices []Device, outside []api.Event, start time.Time) []api.Event {
	byName := map[string]map[string]any{}
	for _, d := range devices {
		byName[d.Name] = d.Values
	}
	return slices.DeleteFunc(outside, func(e api.Event) bool {
		values := byName[e.Device]
		if _, known := values[e.Capability]; known || e.Time.Sub(start) > startingFor {
			return false
		}
		values[e.Capability] = e.Value
		return true
	})
}

// withoutEchoes is the changes that came from outside your automations: a person, a sensor. Feeding
// in the rest would have the deployed version's writes happen whatever your code does. A switch
// answers once with the value written, so anything else is someone changing it. A level may pass
// through several values on the way, so any change soon after the write is its answer.
func withoutEchoes(events []api.Event) []api.Event {
	type target struct{ device, capability string }
	wrote := map[target]api.Event{}
	var out []api.Event
	for _, e := range events {
		t := target{e.Device, e.Capability}
		switch e.Type {
		case api.EventWrite:
			wrote[t] = e
		case api.EventChange:
			w, ok := wrote[t]
			if !ok || e.Time.Sub(w.Time) > echoWindow {
				out = append(out, e)
				continue
			}
			if _, level := e.Value.(float64); level {
				continue
			}
			if fmt.Sprint(e.Value) != fmt.Sprint(w.Value) {
				out = append(out, e)
			}
			delete(wrote, t)
		}
	}
	return out
}

// diffWrites pairs each write with the same one in the other list, and returns those with no pair.
// A retry of a write isn't a new one, so neither list counts repeats within the tolerance.
func diffWrites(deployed, yours []Write) (removed, added []Write) {
	deployed, yours = withoutRepeats(deployed), withoutRepeats(yours)
	paired := make([]bool, len(yours))
	for _, d := range deployed {
		i := nextSame(yours, d, 0)
		for i >= 0 && paired[i] {
			i = nextSame(yours, d, i+1)
		}
		if i < 0 {
			removed = append(removed, d)
			continue
		}
		paired[i] = true
	}
	for i, y := range yours {
		if !paired[i] {
			added = append(added, y)
		}
	}
	return removed, added
}

func nextSame(writes []Write, w Write, from int) int {
	for i := from; i < len(writes); i++ {
		if sameWrite(w, writes[i]) {
			return i
		}
	}
	return -1
}

func sameWrite(a, b Write) bool {
	return a.Device == b.Device && a.Capability == b.Capability && fmt.Sprint(a.Value) == fmt.Sprint(b.Value) &&
		a.Time.Sub(b.Time).Abs() <= writeTolerance
}

func withoutRepeats(writes []Write) []Write {
	type target struct{ device, capability string }
	last := map[target]Write{}
	var out []Write
	for _, w := range writes {
		t := target{w.Device, w.Capability}
		if l, ok := last[t]; !ok || !sameWrite(l, w) {
			out = append(out, w)
		}
		last[t] = w
	}
	return out
}

// notes say what happened that a difference could come from, other than your change.
func notes(events []api.Event, from, to time.Time, zone *time.Location) []string {
	var out []string
	var down time.Time
	for _, e := range events {
		at := stamp(e.Time, zone)
		switch {
		case e.Type == api.EventProcess && e.Message == "disconnected":
			down = e.Time
		case e.Type == api.EventProcess && e.Message == "connected":
			if !down.IsZero() && e.Time.After(from) && e.Time.Sub(down) >= downFor {
				out = append(out, fmt.Sprintf("Your automations were down from %s to %s", stamp(down, zone), at))
			}
			down = time.Time{}
		case !e.Time.After(from):
		case e.Type == api.EventDeploy:
			out = append(out, fmt.Sprintf("Version %s took over at %s, so writes before that were an older version's", e.Message, at))
		case e.Type == api.EventPause:
			out = append(out, fmt.Sprintf("Rule %s was %s at %s. A replay doesn't pause it", e.Rule, e.Message, at))
		}
	}
	if !down.IsZero() && to.Sub(down) >= downFor {
		out = append(out, "Your automations have been down since "+stamp(down, zone))
	}
	return out
}

func stamp(t time.Time, zone *time.Location) string {
	return t.In(zone).Format("Mon 2 Jan 15:04")
}

// String is a write as oikade replay shows it.
func (w Write) String() string {
	return fmt.Sprintf("%s %s = %v", w.Device, w.Capability, w.Value)
}
