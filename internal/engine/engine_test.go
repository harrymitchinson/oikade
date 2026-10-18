/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
	"github.com/harry/oikade/internal/capability"
	"github.com/harry/oikade/internal/driver"
	"github.com/harry/oikade/internal/hub"
)

const ns = "home"

func set(device, name, value string) homev1alpha1.Action {
	return homev1alpha1.Action{Set: &homev1alpha1.SetAction{
		Device: device, Capability: name, Value: apiextensionsv1.JSON{Raw: []byte(value)},
	}}
}

func delay(d time.Duration) homev1alpha1.Action {
	return homev1alpha1.Action{Delay: &metav1.Duration{Duration: d}}
}

func automation(name string, spec homev1alpha1.AutomationSpec) *homev1alpha1.Automation {
	a := &homev1alpha1.Automation{Spec: spec}
	a.Name, a.Namespace, a.Generation = name, ns, 1
	return a
}

var motionTrigger = []homev1alpha1.Trigger{{Device: "hallway-motion", Capability: capability.Occupancy}}

func TestCompile(t *testing.T) {
	for name, tc := range map[string]struct {
		spec    homev1alpha1.AutomationSpec
		wantErr string
	}{
		"valid": {spec: homev1alpha1.AutomationSpec{
			Triggers:  motionTrigger,
			Condition: `trigger.value && devices["front-door"]["sensor.contact-open"] == false`,
			Actions:   []homev1alpha1.Action{set("hallway-light", capability.LightLevel, "40"), delay(time.Minute)},
		}},
		"syntax error":     {spec: homev1alpha1.AutomationSpec{Triggers: motionTrigger, Condition: "trigger.value &&", Actions: []homev1alpha1.Action{delay(time.Second)}}, wantErr: "condition"},
		"non-bool":         {spec: homev1alpha1.AutomationSpec{Triggers: motionTrigger, Condition: `"yes"`, Actions: []homev1alpha1.Action{delay(time.Second)}}, wantErr: "must evaluate to a bool"},
		"unknown variable": {spec: homev1alpha1.AutomationSpec{Triggers: motionTrigger, Condition: "lights_on", Actions: []homev1alpha1.Action{delay(time.Second)}}, wantErr: "undeclared reference"},
		"unknown trigger":  {spec: homev1alpha1.AutomationSpec{Triggers: []homev1alpha1.Trigger{{Device: "x", Capability: "made.up"}}, Actions: []homev1alpha1.Action{delay(time.Second)}}, wantErr: "triggers[0]"},
		"read-only set":    {spec: homev1alpha1.AutomationSpec{Triggers: motionTrigger, Actions: []homev1alpha1.Action{set("x", capability.Temperature, "20")}}, wantErr: "read-only"},
		"wrong kind":       {spec: homev1alpha1.AutomationSpec{Triggers: motionTrigger, Actions: []homev1alpha1.Action{set("x", capability.LightOn, `"ON"`)}}, wantErr: "takes a bool"},
		"out of range":     {spec: homev1alpha1.AutomationSpec{Triggers: motionTrigger, Actions: []homev1alpha1.Action{set("x", capability.LightLevel, "150")}}, wantErr: "between 0 and 100"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Compile(automation("a", tc.spec))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCompileRoom(t *testing.T) {
	c, err := Compile(automation("goodnight", homev1alpha1.AutomationSpec{
		Triggers: []homev1alpha1.Trigger{{Room: "hallway", Capability: capability.Occupancy}},
		Actions:  []homev1alpha1.Action{roomLightOn("living-room", "false")},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (action{room: "living-room", capability: capability.LightOn, value: false}); c.actions[0] != want {
		t.Fatalf("action = %+v, want %+v", c.actions[0], want)
	}

	for name, tc := range map[string]struct {
		spec    homev1alpha1.AutomationSpec
		wantErr string
	}{
		"trigger with neither": {
			spec:    homev1alpha1.AutomationSpec{Triggers: []homev1alpha1.Trigger{{Capability: capability.Occupancy}}, Actions: []homev1alpha1.Action{delay(time.Second)}},
			wantErr: "triggers[0]: exactly one of device or room is required",
		},
		"set with neither": {
			spec:    homev1alpha1.AutomationSpec{Triggers: motionTrigger, Actions: []homev1alpha1.Action{set("", capability.LightOn, "true")}},
			wantErr: "actions[0]: exactly one of device or room is required",
		},
		"trigger with both": {
			spec:    homev1alpha1.AutomationSpec{Triggers: []homev1alpha1.Trigger{{Device: "m", Room: "hallway", Capability: capability.Occupancy}}, Actions: []homev1alpha1.Action{delay(time.Second)}},
			wantErr: "triggers[0]: exactly one of device or room is required",
		},
		"set with both": {
			spec: homev1alpha1.AutomationSpec{Triggers: motionTrigger, Actions: []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
				Device: "lamp", Room: "hallway", Capability: capability.LightOn, Value: apiextensionsv1.JSON{Raw: []byte("true")},
			}}}},
			wantErr: "actions[0]: exactly one of device or room is required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Compile(automation("a", tc.spec))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

type call struct {
	device, capability string
	value              any
}

type fakeSetter struct {
	mu    sync.Mutex
	calls []call
	err   error
	// fail names devices whose Set fails.
	fail map[string]bool
}

func (f *fakeSetter) Set(_ context.Context, dev types.NamespacedName, c string, v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{dev.Name, c, v})
	if f.fail[dev.Name] {
		return errors.New("not responding")
	}
	return f.err
}

func (f *fakeSetter) got() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

type harness struct {
	t      *testing.T
	e      *Engine
	clock  *clocktesting.FakeClock
	setter *fakeSetter
}

func newHarness(t *testing.T, now time.Time, automations ...*homev1alpha1.Automation) *harness {
	t.Helper()
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	store := hub.NewStore(logr.Discard())
	store.SetDevices(types.NamespacedName{Namespace: ns, Name: "z2m"}, map[types.NamespacedName]hub.Meta{
		{Namespace: ns, Name: "hallway-motion"}: {},
		{Namespace: ns, Name: "hallway-light"}:  {},
	})
	h := &harness{t: t, clock: clocktesting.NewFakeClock(now), setter: &fakeSetter{}}
	h.e = &Engine{
		Store: store, Setter: h.setter, Clock: h.clock, Location: london, Log: logr.Discard(),
		Runs: NewRunsCounter(),
	}
	h.e.Init()
	for _, a := range automations {
		c, err := Compile(a)
		if err != nil {
			t.Fatal(err)
		}
		h.e.Load(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.e.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return h
}

func (h *harness) motion(v bool) {
	h.e.Changed(driver.Update{
		Device: types.NamespacedName{Namespace: ns, Name: "hallway-motion"}, Capability: capability.Occupancy, Value: v,
	})
}

func (h *harness) runs(result string) float64 {
	return testutil.ToFloat64(h.e.Runs.WithLabelValues(ns, "motion-light", result))
}

// eventually polls cond for up to two seconds.
func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
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
	a := automation("motion-light", homev1alpha1.AutomationSpec{
		Triggers:  motionTrigger,
		Condition: "trigger.value && now.getHours(tz) >= 18",
		Actions:   []homev1alpha1.Action{set("hallway-light", capability.LightOn, "true")},
	})
	// 17:30 UTC is 18:30 in London during BST.
	h := newHarness(t, time.Date(2026, 6, 1, 17, 30, 0, 0, time.UTC), a)

	h.motion(false)
	h.eventually("condition_false", func() bool { return h.runs(ResultConditionFalse) == 1 })
	h.motion(true)
	h.expectCalls(call{"hallway-light", capability.LightOn, true})
	h.eventually("succeeded", func() bool { return h.runs(ResultSucceeded) == 1 })
}

func TestRestartKeepsLightOnUntilMotionStops(t *testing.T) {
	a := automation("motion-light", homev1alpha1.AutomationSpec{
		Triggers:  motionTrigger,
		Condition: "trigger.value",
		Mode:      homev1alpha1.AutomationModeRestart,
		Actions: []homev1alpha1.Action{
			set("hallway-light", capability.LightOn, "true"),
			delay(5 * time.Minute),
			set("hallway-light", capability.LightOn, "false"),
		},
	})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	on, off := call{"hallway-light", capability.LightOn, true}, call{"hallway-light", capability.LightOn, false}

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
	time.Sleep(50 * time.Millisecond)
	if got := len(h.setter.got()); got != 2 {
		t.Fatalf("light turned off early: %v", h.setter.got())
	}

	h.clock.Step(2 * time.Minute)
	h.expectCalls(on, on, off)
	h.eventually("succeeded", func() bool { return h.runs(ResultSucceeded) == 1 })
}

func TestSingleIgnoresTriggersWhileRunning(t *testing.T) {
	a := automation("motion-light", homev1alpha1.AutomationSpec{
		Triggers: motionTrigger,
		Actions:  []homev1alpha1.Action{delay(time.Minute), set("hallway-light", capability.LightOn, "true")},
	})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)

	h.motion(true)
	h.eventually("delay to start", h.clock.HasWaiters)
	h.motion(false)
	h.eventually("busy", func() bool { return h.runs(ResultBusy) == 1 })
	h.clock.Step(time.Minute)
	h.expectCalls(call{"hallway-light", capability.LightOn, true})
}

func TestFailedActionStopsTheRun(t *testing.T) {
	a := automation("motion-light", homev1alpha1.AutomationSpec{
		Triggers: motionTrigger,
		Actions: []homev1alpha1.Action{
			set("hallway-light", capability.LightOn, "true"),
			set("hallway-light", capability.LightLevel, "40"),
		},
	})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	h.setter.err = errors.New("not paired")

	h.motion(true)
	h.eventually("failed", func() bool { return h.runs(ResultFailed) == 1 })
	h.expectCalls(call{"hallway-light", capability.LightOn, true})
}

func TestReloadingTheSameGenerationKeepsTheRun(t *testing.T) {
	a := automation("motion-light", homev1alpha1.AutomationSpec{
		Triggers: motionTrigger,
		Actions:  []homev1alpha1.Action{delay(time.Minute), set("hallway-light", capability.LightOn, "true")},
	})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	h.motion(true)
	h.eventually("delay to start", h.clock.HasWaiters)

	c, _ := Compile(a)
	h.e.Load(c)
	h.clock.Step(time.Minute)
	h.expectCalls(call{"hallway-light", capability.LightOn, true})

	a.Generation = 2
	c, _ = Compile(a)
	h.motion(false)
	h.eventually("second delay to start", h.clock.HasWaiters)
	h.e.Load(c)
	h.eventually("cancelled by reload", func() bool { return h.runs(ResultCancelled) == 1 })
}

// roomLightOn sets light.on on every light in a room.
func roomLightOn(room, value string) homev1alpha1.Action {
	return homev1alpha1.Action{Set: &homev1alpha1.SetAction{
		Room: room, Capability: capability.LightOn, Value: apiextensionsv1.JSON{Raw: []byte(value)},
	}}
}

// member is a declared Device: its room, and its capabilities once discovered (nil if not yet).
type member struct {
	room string
	caps []string
}

var lampCaps = []string{capability.LightOn, capability.LightLevel, capability.LinkQuality}

// declare replaces the store's devices with these.
func declare(store *hub.Store, devices map[string]member) {
	m := map[types.NamespacedName]hub.Meta{}
	for name, d := range devices {
		m[types.NamespacedName{Namespace: ns, Name: name}] = hub.Meta{Room: d.room}
	}
	store.SetDevices(types.NamespacedName{Namespace: ns, Name: "z2m"}, m)
	for name, d := range devices {
		if d.caps != nil {
			store.Discovered(types.NamespacedName{Namespace: ns, Name: name}, driver.Info{Found: true, Capabilities: d.caps})
		}
	}
}

// runRoom executes an Automation's actions once against these devices, and returns the result
// and the log lines.
func runRoom(t *testing.T, setter *fakeSetter, devices map[string]member, actions ...homev1alpha1.Action) (string, []string) {
	t.Helper()
	c, err := Compile(automation("goodnight", homev1alpha1.AutomationSpec{Triggers: motionTrigger, Actions: actions}))
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	e := &Engine{
		Store: hub.NewStore(logr.Discard()), Setter: setter, Log: funcr.New(func(_, args string) {
			logs = append(logs, args)
		}, funcr.Options{Verbosity: 1}),
	}
	e.Init()
	declare(e.Store, devices)
	return e.execute(context.Background(), c.Key, c), logs
}

func TestRoomTriggerMatches(t *testing.T) {
	a := automation("motion-light", homev1alpha1.AutomationSpec{
		Triggers:  []homev1alpha1.Trigger{{Room: "hallway", Capability: capability.Occupancy}},
		Condition: `trigger.device in ["hallway-motion", "landing-motion"]`,
		Actions:   []homev1alpha1.Action{set("hallway-light", capability.LightOn, "true")},
	})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	declare(h.e.Store, map[string]member{
		"hallway-motion": {room: "hallway"},
		"landing-motion": {room: "hallway"},
		"kitchen-motion": {room: "kitchen"},
		"loft-motion":    {},
	})
	change := func(device string) {
		h.e.Changed(driver.Update{Device: types.NamespacedName{Namespace: ns, Name: device}, Capability: capability.Occupancy, Value: true})
	}
	on := call{"hallway-light", capability.LightOn, true}

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
		"motion":       {room: "living-room", caps: []string{capability.Occupancy}},
		"lamp-new":     {room: "living-room"},
		"kitchen-lamp": {room: "kitchen", caps: lampCaps},
	}, roomLightOn("living-room", "false"))

	if result != ResultSucceeded {
		t.Fatalf("result = %s, want %s", result, ResultSucceeded)
	}
	want := []call{{"lamp-a", capability.LightOn, false}, {"lamp-b", capability.LightOn, false}}
	if got := setter.got(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestRoomActionEmptyMatch(t *testing.T) {
	setter := &fakeSetter{}
	result, logs := runRoom(t, setter, map[string]member{
		"motion": {room: "garage", caps: []string{capability.Occupancy}},
	}, roomLightOn("garage", "true"), set("hallway-light", capability.LightOn, "true"))

	if result != ResultSucceeded {
		t.Fatalf("result = %s, want %s", result, ResultSucceeded)
	}
	if got := setter.got(); !slices.Equal(got, []call{{"hallway-light", capability.LightOn, true}}) {
		t.Fatalf("calls = %v, want only the device action", got)
	}
	if !slices.ContainsFunc(logs, func(l string) bool {
		return strings.Contains(l, `"level"=1`) && strings.Contains(l, `"msg"="Matched no Devices in Room"`) && strings.Contains(l, `"room"="garage"`)
	}) {
		t.Fatalf("no V(1) empty-match log in %q", logs)
	}
}

func TestRoomActionPartialFailure(t *testing.T) {
	setter := &fakeSetter{fail: map[string]bool{"lamp-a": true}}
	result, logs := runRoom(t, setter, map[string]member{
		"lamp-a": {room: "living-room", caps: lampCaps},
		"lamp-b": {room: "living-room", caps: lampCaps},
	}, roomLightOn("living-room", "false"), set("hallway-light", capability.LightOn, "false"))

	if result != ResultFailed {
		t.Fatalf("result = %s, want %s", result, ResultFailed)
	}
	want := []call{{"lamp-a", capability.LightOn, false}, {"lamp-b", capability.LightOn, false}, {"hallway-light", capability.LightOn, false}}
	if got := setter.got(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if !slices.ContainsFunc(logs, func(l string) bool {
		return strings.Contains(l, `"device"="lamp-a"`) && strings.Contains(l, `"room"="living-room"`)
	}) {
		t.Fatalf("no failure log naming lamp-a in %q", logs)
	}
}

func TestRoomMembershipAtExecution(t *testing.T) {
	a := automation("motion-light", homev1alpha1.AutomationSpec{
		Triggers: motionTrigger,
		Actions: []homev1alpha1.Action{
			roomLightOn("hallway", "true"),
			delay(5 * time.Minute),
			roomLightOn("hallway", "false"),
		},
	})
	h := newHarness(t, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), a)
	devices := map[string]member{
		"hallway-motion": {room: "hallway", caps: []string{capability.Occupancy}},
		"hallway-light":  {room: "hallway", caps: lampCaps},
	}
	declare(h.e.Store, devices)

	h.motion(true)
	h.expectCalls(call{"hallway-light", capability.LightOn, true})
	h.eventually("delay to start", h.clock.HasWaiters)

	// A lamp joins the hallway during the delay.
	devices["hallway-lamp"] = member{room: "hallway", caps: lampCaps}
	declare(h.e.Store, devices)
	h.clock.Step(5 * time.Minute)

	h.expectCalls(
		call{"hallway-light", capability.LightOn, true},
		call{"hallway-lamp", capability.LightOn, false},
		call{"hallway-light", capability.LightOn, false},
	)
	h.eventually("succeeded", func() bool { return h.runs(ResultSucceeded) == 1 })
}
