// Package simhome runs a home in one process, with no host and no hardware, on a clock it moves
// itself. It's the real engine, with the real arbiters and planner, which is what oikade dev and the
// hometest package are made of.
package simhome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/sun"
)

const (
	// settleTimeout is how long the engine has to catch up with one move of the clock.
	settleTimeout = 10 * time.Second
)

// instance is the driver instance simulated devices belong to.
var instance = driver.InstanceKey{Driver: "simulated", Name: "home"}

// Home is a home to run. Compile runs again at each restart, as a new process would.
type Home struct {
	Compile func() ([]*engine.Compiled, map[string]*flow.Flow, error)
	Control []wire.DeviceControl
	// Site is where sunrise and sunset are worked out for. Nil means they never happen.
	Site *sun.Site
}

// Device is a simulated device.
type Device struct {
	Name, Room   string
	Capabilities []string
	// DeviceTypes are what it is, which a room write can be limited to.
	DeviceTypes []devicetype.Type
	Values      map[string]any
}

// Write is something your automations wrote to a device.
type Write struct {
	Time       time.Time `json:"time"`
	Device     string    `json:"device"`
	Capability string    `json:"capability"`
	Value      any       `json:"value"`
}

// Claim is a claim on an arbitrated capability.
type Claim struct {
	Source string
	// Level is Normal, Override or Safety.
	Level   string
	Value   any
	Winner  bool
	Latched bool
}

// Event is something the home did, for a person to read.
type Event struct {
	Time time.Time
	Text string
}

// Options configure a Sim.
type Options struct {
	Home    Home
	Devices []Device
	Start   time.Time
	// Location is where the home is. Nil means Start's.
	Location *time.Location
	Log      *slog.Logger
	// Dry makes no outside call from a flow. Each takes its answer from Calls, by CallKey and in
	// order, or goes without and is counted.
	Dry   bool
	Calls map[string][]api.Event
}

// Sim is a running simulated home. Its methods are for one goroutine at a time.
type Sim struct {
	home     Home
	dry      bool
	calls    map[string][]api.Event
	devices  []Device
	clock    *clock.Fake
	location *time.Location
	log      *slog.Logger
	store    *hub.Store

	holds    *memory[engine.HoldState]
	controls *memory[engine.ControlState]
	journal  *journal

	engine  *engine.Engine
	flows   map[string]*flow.Flow
	stop    func()
	stopped chan struct{}

	mu     sync.Mutex
	writes []Write
	events []Event
	active map[string]bool
	ended  map[string][]string
	// runs counts the runs going on, by rule.
	runs  map[string]int
	fires map[string]time.Time
	// swept is when a replay last counted its devices as seen.
	swept time.Time
	// skipped counts the outside calls a dry run left out, by name.
	skipped map[string]int
}

// New starts a home.
func New(o Options) (*Sim, error) {
	s := &Sim{
		home: o.Home, devices: o.Devices, clock: clock.NewFake(o.Start), location: o.Location, log: logging.OrDiscard(o.Log),
		holds: newMemory[engine.HoldState](), controls: newMemory[engine.ControlState](), journal: newJournal(),
		ended: map[string][]string{}, fires: map[string]time.Time{}, active: map[string]bool{},
		runs: map[string]int{}, dry: o.Dry, calls: maps.Clone(o.Calls), skipped: map[string]int{},
	}
	if s.location == nil {
		s.location = o.Start.Location()
	}
	s.store = hub.NewStore(logging.Discard())
	metas := map[ident.ID]hub.Meta{}
	for _, d := range o.Devices {
		metas[key(d.Name)] = hub.Meta{
			DisplayName: d.Name, Room: d.Room, Instance: instance, Control: s.entries(d.Name), DeviceTypes: d.DeviceTypes,
		}
	}
	s.store.SetDevices(instance, metas)
	for _, d := range o.Devices {
		s.store.Discovered(key(d.Name), driver.Info{Found: true, Capabilities: d.Capabilities})
		for _, name := range slices.Sorted(maps.Keys(d.Values)) {
			v, err := capability.Validate(name, d.Values[name])
			if err != nil {
				return nil, fmt.Errorf("device %s: %w", d.Name, err)
			}
			s.store.Changed(driver.Update{Device: key(d.Name), Capability: name, Value: v, Time: o.Start})
		}
		s.store.Seen(key(d.Name), o.Start)
	}
	if err := s.start(); err != nil {
		return nil, err
	}
	return s, s.settle()
}

func key(name string) ident.ID {
	return ident.ID{Namespace: wire.Namespace, Name: name}
}

func (s *Sim) entries(device string) []control.Entry {
	var out []control.Entry
	for _, c := range s.home.Control {
		if c.Device == device {
			out = append(out, c.Entry)
		}
	}
	return out
}

// start begins a process's life, from what the last one left.
func (s *Sim) start() error {
	rules, flows, err := s.home.Compile()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	eng := &engine.Engine{
		Store: s.store, Setter: driver.SetterFunc(s.set), States: s.holds, Controls: s.controls, Clock: s.clock,
		Location: s.location, Site: s.home.Site, Log: s.log.With("logger", "automations"),
		OnBudget: func(key ident.ID, r engine.BudgetReport) {
			if r.Unplanned {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.note("Budget "+key.Name, " couldn't plan: "+r.Message)
			}
		},
		OnEnd: func(key ident.ID, result string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.runs[key.Name]--
			if result != engine.ResultCancelled {
				s.ended[key.Name] = append(s.ended[key.Name], result)
				s.note(key.Name, " "+result)
			}
		},
		OnSchedule: func(key ident.ID, sc engine.Schedule) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if sc.Hold && sc.Active != s.active[key.Name] {
				s.active[key.Name] = sc.Active
				s.note("Hold "+key.Name, map[bool]string{true: " started", false: " ended"}[sc.Active])
			}
			if !sc.LastFire.IsZero() {
				s.fires[key.Name] = sc.LastFire
			}
		},
	}
	eng.OnStart = func(key ident.ID, _ time.Time) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.runs[key.Name]++
		s.note(key.Name, " started")
	}
	eng.Init()
	s.store.Listeners = []driver.Sink{eng}
	for _, r := range rules {
		if r.HasTimers() {
			s.mu.Lock()
			since, ok := s.fires[r.Key.Name]
			s.mu.Unlock()
			if !ok {
				since = s.clock.Now()
				s.mu.Lock()
				s.fires[r.Key.Name] = since
				s.mu.Unlock()
			}
			r.KeepSince(since)
		}
		eng.Load(r)
	}
	for _, f := range flows {
		f.Journal, f.Actions, f.Clock, f.Log, f.Process = s.journal, eng, s.clock, s.log.With("logger", "flows"), ctx
		f.OnPark = eng.Park
		if s.dry {
			f.Dry = func(call string, result any) error { return s.answer(f.Key.Name, call, result) }
		}
	}
	s.engine, s.flows, s.stop, s.stopped = eng, flows, cancel, make(chan struct{})
	s.notice(eng)

	byName := map[string]*flow.Flow{}
	maps.Copy(byName, flows)
	bodies := flow.Recover(ctx, wire.Namespace, byName, s.journal.saved(), s.journal, eng)
	for name, body := range bodies {
		eng.Resume(ctx, key(name), body)
	}
	stopped := s.stopped
	go func() {
		defer close(stopped)
		eng.Run(ctx)
	}()
	return nil
}

// notice has the engine take up the device list, which a restart gives it afresh.
func (s *Sim) notice(eng *engine.Engine) {
	for _, d := range s.devices {
		eng.ControlChanged(key(d.Name))
	}
}

// set is a device taking a write, which works at once.
func (s *Sim) set(_ context.Context, dev ident.ID, name string, value any) error {
	v, err := capability.Validate(name, value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.writes = append(s.writes, Write{Time: s.clock.Now(), Device: dev.Name, Capability: name, Value: v})
	s.note("Wrote "+dev.Name+" "+name, fmt.Sprintf(" = %v", v))
	s.mu.Unlock()
	return s.report(dev, name, v)
}

func (s *Sim) report(dev ident.ID, name string, value any) error {
	snap, ok := s.store.Get(dev)
	if !ok {
		return fmt.Errorf("no device %s", dev.Name)
	}
	now := s.clock.Now()
	s.store.Seen(dev, now)
	if previous, known := snap.Values[name]; known && previous == value {
		return nil
	}
	s.store.Changed(driver.Update{Device: dev, Capability: name, Value: value, Previous: snap.Values[name], Time: now})
	return nil
}

// answer stands in for a flow's outside call, with what it answered the last time.
func (s *Sim) answer(rule, name string, result any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := CallKey(rule, name)
	waiting := s.calls[at]
	if len(waiting) == 0 {
		s.skipped[at]++
		return nil
	}
	call := waiting[0]
	s.calls[at] = waiting[1:]
	if call.Result != "" {
		return errors.New(call.Result)
	}
	if result == nil || call.Value == nil {
		return nil
	}
	raw, err := json.Marshal(call.Value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

// note records an event. Call with mu held.
func (s *Sim) note(what, how string) {
	s.events = append(s.events, Event{Time: s.clock.Now(), Text: what + how})
}

// Events are what the home has done, oldest first.
func (s *Sim) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

// Set changes a device from outside your automations, as its sensor or a tap in an app would.
func (s *Sim) Set(device, name string, value any) error {
	return s.Report(device, map[string]any{name: value})
}

// Report changes several of a device's values from outside the home, as one message from it would,
// so the home takes them in together, then waits for the home to catch up.
func (s *Sim) Report(device string, values map[string]any) error {
	dev := key(device)
	snap, ok := s.store.Get(dev)
	if !ok {
		return fmt.Errorf("no device %s", device)
	}
	now := s.clock.Now()
	var changes []driver.Update
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if !slices.Contains(snap.Info.Capabilities, name) {
			return fmt.Errorf("device %s has no %s", device, name)
		}
		v, err := capability.Validate(name, values[name])
		if err != nil {
			return err
		}
		if previous, known := snap.Values[name]; !known || previous != v {
			changes = append(changes, driver.Update{Device: dev, Capability: name, Value: v, Previous: snap.Values[name], Time: now})
		}
	}
	s.store.Seen(dev, now)
	s.store.Changed(changes...)
	return s.settle()
}

// Advance moves the clock on, from one thing the home has to do to the next.
func (s *Sim) Advance(d time.Duration) error {
	to := s.clock.Now().Add(d)
	for s.clock.Now().Before(to) {
		next := to
		if at := s.engine.Earliest(); !at.IsZero() && at.Before(next) {
			next = at
		}
		if at := s.engine.ParkedUntil(); !at.IsZero() && at.Before(next) {
			next = at
		}
		if !next.After(s.clock.Now()) {
			next = s.clock.Now().Add(time.Second)
		}
		s.clock.SetTime(next)
		if err := s.settle(); err != nil {
			return err
		}
	}
	return nil
}

// Now is the simulated time.
func (s *Sim) Now() time.Time { return s.clock.Now().In(s.location) }

// Restart stops the process and starts another from what it left, as a crash or a deploy would.
func (s *Sim) Restart() error {
	if err := s.settle(); err != nil {
		return err
	}
	s.stop()
	<-s.stopped
	s.mu.Lock()
	clear(s.runs)
	s.mu.Unlock()
	if err := s.start(); err != nil {
		return err
	}
	return s.settle()
}

// Close stops the home.
func (s *Sim) Close() {
	s.stop()
	<-s.stopped
}

// Value reads a device's value.
func (s *Sim) Value(device, name string) (any, bool) {
	snap, ok := s.store.Get(key(device))
	if !ok {
		return nil, false
	}
	v, known := snap.Values[name]
	return v, known
}

// Writes are what your automations have written, oldest first.
func (s *Sim) Writes() []Write {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.writes)
}

// Claims are the claims on an arbitrated capability.
func (s *Sim) Claims(device, name string) []Claim {
	var out []Claim
	for _, c := range s.engine.ControlOf(key(device)) {
		if c.Capability != name {
			continue
		}
		for _, o := range c.Claims {
			var v any
			if err := json.Unmarshal(o.Value, &v); err != nil {
				continue
			}
			out = append(out, Claim{Source: o.Source, Level: o.Level, Value: v, Winner: o.Winner, Latched: o.Latched})
		}
	}
	return out
}

// Finished says a flow or moment has run to the end, successfully, and nothing of it is running.
func (s *Sim) Finished(rule string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.ended[rule], engine.ResultSucceeded) && s.runs[rule] == 0
}

// Failed says how the runs of a flow or moment that didn't succeed ended.
func (s *Sim) Failed(rule string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.ended[rule]), func(r string) bool { return r == engine.ResultSucceeded })
}

// settle waits for the engine and every run to catch up with the clock.
func (s *Sim) settle() error {
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if s.engine.Idle() {
			return nil
		}
		time.Sleep(50 * time.Microsecond)
	}
	return errors.New("the home didn't settle: a condition or flow step may be stuck")
}
