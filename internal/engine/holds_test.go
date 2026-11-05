package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
)

// holdRule decodes a Rule spec in the test namespace.
func holdRule(t *testing.T, name, spec string) *homev1alpha1.Rule {
	t.Helper()
	r := ruleYAML(t, spec)
	r.Name, r.Namespace, r.Generation = name, ns, 1
	return r
}

// holdDevices are the devices every hold test declares, with their capabilities.
var holdDevices = map[string][]string{
	"water-heater":    {capability.SwitchOn},
	"bathroom-fan":    {capability.SwitchOn},
	"bathroom-sensor": {capability.Humidity},
	"workshop-heater": {capability.SwitchOn},
	"workshop-temp":   {capability.Temperature},
	"main-valve":      {capability.SwitchOn},
	"leak-sensor":     {capability.ContactOpen},
	"pump":            {capability.SwitchOn},
	"hall-light":      {capability.LightOn, capability.LightLevel},
	"hall-motion":     {capability.Occupancy},
	"towel-rail":      {capability.SwitchOn},
	"water-temp":      {capability.Temperature},
	"outdoor-temp":    {capability.Temperature},
}

// rig is an engine running holds against a fake clock, as one run of the runtime.
type rig struct {
	t       *testing.T
	mu      sync.Mutex
	control map[string][]homev1alpha1.CapabilityControlStatus
	reports int
	skipped map[string][]string
	budgets map[string]BudgetReport
	e       *Engine
	clock   *clocktesting.FakeClock
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

func (l *logLines) add(_, args string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, args)
}

// startRig runs an engine from now with these saved states and device values, and loads the Rules.
func startRig(t *testing.T, now time.Time, states HoldStates, values map[string]map[string]any, _ map[string]string, rules ...*homev1alpha1.Rule) *rig {
	t.Helper()
	return startRigWith(t, rigOptions{now: now, holds: states, values: values}, rules...)
}

// rigOptions are what a run of the runtime starts from.
type rigOptions struct {
	now      time.Time
	holds    HoldStates
	controls ControlStates
	values   map[string]map[string]any
	// control is each device's control entries.
	control map[string][]homev1alpha1.CapabilityControl
}

func startRigWith(t *testing.T, o rigOptions, rules ...*homev1alpha1.Rule) *rig {
	t.Helper()
	now, states, values := o.now, o.holds, o.values
	logs := &logLines{}
	r := &rig{t: t, clock: clocktesting.NewFakeClock(now), setter: &fakeSetter{}, sched: &schedules{}, logs: logs}
	r.e = &Engine{
		Setter: r.setter, Clock: r.clock, Location: london, Site: londonSite,
		Log:  funcr.New(logs.add, funcr.Options{Verbosity: 1}),
		Runs: NewRunsCounter(), NextChange: NewNextChangeGauge(), Active: NewActiveGauge(),
		OnSchedule: r.sched.record, States: states, Controls: o.controls, ControlLevel: NewControlLevelGauge(),
		OnControl: r.recordControl, OnSkipped: r.recordSkipped,
		OnBudget: r.recordBudget, Budget: NewBudgetMetrics(),
	}
	r.e.Init()
	r.store = hub.NewStore(logr.Discard(), r.e)
	r.e.Store = r.store
	metas := map[types.NamespacedName]hub.Meta{}
	for name := range holdDevices {
		metas[types.NamespacedName{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m, Control: o.control[name]}
	}
	r.store.SetDevices(z2m, metas)
	for name, caps := range holdDevices {
		r.store.Discovered(types.NamespacedName{Namespace: ns, Name: name}, driver.Info{Found: true, Capabilities: caps})
	}
	for dev, vs := range values {
		for c, v := range vs {
			r.store.Changed(driver.Update{Device: types.NamespacedName{Namespace: ns, Name: dev}, Capability: c, Value: v})
		}
	}
	for _, rule := range rules {
		c, err := Compile(rule)
		if err != nil {
			t.Fatal(err)
		}
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
	idle := func() bool {
		r.e.mu.Lock()
		empty := len(r.e.rules) == 0 && len(r.e.arbs) == 0
		r.e.mu.Unlock()
		if len(r.e.events) > 0 || !empty && !r.e.earliest().IsZero() && !r.clock.HasWaiters() {
			return false
		}
		r.e.mu.Lock()
		defer r.e.mu.Unlock()
		now := r.clock.Now()
		if len(r.e.controlDue) > 0 {
			return false
		}
		for _, arbs := range r.e.arbs {
			for _, ar := range arbs {
				if ar.writing || !ar.next.IsZero() && !ar.next.After(now) {
					return false
				}
			}
		}
		for _, s := range r.e.rules {
			if s.hold != nil && (!s.hold.loaded || s.hold.writing || s.next.At.IsZero() || !s.next.At.After(now)) {
				return false
			}
		}
		return true
	}
	for range 500 {
		if idle() {
			time.Sleep(5 * time.Millisecond)
			if idle() {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatal("timed out waiting for the engine to settle")
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

func (r *rig) recordControl(dev types.NamespacedName, st []homev1alpha1.CapabilityControlStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.control == nil {
		r.control = map[string][]homev1alpha1.CapabilityControlStatus{}
	}
	r.control[dev.Name] = st
	r.reports++
}

func (r *rig) recordBudget(key types.NamespacedName, b BudgetReport) {
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

func (r *rig) recordSkipped(key types.NamespacedName, skipped []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.skipped == nil {
		r.skipped = map[string][]string{}
	}
	r.skipped[key.Name] = skipped
}

func (r *rig) change(dev, c string, v any) {
	r.t.Helper()
	r.store.Changed(driver.Update{Device: types.NamespacedName{Namespace: ns, Name: dev}, Capability: c, Value: v})
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

var heaterNight = `
  while: {window: {start: "22:00", end: "06:00"}}
  hold: {device: water-heater, capability: switch.on, value: true}
`

func testWindowHoldRestartRespectsATap(t *testing.T, states backend) {
	r := startRig(t, local("2026-10-21 21:59 BST"), states.holds(t), nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.expect()
	r.at("2026-10-21 22:00 BST")
	r.expect(call{"water-heater", capability.SwitchOn, true})
	if r.active("water-heater-night") != 1 {
		t.Error("oikade_rule_active isn't 1")
	}
	if s := r.sched.last(); !s.Hold || !s.Active || !s.Next.Equal(local("2026-10-22 06:00 BST")) {
		t.Errorf("reported %+v, want active until 06:00", s)
	}
	// A Home app tap turns it off at 23:00. A hold doesn't fight it.
	r.at("2026-10-21 23:00 BST")
	r.change("water-heater", capability.SwitchOn, false)
	r.stop()

	// The runtime restarts at 02:00. The hold is still active, as saved, so it writes nothing.
	r = startRig(t, local("2026-10-22 02:00 BST"), states.holds(t), nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.expect()
	r.at("2026-10-22 06:00 BST")
	r.expect(call{"water-heater", capability.SwitchOn, false})
}

func testWindowHoldStartsAfterDowntime(t *testing.T, states backend) {
	r := startRig(t, local("2026-10-21 21:50 BST"), states.holds(t), nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.stop()
	r.expect()

	// Down from 21:50 to 02:00: the hold started meanwhile, so it writes on.
	r = startRig(t, local("2026-10-22 02:00 BST"), states.holds(t), nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.expect(call{"water-heater", capability.SwitchOn, true})
}

func testOffDelaySurvivesARestart(t *testing.T, states backend) {
	fan := holdRule(t, "bathroom-humid", `
  while: {when: 'devices["bathroom-sensor"]["sensor.relative-humidity"] > 70', offDelay: 10m}
  hold: {device: bathroom-fan, capability: switch.on, value: true}
`)
	humidity := func(v float64) map[string]map[string]any {
		return map[string]map[string]any{"bathroom-sensor": {capability.Humidity: v}}
	}
	r := startRig(t, local("2026-10-21 08:00 BST"), states.holds(t), humidity(72), nil, fan)
	r.expect(call{"bathroom-fan", capability.SwitchOn, true})
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
	r.expect(call{"bathroom-fan", capability.SwitchOn, false})
}

func testMotionOffDelaySurvivesARestart(t *testing.T, states backend) {
	light := holdRule(t, "hall-motion-light", `
  while: {when: 'devices["hall-motion"]["sensor.occupancy-detected"]', offDelay: 5m}
  hold: {device: hall-light, capability: light.on, value: true}
`)
	motion := func(v bool) map[string]map[string]any {
		return map[string]map[string]any{"hall-motion": {capability.Occupancy: v}}
	}
	r := startRig(t, local("2026-10-21 20:00 BST"), states.holds(t), motion(true), nil, light)
	r.expect(call{"hall-light", capability.LightOn, true})
	r.at("2026-10-21 20:10 BST")
	r.change("hall-motion", capability.Occupancy, false)
	r.at("2026-10-21 20:12 BST")
	r.change("hall-motion", capability.Occupancy, true)
	r.at("2026-10-21 20:20 BST")
	r.change("hall-motion", capability.Occupancy, false)
	r.at("2026-10-21 20:22 BST")
	r.stop()

	r = startRig(t, local("2026-10-21 20:22 BST"), states.holds(t), motion(false), nil, light)
	r.at("2026-10-21 20:24 BST")
	r.expect()
	r.at("2026-10-21 20:25 BST")
	r.expect(call{"hall-light", capability.LightOn, false})
}

var overheat = `
  priority: Safety
  while: {when: 'devices["workshop-temp"]["sensor.temperature"] > 45', latch: true}
  hold: {device: workshop-heater, capability: switch.on, value: false}
`

func testLatchedHoldActsOnStartupUntilReleased(t *testing.T, states backend) {
	hot := map[string]map[string]any{"workshop-temp": {capability.Temperature: 50.0}}
	reset := holdRule(t, "workshop-overheat-reset", "  triggers: [{at: {time: \"09:00\"}}]\n  do: [{release: {rule: workshop-overheat}}]\n")
	reset.CreationTimestamp.Time = local("2026-10-21 08:00 BST")

	// It overheated while the runtime was down: the latch acts on startup.
	r := startRig(t, local("2026-10-21 08:00 BST"), states.holds(t), hot, nil, holdRule(t, "workshop-overheat", overheat), reset)
	r.expect(call{"workshop-heater", capability.SwitchOn, false})
	r.change("workshop-temp", capability.Temperature, 20.0)
	r.at("2026-10-21 08:30 BST")
	r.expect(call{"workshop-heater", capability.SwitchOn, false})
	if s := r.sched.last(); !s.Active || !s.Next.IsZero() {
		t.Errorf("reported %+v, want active with no next change", s)
	}
	r.stop()

	// A restart keeps the latch, though it has cooled down.
	cool := map[string]map[string]any{"workshop-temp": {capability.Temperature: 20.0}}
	r = startRig(t, local("2026-10-21 08:30 BST"), states.holds(t), cool, nil, holdRule(t, "workshop-overheat", overheat), reset)
	r.expect()
	r.at("2026-10-21 09:00 BST")
	r.expect(call{"workshop-heater", capability.SwitchOn, true})
	r.stop()

	// The release is saved.
	r = startRig(t, local("2026-10-21 09:05 BST"), states.holds(t), cool, nil, holdRule(t, "workshop-overheat", overheat))
	r.expect()
}

func TestWhenUnknownKeepsTheState(t *testing.T) {
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, nil, nil, holdRule(t, "workshop-overheat", overheat))
	r.expect()
	r.change("workshop-temp", capability.Temperature, 50.0)
	r.expect(call{"workshop-heater", capability.SwitchOn, false})
}

func TestNumberHoldWritesNothingWhenItEnds(t *testing.T) {
	r := startRig(t, local("2026-10-21 21:59 BST"), nil, nil, nil, holdRule(t, "hall-dim", `
  while: {window: {start: "22:00", end: "23:00"}}
  hold: {device: hall-light, capability: light.level, value: 30}
`))
	r.at("2026-10-21 22:00 BST")
	r.expect(call{"hall-light", capability.LightLevel, int64(30)})
	r.at("2026-10-21 23:00 BST")
	r.expect(call{"hall-light", capability.LightLevel, int64(30)})
	if r.active("hall-dim") != 0 {
		t.Error("oikade_rule_active isn't 0 after the hold ended")
	}
}

func TestEditingTheTargetEndsTheOldOne(t *testing.T) {
	states := newMemoryStates[HoldState]()
	r := startRig(t, local("2026-10-21 23:00 BST"), states, nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.expect(call{"water-heater", capability.SwitchOn, true})
	edited := holdRule(t, "water-heater-night", strings.ReplaceAll(heaterNight, "water-heater", "bathroom-fan"))
	edited.Generation = 2
	c, err := Compile(edited)
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	r.settle()
	r.expect(call{"water-heater", capability.SwitchOn, true}, call{"water-heater", capability.SwitchOn, false},
		call{"bathroom-fan", capability.SwitchOn, true})
}

func TestFailedHoldWriteRetries(t *testing.T) {
	states := newMemoryStates[HoldState]()
	r := startRig(t, local("2026-10-21 21:59 BST"), states, nil, nil, holdRule(t, "water-heater-night", heaterNight))
	r.setter.mu.Lock()
	r.setter.fail = map[string]bool{"water-heater": true}
	r.setter.mu.Unlock()
	r.at("2026-10-21 22:00 BST")
	if s, _ := states.Get(types.NamespacedName{Namespace: ns, Name: "water-heater-night"}); s.Active {
		t.Error("saved a failed write as active")
	}
	r.setter.mu.Lock()
	r.setter.fail = nil
	r.setter.mu.Unlock()
	r.at("2026-10-21 22:01 BST")
	r.expect(call{"water-heater", capability.SwitchOn, true}, call{"water-heater", capability.SwitchOn, true})
}

func TestRemoveForgetsTheHold(t *testing.T) {
	states := newMemoryStates[HoldState]()
	key := types.NamespacedName{Namespace: ns, Name: "water-heater-night"}
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
	compile := func(spec string) *window {
		c, err := Compile(holdRule(t, "w", spec+"\n  hold: {device: d, capability: switch.on, value: true}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return c.hold.window
	}
	night := compile(`  while: {window: {start: "22:00", end: "06:00", days: [Fri]}}`)
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
	allDay := compile(`  while: {window: {start: "07:00", end: "07:00"}}`)
	if in, _ := allDay.at(local("2026-10-23 03:00 BST"), london, nil); !in {
		t.Error("a window that ends when it starts isn't a whole day")
	}
}

func TestSunWindowFollowsTheDay(t *testing.T) {
	c, err := Compile(holdRule(t, "porch", `
  while: {window: {start: {sun: Sunset, offset: -15m}, end: "23:00"}}
  hold: {device: d, capability: switch.on, value: true}
`))
	if err != nil {
		t.Fatal(err)
	}
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
	metas := map[types.NamespacedName]hub.Meta{}
	for name := range holdDevices {
		metas[types.NamespacedName{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m}
	}
	for name, room := range rooms {
		metas[types.NamespacedName{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m, Room: room}
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
	c, err := Compile(holdRule(t, "porch-day", `
  while: {window: {start: "00:00", end: "00:00"}}
  hold: {room: porch, capability: light.on, value: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	r.settle()
	r.expect()
	r.store.Discovered(types.NamespacedName{Namespace: ns, Name: "porch-lamp"}, driver.Info{Found: true, Capabilities: []string{capability.LightOn}})
	r.at("2026-10-21 12:01 BST")
	r.expect(call{"porch-lamp", capability.LightOn, true})
}

func TestReleaseWhileWhenIsUnknown(t *testing.T) {
	hot := map[string]map[string]any{"workshop-temp": {capability.Temperature: 50.0}}
	r := startRig(t, local("2026-10-21 08:00 BST"), nil, hot, nil, holdRule(t, "workshop-overheat", overheat))
	r.expect(call{"workshop-heater", capability.SwitchOn, false})
	// The sensor drops out of the store, so when can't be evaluated.
	r.declareAlso(nil)
	r.store.SetDevices(z2m, map[types.NamespacedName]hub.Meta{{Namespace: ns, Name: "workshop-heater"}: {Instance: z2m}})
	r.store.Discovered(types.NamespacedName{Namespace: ns, Name: "workshop-heater"}, driver.Info{Found: true, Capabilities: []string{capability.SwitchOn}})
	r.e.release(types.NamespacedName{Namespace: ns, Name: "workshop-overheat"})
	r.settle()
	r.expect(call{"workshop-heater", capability.SwitchOn, false})
	// It's back, and cool: the released hold ends rather than latching again.
	r.declareAlso(nil)
	r.change("workshop-temp", capability.Temperature, 20.0)
	r.expect(call{"workshop-heater", capability.SwitchOn, false}, call{"workshop-heater", capability.SwitchOn, true})
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
	edited := holdRule(t, "water-heater-night", strings.ReplaceAll(heaterNight, "water-heater", "bathroom-fan"))
	edited.Generation = 2
	c, err := Compile(edited)
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
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

func TestCompileBudgetHold(t *testing.T) {
	// The rest of a budget is checked against its target's control entry, by schedule.Compile.
	for spec, want := range map[string]string{
		`
  while: {budget: {minutes: {expression: "120"}}, when: "true"}
  hold: {device: pump, capability: switch.on, value: true}`: "budget must be the only field",
	} {
		if _, err := Compile(holdRule(t, "x", spec)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%s) = %v, want %q", spec, err, want)
		}
	}
}
