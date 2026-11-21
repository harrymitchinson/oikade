package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

func set(device, name, value string) Action {
	return Set{Device: device, Capability: name, Value: parsed(value)}
}

// parsed is a JSON value as Go.
func parsed(value string) any {
	var v any
	_ = json.Unmarshal([]byte(value), &v)
	return v
}

func delay(d time.Duration) Action { return Delay(d) }

// steps is a flow written as the actions it applies in order, as a home's flow does: a step that
// fails is logged and the run carries on, and then ends as failed.
type steps struct {
	Trigger
	// Mode defaults to ModeSingle.
	Mode Mode
	Do   []Action
}

// engineKey is the context key of the engine a steps flow applies its actions to: the one whose
// Run got the context, by withEngine.
type engineKey struct{}

func withEngine(ctx context.Context, e *Engine) context.Context {
	return context.WithValue(ctx, engineKey{}, e)
}

// body applies the steps to the engine in ctx.
func (s steps) body(key ident.ID) Body {
	return func(ctx context.Context) string {
		e := ctx.Value(engineKey{}).(*Engine)
		failed := false
		for _, a := range s.Do {
			err := e.Apply(ctx, key, a)
			switch {
			case ctx.Err() != nil:
				return ResultCancelled
			case err != nil:
				e.Log.Error("Failed step of flow run", "err", err, "rule", key)
				failed = true
			}
		}
		if failed {
			return ResultFailed
		}
		return ResultSucceeded
	}
}

func flowRule(t *testing.T, name string, s steps) *Compiled {
	t.Helper()
	key := ident.ID{Name: name}
	c, err := NewFlow(key, Flow{Trigger: s.Trigger, Mode: s.Mode, Body: s.body(key)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func holdRule(t *testing.T, name string, h Hold) *Compiled {
	t.Helper()
	c, err := NewHold(ident.ID{Name: name}, h)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// reading is a device's value, as a condition sees it. Like a condition reading a device that
// hasn't reported, it's an error until there is one.
func reading(v Vars, device, name string) (any, error) {
	devices, _ := v["devices"].(map[string]map[string]any)
	value, ok := devices[device][name]
	if !ok {
		return nil, fmt.Errorf("%s %s hasn't reported", device, name)
	}
	return value, nil
}

func isTrue(device, name string) Condition {
	return func(v Vars) (bool, error) {
		value, err := reading(v, device, name)
		b, _ := value.(bool)
		return b, err
	}
}

func above(device, name string, limit float64) Condition {
	return func(v Vars) (bool, error) {
		value, err := reading(v, device, name)
		f, _ := value.(float64)
		return f > limit, err
	}
}

func below(device, name string, limit float64) Condition {
	return func(v Vars) (bool, error) {
		value, err := reading(v, device, name)
		f, _ := value.(float64)
		return f < limit, err
	}
}

// triggered is what fired the flow.
func triggered(v Vars) map[string]any {
	m, _ := v["trigger"].(map[string]any)
	return m
}

// triggerValue holds when the change that fired was to true.
func triggerValue(v Vars) (bool, error) {
	b, _ := triggered(v)["value"].(bool)
	return b, nil
}

// eveningMotion holds for motion from 18:00, local time.
func eveningMotion(v Vars) (bool, error) {
	on, _ := triggerValue(v)
	tz, err := time.LoadLocation(v["tz"].(string))
	if err != nil {
		return false, err
	}
	return on && v["now"].(time.Time).In(tz).Hour() >= 18, nil
}

// timerFired holds when a timer fired, and not a change.
func timerFired(v Vars) (bool, error) {
	typ := triggered(v)[triggerType]
	return typ == TypeAt || typ == TypeSun, nil
}

// hallwaySensor holds for the hallway's two motion sensors.
func hallwaySensor(v Vars) (bool, error) {
	device := triggered(v)["device"]
	return device == "hallway-motion" || device == "landing-motion", nil
}

var motionTrigger = []Change{{Device: "hallway-motion", Capability: capability.Occupied}}

// noop is a flow's body that does nothing, for a test of its trigger.
func noop(context.Context) string { return ResultSucceeded }

func TestApplyChecksItsActions(t *testing.T) {
	for name, tc := range map[string]struct {
		do      Action
		wantErr string
	}{
		"read-only set": {set("x", capability.Temperature, "20"), "read-only"},
		"wrong kind":    {set("x", capability.On, `"ON"`), "takes a bool"},
		"out of range":  {set("x", capability.Brightness, "150"), "between 0 and 100"},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Engine{Store: hub.NewStore(logging.Discard()), Setter: &fakeSetter{}, Log: logging.Discard()}
			e.Init()
			err := e.Apply(t.Context(), goKey("a"), tc.do)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
	unknown := Trigger{Changes: []Change{{Device: "x", Capability: "made.up"}}}
	if _, err := NewFlow(goKey("a"), Flow{Trigger: unknown, Body: noop}); err == nil || !strings.Contains(err.Error(), "changes[0]") {
		t.Fatalf("error = %v, want it to name the change", err)
	}
}

func TestRoomSetsAndChangesAreChecked(t *testing.T) {
	a, err := roomLightOn("living-room", "false").action()
	if want := (action{room: "living-room", capability: capability.On, value: false}); err != nil || !reflect.DeepEqual(a, want) {
		t.Fatalf("action = %+v, %v, want %+v", a, err, want)
	}

	for name, tc := range map[string]struct {
		err     error
		wantErr string
	}{
		"trigger with neither": {
			flowErr(Trigger{Changes: []Change{{Capability: capability.Occupied}}}),
			"changes[0]: exactly one of device or room is required",
		},
		"set with neither": {
			actionErr(set("", capability.On, "true")),
			"exactly one of device or room is required",
		},
		"trigger with both": {
			flowErr(Trigger{Changes: []Change{{Device: "m", Room: "hallway", Capability: capability.Occupied}}}),
			"changes[0]: exactly one of device or room is required",
		},
		"set with both": {
			actionErr(Set{Device: "lamp", Room: "hallway", Capability: capability.On, Value: true}),
			"exactly one of device or room is required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.err == nil || !strings.Contains(tc.err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", tc.err, tc.wantErr)
			}
		})
	}
}

// flowErr is the error compiling a flow with this trigger.
func flowErr(tr Trigger) error {
	_, err := NewFlow(goKey("a"), Flow{Trigger: tr, Body: noop})
	return err
}

// actionErr is the error Apply gets checking a.
func actionErr(a Action) error {
	_, err := a.action()
	return err
}

type call struct {
	device, capability string
	value              any
}

type fakeSetter struct {
	mu    sync.Mutex
	calls []call
	// by is whose write each call's context said it was.
	by  []string
	err error
	// fail names devices whose Set fails.
	fail map[string]bool
	// before, when set, runs as each Set arrives.
	before func()
}

func (f *fakeSetter) Set(ctx context.Context, dev ident.ID, c string, v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{dev.Name, c, v})
	f.by = append(f.by, driver.Writer(ctx))
	if f.before != nil {
		f.before()
	}
	if f.fail[dev.Name] {
		return errors.New("not responding")
	}
	return f.err
}

func (f *fakeSetter) writers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.by)
}

func (f *fakeSetter) got() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

type harness struct {
	t      *testing.T
	e      *Engine
	clock  *clock.Fake
	setter *fakeSetter
}

func newHarness(t *testing.T, now time.Time, rules ...*Compiled) *harness {
	t.Helper()
	return newHarnessWith(t, now, nil, rules...)
}

// newHarnessWith is newHarness with a hook to configure the engine before its first Load.
func newHarnessWith(t *testing.T, now time.Time, configure func(*Engine), rules ...*Compiled) *harness {
	t.Helper()
	store := hub.NewStore(logging.Discard())
	store.SetDevices(z2m, map[ident.ID]hub.Meta{
		{Name: "hallway-motion"}: {},
		{Name: "hallway-light"}:  {},
	})
	h := &harness{t: t, clock: clock.NewFake(now), setter: &fakeSetter{}}
	h.e = &Engine{
		Store: store, Setter: h.setter, Clock: h.clock, Location: london, Log: logging.Discard(),
		RuleMetrics: metrics.NewSet(),
	}
	h.e.Init()
	if configure != nil {
		configure(h.e)
	}
	for _, c := range rules {
		h.e.Load(c)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { h.e.Run(withEngine(ctx, h.e)); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return h
}

func (h *harness) motion(v bool) {
	h.e.Changed(driver.Update{
		Device: ident.ID{Name: "hallway-motion"}, Capability: capability.Occupied, Value: v,
	})
}

func (h *harness) runs(result string) float64 {
	return float64(h.e.metrics.runs.Counter("flow/motion-light", result).Get())
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	testkit.Eventually(h.t, what, cond)
}

// settle waits for the engine to catch up with the clock.
func (h *harness) settle() {
	h.t.Helper()
	waitIdle(h.t, h.e)
}

func (h *harness) expectCalls(want ...call) {
	h.t.Helper()
	h.eventually(fmt.Sprintf("%d calls", len(want)), func() bool { return len(h.setter.got()) >= len(want) })
	got := h.setter.got()
	if len(got) != len(want) {
		h.t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			h.t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}

func TestConditionUsesLocalTime(t *testing.T) {
	a := flowRule(t, "motion-light", steps{Changes: motionTrigger, Condition: eveningMotion, Do: []Action{set("hallway-light", capability.On, "true")}})
	// 17:30 UTC is 18:30 in London during BST.
	h := newHarness(t, time.Date(2026, 6, 1, 17, 30, 0, 0, time.UTC), a)

	h.motion(false)
	h.eventually("condition_false", func() bool { return h.runs(ResultConditionFalse) == 1 })
	h.motion(true)
	h.expectCalls(call{"hallway-light", capability.On, true})
	h.eventually("succeeded", func() bool { return h.runs(ResultSucceeded) == 1 })
}

func TestRestartKeepsLightOnUntilMotionStops(t *testing.T) {
	a := flowRule(t, "motion-light", steps{Changes: motionTrigger, Condition: triggerValue, Mode: ModeRestart, Do: []Action{
		set("hallway-light", capability.On, "true"),
		delay(5 * time.Minute),
		set("hallway-light", capability.On, "false"),
	}})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	on, off := call{"hallway-light", capability.On, true}, call{"hallway-light", capability.On, false}

	h.motion(true)
	h.expectCalls(on)
	h.eventually("delay to start", h.clock.HasWaiters)
	h.clock.Step(3 * time.Minute)

	// More motion restarts the countdown.
	h.motion(true)
	h.expectCalls(on, on)
	h.eventually("first run cancelled", func() bool { return h.runs(ResultCancelled) == 1 })
	h.eventually("second delay to start", h.clock.HasWaiters)
	h.clock.Step(3 * time.Minute)
	h.settle()
	if got := len(h.setter.got()); got != 2 {
		t.Fatalf("light turned off early: %v", h.setter.got())
	}

	h.clock.Step(2 * time.Minute)
	h.expectCalls(on, on, off)
	h.eventually("succeeded", func() bool { return h.runs(ResultSucceeded) == 1 })
}

func TestSingleIgnoresTriggersWhileRunning(t *testing.T) {
	a := flowRule(t, "motion-light", steps{Changes: motionTrigger, Do: []Action{delay(time.Minute), set("hallway-light", capability.On, "true")}})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)

	h.motion(true)
	h.eventually("delay to start", h.clock.HasWaiters)
	h.motion(false)
	h.eventually("busy", func() bool { return h.runs(ResultBusy) == 1 })
	h.clock.Step(time.Minute)
	h.expectCalls(call{"hallway-light", capability.On, true})
}

// A set that fails is Apply's error, and the run that made it counts as failed.
func TestAFailedSetFailsTheRun(t *testing.T) {
	a := flowRule(t, "motion-light", steps{Changes: motionTrigger, Do: []Action{
		set("hallway-light", capability.On, "true"),
		set("hallway-light", capability.Brightness, "40"),
	}})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	h.setter.err = errors.New("not paired")

	h.motion(true)
	h.eventually("failed", func() bool { return h.runs(ResultFailed) == 1 })
	h.expectCalls(call{"hallway-light", capability.On, true}, call{"hallway-light", capability.Brightness, 40.0})
}

func TestReloadingTheSameGenerationKeepsTheRun(t *testing.T) {
	a := func() *Compiled {
		return flowRule(t, "motion-light", steps{
			Changes: motionTrigger,
			Do:      []Action{delay(time.Minute), set("hallway-light", capability.On, "true")},
		})
	}
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a())
	h.motion(true)
	h.eventually("delay to start", h.clock.HasWaiters)

	h.e.Load(a())
	h.clock.Step(time.Minute)
	h.expectCalls(call{"hallway-light", capability.On, true})

	edited := a()
	edited.Generation = 2
	h.motion(false)
	h.eventually("second delay to start", h.clock.HasWaiters)
	h.e.Load(edited)
	h.eventually("cancelled by reload", func() bool { return h.runs(ResultCancelled) == 1 })
}

// roomLightOn sets on on every light in a room.
func roomLightOn(room, value string) Action {
	return Set{Room: room, Capability: capability.On, Value: parsed(value)}
}

// member is a declared Device: its room, and its capabilities once discovered (nil if not yet).
type member struct {
	room string
	caps []string
}

var lampCaps = []string{capability.On, capability.Brightness, capability.LinkQuality}

// z2m is the driver instance every test device belongs to.
var z2m = driver.InstanceKey{Driver: driver.Zigbee2MQTT, Name: "z2m"}

// declare replaces the store's devices with these.
func declare(store *hub.Store, devices map[string]member) {
	m := map[ident.ID]hub.Meta{}
	for name, d := range devices {
		m[ident.ID{Name: name}] = hub.Meta{Room: d.room}
	}
	store.SetDevices(z2m, m)
	for name, d := range devices {
		if d.caps != nil {
			store.Discovered(ident.ID{Name: name}, driver.Info{Found: true, Capabilities: d.caps})
		}
	}
}

// runRoom runs a flow's steps once against these devices, and returns the result and the log
// lines.
func runRoom(t *testing.T, setter *fakeSetter, devices map[string]member, actions ...Action) (string, []string) {
	t.Helper()
	logs := testkit.Logs(t)
	e := &Engine{Store: hub.NewStore(logging.Discard()), Setter: setter, Log: logs.Logger}
	e.Init()
	declare(e.Store, devices)
	body := steps{Do: actions}.body(ident.ID{Name: "goodnight"})
	return body(withEngine(t.Context(), e)), logs.Lines()
}

func TestRoomTriggerMatches(t *testing.T) {
	a := flowRule(t, "motion-light", steps{Changes: []Change{{Room: "hallway", Capability: capability.Occupied}}, Condition: hallwaySensor, Do: []Action{set("hallway-light", capability.On, "true")}})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	declare(h.e.Store, map[string]member{
		"hallway-motion": {room: "hallway"},
		"landing-motion": {room: "hallway"},
		"kitchen-motion": {room: "kitchen"},
		"loft-motion":    {},
	})
	change := func(device string) {
		h.e.Changed(driver.Update{Device: ident.ID{Name: device}, Capability: capability.Occupied, Value: true})
	}
	on := call{"hallway-light", capability.On, true}

	// Changes from another room or from a device with no room don't match; changes are handled in
	// order, so the first call proves they were skipped.
	change("kitchen-motion")
	change("loft-motion")
	change("hallway-motion")
	h.expectCalls(on)
	h.eventually("first run", func() bool { return h.runs(ResultSucceeded) == 1 })

	// trigger.device is the sensor that changed, so the condition holds for the second sensor too.
	change("landing-motion")
	h.expectCalls(on, on)
	h.eventually("second run", func() bool { return h.runs(ResultSucceeded) == 2 })
	if n := h.runs(ResultConditionFalse); n != 0 {
		t.Fatalf("condition_false = %v, want 0", n)
	}
}

func TestRoomActionSetsMembers(t *testing.T) {
	setter := &fakeSetter{}
	result, _ := runRoom(t, setter, map[string]member{
		"lamp-b":       {room: "living-room", caps: lampCaps},
		"lamp-a":       {room: "living-room", caps: lampCaps},
		"motion":       {room: "living-room", caps: []string{capability.Occupied}},
		"lamp-new":     {room: "living-room"},
		"kitchen-lamp": {room: "kitchen", caps: lampCaps},
	}, roomLightOn("living-room", "false"))

	if result != ResultSucceeded {
		t.Fatalf("result = %s, want %s", result, ResultSucceeded)
	}
	want := []call{{"lamp-a", capability.On, false}, {"lamp-b", capability.On, false}}
	if got := setter.got(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestRoomActionEmptyMatch(t *testing.T) {
	setter := &fakeSetter{}
	result, logs := runRoom(t, setter, map[string]member{
		"motion": {room: "garage", caps: []string{capability.Occupied}},
	}, roomLightOn("garage", "true"), set("hallway-light", capability.On, "true"))

	if result != ResultSucceeded {
		t.Fatalf("result = %s, want %s", result, ResultSucceeded)
	}
	if got := setter.got(); !slices.Equal(got, []call{{"hallway-light", capability.On, true}}) {
		t.Fatalf("calls = %v, want only the device action", got)
	}
	if !slices.ContainsFunc(logs, func(l string) bool {
		return strings.Contains(l, `level=DEBUG`) && strings.Contains(l, `msg="Matched no Devices in Room"`) && strings.Contains(l, `room=garage`)
	}) {
		t.Fatalf("no debug empty-match log in %q", logs)
	}
}

func TestRoomActionPartialFailure(t *testing.T) {
	setter := &fakeSetter{fail: map[string]bool{"lamp-a": true}}
	result, logs := runRoom(t, setter, map[string]member{
		"lamp-a": {room: "living-room", caps: lampCaps},
		"lamp-b": {room: "living-room", caps: lampCaps},
	}, roomLightOn("living-room", "false"), set("hallway-light", capability.On, "false"))

	if result != ResultFailed {
		t.Fatalf("result = %s, want %s", result, ResultFailed)
	}
	want := []call{{"lamp-a", capability.On, false}, {"lamp-b", capability.On, false}, {"hallway-light", capability.On, false}}
	if got := setter.got(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if !slices.ContainsFunc(logs, func(l string) bool {
		return strings.Contains(l, `device=lamp-a`) && strings.Contains(l, `room=living-room`)
	}) {
		t.Fatalf("no failure log naming lamp-a in %q", logs)
	}
}

func TestRoomMembershipAtExecution(t *testing.T) {
	a := flowRule(t, "motion-light", steps{Changes: motionTrigger, Do: []Action{
		roomLightOn("hallway", "true"),
		delay(5 * time.Minute),
		roomLightOn("hallway", "false"),
	}})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	devices := map[string]member{
		"hallway-motion": {room: "hallway", caps: []string{capability.Occupied}},
		"hallway-light":  {room: "hallway", caps: lampCaps},
	}
	declare(h.e.Store, devices)

	h.motion(true)
	h.expectCalls(call{"hallway-light", capability.On, true})
	h.eventually("delay to start", h.clock.HasWaiters)

	// A lamp joins the hallway during the delay.
	devices["hallway-lamp"] = member{room: "hallway", caps: lampCaps}
	declare(h.e.Store, devices)
	h.clock.Step(5 * time.Minute)

	h.expectCalls(
		call{"hallway-light", capability.On, true},
		call{"hallway-lamp", capability.On, false},
		call{"hallway-light", capability.On, false},
	)
	h.eventually("succeeded", func() bool { return h.runs(ResultSucceeded) == 1 })
}
