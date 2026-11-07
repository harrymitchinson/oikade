package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/sun"
)

// holdDevices are the devices every hold test declares, with their capabilities.
var holdDevices = map[string][]string{
	"water-heater":    {capability.On},
	"bathroom-fan":    {capability.On},
	"bathroom-sensor": {capability.Humidity},
	"workshop-heater": {capability.On},
	"workshop-temp":   {capability.Temperature},
	"main-valve":      {capability.On},
	"leak-sensor":     {capability.Open},
	"pump":            {capability.On},
	"hall-light":      {capability.On, capability.Brightness},
	"hall-motion":     {capability.Occupied},
	"towel-rail":      {capability.On},
	"water-temp":      {capability.Temperature},
	"outdoor-temp":    {capability.Temperature},
}

// rig is an engine running holds against a fake clock, as one run of the host.
type rig struct {
	t       *testing.T
	mu      sync.Mutex
	budgets map[string]BudgetReport
	e       *Engine
	clock   *clock.Fake
	setter  *fakeSetter
	store   *hub.Store
	sched   *schedules
	logs    *logLines
	stop    func()
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) add(args string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, args)
}

// startRig runs an engine from now with these saved states and device values, and loads the Rules.
func startRig(t *testing.T, now time.Time, states HoldStates, values map[string]map[string]any, _ map[string]string, rules ...*Compiled) *rig {
	t.Helper()
	return startRigWith(t, rigOptions{now: now, holds: states, values: values}, rules...)
}

// rigOptions are what a run of the host starts from.
type rigOptions struct {
	now      time.Time
	holds    HoldStates
	controls ControlStates
	values   map[string]map[string]any
	// control is each device's control entries.
	control map[string][]control.Entry
}

func startRigWith(t *testing.T, o rigOptions, rules ...*Compiled) *rig {
	t.Helper()
	now, states, values := o.now, o.holds, o.values
	logs := &logLines{}
	r := &rig{t: t, clock: clock.NewFake(now), setter: &fakeSetter{}, sched: &schedules{}, logs: logs}
	r.e = &Engine{
		Setter: r.setter, Clock: r.clock, Location: london, Site: londonSite,
		Log:  logging.Lines(logs.add, slog.LevelDebug),
		Runs: NewRunsCounter(), NextChange: NewNextChangeGauge(), Active: NewActiveGauge(),
		OnSchedule: r.sched.record, States: states, Controls: o.controls, ControlLevel: NewControlLevelGauge(),
		OnBudget: r.recordBudget, Budget: NewBudgetMetrics(),
	}
	r.e.Init()
	r.store = hub.NewStore(logging.Discard(), r.e)
	r.e.Store = r.store
	metas := map[ident.ID]hub.Meta{}
	for name := range holdDevices {
		metas[ident.ID{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m, Control: o.control[name]}
	}
	r.store.SetDevices(z2m, metas)
	for name, caps := range holdDevices {
		r.store.Discovered(ident.ID{Namespace: ns, Name: name}, driver.Info{Found: true, Capabilities: caps})
	}
	for dev, vs := range values {
		for c, v := range vs {
			r.store.Changed(driver.Update{Device: ident.ID{Namespace: ns, Name: dev}, Capability: c, Value: v})
		}
	}
	for _, c := range rules {
		r.e.Load(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.e.Run(ctx); close(done) }()
	var once sync.Once
	r.stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(r.stop)
	r.settle()
	return r
}

// settle waits until the engine is idle: every hold evaluated, no write in progress and the
// timer armed.
func (r *rig) settle() {
	r.t.Helper()
	poll(r.t, "the engine to settle", func() bool { return r.e.Idle() && r.holdsArmed() })
}

// holdsArmed says every hold has a next look due.
func (r *rig) holdsArmed() bool {
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	now := r.clock.Now()
	for _, s := range r.e.rules {
		if s.hold != nil && !s.next.At.After(now) {
			return false
		}
	}
	return true
}

// at moves the clock to t, step by step so every timer on the way fires, and waits to settle.
func (r *rig) at(s string) {
	r.t.Helper()
	to := local(s)
	for r.clock.Now().Before(to) {
		next := r.clock.Now().Add(time.Minute)
		if next.After(to) {
			next = to
		}
		r.clock.SetTime(next)
		r.settle()
	}
}

func (r *rig) recordBudget(key ident.ID, b BudgetReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.budgets == nil {
		r.budgets = map[string]BudgetReport{}
	}
	r.budgets[key.Name] = b
}

func (r *rig) budget(name string) BudgetReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.budgets[name]
}

func (r *rig) change(dev, c string, v any) {
	r.t.Helper()
	r.store.Changed(driver.Update{Device: ident.ID{Namespace: ns, Name: dev}, Capability: c, Value: v})
	r.settle()
}

func (r *rig) expect(want ...call) {
	r.t.Helper()
	got := r.setter.got()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		r.t.Fatalf("calls = %v, want %v", got, want)
	}
}

func (r *rig) active(name string) float64 {
	return testutil.ToFloat64(r.e.Active.WithLabelValues(ns, name))
}

var heaterNight = Hold{Device: "water-heater", Capability: capability.On, Value: true, Window: &Window{Start: At{Time: "22:00"}, End: At{Time: "06:00"}}}

func testWindowHoldRestartRespectsATap(t *testing.T, states *storeBackend) {
	r := startRig(t, local("2026-10-21 21:59 BST"), states.holds(t), nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.expect()
	r.at("2026-10-21 22:00 BST")
	r.expect(call{"water-heater", capability.On, true})
	if r.active("water-heater-night") != 1 {
		t.Error("oikade_rule_active isn't 1")
	}
	if s := r.sched.last(); !s.Hold || !s.Active || !s.Next.Equal(local("2026-10-22 06:00 BST")) {
		t.Errorf("reported %+v, want active until 06:00", s)
	}
	// A Home app tap turns it off at 23:00. A hold doesn't fight it.
	r.at("2026-10-21 23:00 BST")
	r.change("water-heater", capability.On, false)
	r.stop()

	// The process restarts at 02:00. The hold is still active, as saved, so it writes nothing.
	r = startRig(t, local("2026-10-22 02:00 BST"), states.holds(t), nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.expect()
	r.at("2026-10-22 06:00 BST")
	r.expect(call{"water-heater", capability.On, false})
}

func testWindowHoldStartsAfterDowntime(t *testing.T, states *storeBackend) {
	r := startRig(t, local("2026-10-21 21:50 BST"), states.holds(t), nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.stop()
	r.expect()

	// Down from 21:50 to 02:00: the hold started meanwhile, so it writes on.
	r = startRig(t, local("2026-10-22 02:00 BST"), states.holds(t), nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.expect(call{"water-heater", capability.On, true})
}

func testOffDelaySurvivesARestart(t *testing.T, states *storeBackend) {
	fan := holdRule(t, "bathroom-humid", Hold{Device: "bathroom-fan", Capability: capability.On, Value: true, When: above("bathroom-sensor", capability.Humidity, 70), OffDelay: 10 * time.Minute})
	humidity := func(v float64) map[string]map[string]any {
		return map[string]map[string]any{"bathroom-sensor": {capability.Humidity: v}}
	}
	r := startRig(t, local("2026-10-21 08:00 BST"), states.holds(t), humidity(72), nil, fan)
	r.expect(call{"bathroom-fan", capability.On, true})
	r.at("2026-10-21 08:04 BST")
	r.change("bathroom-sensor", capability.Humidity, 69.0)
	r.at("2026-10-21 08:06 BST")
	r.change("bathroom-sensor", capability.Humidity, 71.0)
	r.at("2026-10-21 08:09 BST")
	r.change("bathroom-sensor", capability.Humidity, 68.0)
	if s := r.sched.last(); !s.Next.Equal(local("2026-10-21 08:19 BST")) {
		t.Errorf("next change = %v, want 08:19", s.Next)
	}
	r.at("2026-10-21 08:15 BST")
	r.stop()

	r = startRig(t, local("2026-10-21 08:15 BST"), states.holds(t), humidity(68), nil, fan)
	r.at("2026-10-21 08:18 BST")
	r.expect()
	r.at("2026-10-21 08:19 BST")
	r.expect(call{"bathroom-fan", capability.On, false})
}

func testMotionOffDelaySurvivesARestart(t *testing.T, states *storeBackend) {
	light := holdRule(t, "hall-motion-light", Hold{Device: "hall-light", Capability: capability.On, Value: true, When: isTrue("hall-motion", capability.Occupied), OffDelay: 5 * time.Minute})
	motion := func(v bool) map[string]map[string]any {
		return map[string]map[string]any{"hall-motion": {capability.Occupied: v}}
	}
	r := startRig(t, local("2026-10-21 20:00 BST"), states.holds(t), motion(true), nil, light)
	r.expect(call{"hall-light", capability.On, true})
	r.at("2026-10-21 20:10 BST")
	r.change("hall-motion", capability.Occupied, false)
	r.at("2026-10-21 20:12 BST")
	r.change("hall-motion", capability.Occupied, true)
	r.at("2026-10-21 20:20 BST")
	r.change("hall-motion", capability.Occupied, false)
	r.at("2026-10-21 20:22 BST")
	r.stop()

	r = startRig(t, local("2026-10-21 20:22 BST"), states.holds(t), motion(false), nil, light)
	r.at("2026-10-21 20:24 BST")
	r.expect()
	r.at("2026-10-21 20:25 BST")
	r.expect(call{"hall-light", capability.On, false})
}

var overheat = Hold{Device: "workshop-heater", Capability: capability.On, Value: false, Priority: PrioritySafety, When: above("workshop-temp", capability.Temperature, 45), Latch: true}

func testLatchedHoldActsOnStartupUntilReleased(t *testing.T, states *storeBackend) {
	hot := map[string]map[string]any{"workshop-temp": {capability.Temperature: 50.0}}
	reset := momentRule(t, "workshop-overheat-reset", Moment{Timers: []Timer{At{Time: "09:00"}}, Since: local("2026-10-21 08:00 BST"), Do: []Action{Release("workshop-overheat")}})

	// It overheated while the host was down: the latch acts on startup.
	r := startRig(t, local("2026-10-21 08:00 BST"), states.holds(t), hot, nil, holdRule(t, "workshop-overheat", overheat), reset)
	r.expect(call{"workshop-heater", capability.On, false})
	r.change("workshop-temp", capability.Temperature, 20.0)
	r.at("2026-10-21 08:30 BST")
	r.expect(call{"workshop-heater", capability.On, false})
	if s := r.sched.last(); !s.Active || !s.Next.IsZero() {
		t.Errorf("reported %+v, want active with no next change", s)
	}
	r.stop()

	// A restart keeps the latch, though it has cooled down.
	cool := map[string]map[string]any{"workshop-temp": {capability.Temperature: 20.0}}
	r = startRig(t, local("2026-10-21 08:30 BST"), states.holds(t), cool, nil, holdRule(t, "workshop-overheat", overheat), reset)
	r.expect()
	r.at("2026-10-21 09:00 BST")
	r.expect(call{"workshop-heater", capability.On, true})
	r.stop()

	// The release is saved.
	r = startRig(t, local("2026-10-21 09:05 BST"), states.holds(t), cool, nil, holdRule(t, "workshop-overheat", overheat))
	r.expect()
}

func TestWhenUnknownKeepsTheState(t *testing.T) {
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, nil, nil, holdRule(t, "workshop-overheat", overheat))
	r.expect()
	r.change("workshop-temp", capability.Temperature, 50.0)
	r.expect(call{"workshop-heater", capability.On, false})
}

func TestNumberHoldWritesNothingWhenItEnds(t *testing.T) {
	r := startRig(t, local("2026-10-21 21:59 BST"), nil, nil, nil, holdRule(t, "hall-dim", Hold{Device: "hall-light", Capability: capability.Brightness, Value: 30, Window: &Window{Start: At{Time: "22:00"}, End: At{Time: "23:00"}}}))
	r.at("2026-10-21 22:00 BST")
	r.expect(call{"hall-light", capability.Brightness, int64(30)})
	r.at("2026-10-21 23:00 BST")
	r.expect(call{"hall-light", capability.Brightness, int64(30)})
	if r.active("hall-dim") != 0 {
		t.Error("oikade_rule_active isn't 0 after the hold ended")
	}
}

func TestEditingTheTargetEndsTheOldOne(t *testing.T) {
	states := newMemoryStates[HoldState]()
	r := startRig(t, local("2026-10-21 23:00 BST"), states, nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.expect(call{"water-heater", capability.On, true})
	edited := heaterNight
	edited.Device, edited.Generation = "bathroom-fan", 2
	r.e.Load(holdRule(t, "water-heater-night", edited))
	r.settle()
	r.expect(call{"water-heater", capability.On, true}, call{"water-heater", capability.On, false},
		call{"bathroom-fan", capability.On, true})
}

func TestFailedHoldWriteRetries(t *testing.T) {
	states := newMemoryStates[HoldState]()
	r := startRig(t, local("2026-10-21 21:59 BST"), states, nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.setter.mu.Lock()
	r.setter.fail = map[string]bool{"water-heater": true}
	r.setter.mu.Unlock()
	r.at("2026-10-21 22:00 BST")
	if s, _ := states.Get(ident.ID{Namespace: ns, Name: "water-heater-night"}); s.Active {
		t.Error("saved a failed write as active")
	}
	r.setter.mu.Lock()
	r.setter.fail = nil
	r.setter.mu.Unlock()
	r.at("2026-10-21 22:01 BST")
	r.expect(call{"water-heater", capability.On, true}, call{"water-heater", capability.On, true})
}

func TestRemoveForgetsTheHold(t *testing.T) {
	states := newMemoryStates[HoldState]()
	key := ident.ID{Namespace: ns, Name: "water-heater-night"}
	r := startRig(t, local("2026-10-21 23:00 BST"), states, nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.e.Unload(key)
	if _, ok := states.Get(key); !ok {
		t.Fatal("unloading an invalid Rule forgot its state")
	}
	r.e.Remove(key)
	r.stop()
	if _, ok := states.Get(key); ok {
		t.Fatal("removing a deleted Rule kept its state")
	}
	if !r.sched.last().Cleared {
		t.Error("removing didn't clear the status")
	}
}

func TestWindowAt(t *testing.T) {
	compile := func(w Window) *window {
		return holdRule(t, "w", Hold{Device: "d", Capability: capability.On, Value: true, Window: &w}).hold.window
	}
	night := compile(Window{Start: At{Time: "22:00"}, End: At{Time: "06:00"}, Days: []time.Weekday{time.Friday}})
	for _, tc := range []struct {
		at   string
		in   bool
		next string
	}{
		{"2026-10-23 21:00 BST", false, "2026-10-23 22:00 BST"}, // Friday
		{"2026-10-23 22:00 BST", true, "2026-10-24 06:00 BST"},
		{"2026-10-24 05:59 BST", true, "2026-10-24 06:00 BST"}, // Saturday morning, started Friday
		{"2026-10-24 22:30 BST", false, "2026-10-30 22:00 GMT"},
	} {
		in, next := night.at(local(tc.at), london, nil)
		if in != tc.in || !next.Equal(local(tc.next)) {
			t.Errorf("at %s: in %v, next %v; want %v, %s", tc.at, in, next, tc.in, tc.next)
		}
	}
	allDay := compile(Window{Start: At{Time: "07:00"}, End: At{Time: "07:00"}})
	if in, _ := allDay.at(local("2026-10-23 03:00 BST"), london, nil); !in {
		t.Error("a window that ends when it starts isn't a whole day")
	}
}

func TestSunWindowFollowsTheDay(t *testing.T) {
	c := holdRule(t, "porch", Hold{Device: "d", Capability: capability.On, Value: true, Window: &Window{Start: SunTime{Event: sun.Sunset, Offset: -15 * time.Minute}, End: At{Time: "23:00"}}})
	w := c.hold.window
	for _, tc := range []struct{ day, sunset string }{
		{"2026-06-21", "21:06 BST"},
		{"2026-12-21", "15:38 GMT"},
	} {
		_, next := w.at(local(tc.day+" 12:00 "+tc.sunset[6:]), london, londonSite)
		if got := next.In(london).Format("15:04 MST"); got != tc.sunset {
			t.Errorf("%s: window starts %s, want %s", tc.day, got, tc.sunset)
		}
	}
	if in, next := w.at(local("2026-12-21 16:00 GMT"), london, nil); in || !next.IsZero() {
		t.Error("a sun window opened without a site")
	}
}

// declareAlso adds devices to the rig's store, in rooms, undiscovered.
func (r *rig) declareAlso(rooms map[string]string) {
	metas := map[ident.ID]hub.Meta{}
	for name := range holdDevices {
		metas[ident.ID{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m}
	}
	for name, room := range rooms {
		metas[ident.ID{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m, Room: room}
	}
	r.store.SetDevices(z2m, metas)
}

func (r *rig) failing(devices ...string) {
	r.setter.mu.Lock()
	defer r.setter.mu.Unlock()
	r.setter.fail = map[string]bool{}
	for _, d := range devices {
		r.setter.fail[d] = true
	}
}

func TestRoomHoldWaitsForItsMembers(t *testing.T) {
	r := startRig(t, local("2026-10-21 12:00 BST"), nil, nil, nil)
	r.declareAlso(map[string]string{"porch-lamp": "porch"})
	c := holdRule(t, "porch-day", Hold{Room: "porch", Capability: capability.On, Value: true, Window: &Window{Start: At{Time: "00:00"}, End: At{Time: "00:00"}}})
	r.e.Load(c)
	r.settle()
	r.expect()
	r.store.Discovered(ident.ID{Namespace: ns, Name: "porch-lamp"}, driver.Info{Found: true, Capabilities: []string{capability.On}})
	r.at("2026-10-21 12:01 BST")
	r.expect(call{"porch-lamp", capability.On, true})
}

func TestReleaseWhileWhenIsUnknown(t *testing.T) {
	hot := map[string]map[string]any{"workshop-temp": {capability.Temperature: 50.0}}
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, hot, nil, holdRule(t, "workshop-overheat", overheat))
	r.expect(call{"workshop-heater", capability.On, false})
	// The sensor drops out of the store, so when can't be evaluated.
	r.declareAlso(nil)
	r.store.SetDevices(z2m, map[ident.ID]hub.Meta{{Namespace: ns, Name: "workshop-heater"}: {Instance: z2m}})
	r.store.Discovered(ident.ID{Namespace: ns, Name: "workshop-heater"}, driver.Info{Found: true, Capabilities: []string{capability.On}})
	r.e.release(ident.ID{Namespace: ns, Name: "workshop-overheat"})
	r.settle()
	r.expect(call{"workshop-heater", capability.On, false})
	// It's back, and cool: the released hold ends rather than latching again.
	r.declareAlso(nil)
	r.change("workshop-temp", capability.Temperature, 20.0)
	r.expect(call{"workshop-heater", capability.On, false}, call{"workshop-heater", capability.On, true})
}

func TestChangesDontSkipTheRetryWait(t *testing.T) {
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, nil, nil, holdRule(t, "workshop-overheat", overheat))
	r.failing("workshop-heater")
	r.change("workshop-temp", capability.Temperature, 50.0)
	for i := range 5 {
		r.change("workshop-temp", capability.Temperature, 51.0+float64(i))
	}
	if n := len(r.setter.got()); n != 1 {
		t.Fatalf("%d writes before the retry, want 1", n)
	}
	r.failing()
	r.at("2026-10-21 08:01 BST")
	if n := len(r.setter.got()); n != 2 {
		t.Fatalf("%d writes after the retry, want 2", n)
	}
}

func TestEditedTargetEndsTheOldOneOnce(t *testing.T) {
	r := startRig(t, local("2026-10-21 23:00 BST"), nil, nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.failing("bathroom-fan")
	edited := heaterNight
	edited.Device, edited.Generation = "bathroom-fan", 2
	r.e.Load(holdRule(t, "water-heater-night", edited))
	r.settle()
	r.at("2026-10-21 23:02 BST")
	// The fan is retried, but the heater is ended only once.
	var heater, fan int
	for _, c := range r.setter.got() {
		switch c.device {
		case "water-heater":
			heater++
		case "bathroom-fan":
			fan++
		}
	}
	if heater != 2 || fan < 3 {
		t.Fatalf("calls = %v, want the heater on and off once and the fan retried", r.setter.got())
	}
}

// A device moving room changes no value, so the host announces it again, and a hold whose when
// reads rooms looks again then rather than at its next change.
func TestAHoldLooksAgainWhenADeviceIsAnnounced(t *testing.T) {
	inBathroom := func(v Vars) (bool, error) {
		return v["members"].(map[string]hub.Member)["bathroom-sensor"].Room == "bathroom", nil
	}
	r := startRig(t, local("2026-10-21 12:00 BST"), nil, nil, nil,
		holdRule(t, "bathroom-fan", Hold{Device: "bathroom-fan", Capability: capability.On, Value: true, When: inBathroom}))
	r.expect()
	r.declareAlso(map[string]string{"bathroom-sensor": "bathroom"})
	r.settle()
	r.expect()
	sensor := ident.ID{Namespace: ns, Name: "bathroom-sensor"}
	r.store.Discovered(sensor, driver.Info{Found: true, Capabilities: holdDevices["bathroom-sensor"]})
	r.settle()
	r.expect(call{"bathroom-fan", capability.On, true})
}
