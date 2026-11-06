package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	testclock "k8s.io/utils/clock/testing"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
)

// gatedControls is a store whose saves wait for the test, and can fail.
type gatedControls struct {
	States[ControlState]
	gate chan struct{}
	err  atomic.Pointer[error]
}

func (g *gatedControls) Put(ctx context.Context, key types.NamespacedName, s ControlState) error {
	<-g.gate
	if err := g.err.Load(); err != nil {
		return *err
	}
	return g.States.Put(ctx, key, s)
}

func arbitratedEngine(t *testing.T, controls ControlStates) (*Engine, types.NamespacedName) {
	t.Helper()
	dev := types.NamespacedName{Namespace: ns, Name: "valve"}
	eng := &Engine{Clock: testclock.NewFakeClock(time.Now()), Log: logr.Discard(), Controls: controls}
	store := hub.NewStore(logr.Discard(), eng)
	eng.Store, eng.Setter = store, &fakeSetter{}
	eng.Init()
	store.SetDevices(z2m, map[types.NamespacedName]hub.Meta{dev: {
		Instance: z2m,
		Control:  control.FromAPIList([]homev1alpha1.CapabilityControl{entry(capability.SwitchOn, "true", homev1alpha1.ControlResolvePreferOff, homev1alpha1.ControlExternalOverride, "1h")}),
	}})
	return eng, dev
}

func testClaim() control.Claim {
	return control.Claim{Source: "Rule/x", Level: control.Normal, Value: 0, Expires: time.Now().Add(time.Hour)}
}

func TestClaimAnswersOnlyOnceSaved(t *testing.T) {
	controls := &gatedControls{States: newMemoryStates[ControlState](), gate: make(chan struct{})}
	eng, dev := arbitratedEngine(t, controls)
	answered := make(chan error, 1)
	go func() {
		_, err := eng.Claim(context.Background(), dev, capability.SwitchOn, testClaim())
		answered <- err
	}()
	select {
	case err := <-answered:
		t.Fatalf("the claim was answered before it was saved: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(controls.gate)
	select {
	case err := <-answered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the claim was never answered")
	}
	saved, ok := controls.Get(dev)
	if !ok || len(saved[capability.SwitchOn].Claims) != 1 {
		t.Fatalf("saved %+v", saved)
	}
}

func TestClaimReportsAFailedSave(t *testing.T) {
	controls := &gatedControls{States: newMemoryStates[ControlState](), gate: make(chan struct{})}
	close(controls.gate)
	failure := errors.New("disk full")
	controls.err.Store(&failure)
	eng, dev := arbitratedEngine(t, controls)
	if _, err := eng.Claim(context.Background(), dev, capability.SwitchOn, testClaim()); err == nil {
		t.Fatal("a claim that wasn't saved was reported as saved")
	}
	// It stays marked, so the next save still carries it.
	controls.err.Store(nil)
	if err := eng.flushControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if saved, _ := controls.Get(dev); len(saved[capability.SwitchOn].Claims) != 1 {
		t.Fatalf("saved %+v", saved)
	}
}

func TestReleaseAnswersOnlyOnceSaved(t *testing.T) {
	controls := newMemoryStates[ControlState]()
	eng, dev := arbitratedEngine(t, controls)
	c := testClaim()
	if ok, err := eng.Claim(context.Background(), dev, capability.SwitchOn, c); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := eng.ReleaseClaim(context.Background(), dev, capability.SwitchOn, c.Source); !ok || err != nil {
		t.Fatalf("release: %v %v", ok, err)
	}
	if saved, _ := controls.Get(dev); len(saved[capability.SwitchOn].Claims) != 0 {
		t.Fatalf("still saved: %+v", saved)
	}
}

func TestClaimOnAnUnarbitratedCapabilityIsRefused(t *testing.T) {
	eng, dev := arbitratedEngine(t, newMemoryStates[ControlState]())
	if ok, err := eng.Claim(context.Background(), dev, capability.LightOn, testClaim()); ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestFlowBodyRunsAsARunInProgress(t *testing.T) {
	started := make(chan struct{}, 4)
	stop := make(chan struct{})
	key := goKey("motion-light")
	c, err := NewFlow(key, Flow{
		Trigger: Trigger{Changes: []Change{{Device: "hallway-motion", Capability: capability.Occupancy}}},
		Body: func(context.Context) string {
			started <- struct{}{}
			<-stop
			return ResultSucceeded
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, time.Now(), func(e *Engine) { e.Load(c) })
	h.motion(true)
	<-started
	h.motion(false)
	h.eventually("the trigger to count as busy", func() bool { return h.runs(ResultBusy) == 1 })
	close(stop)
	h.eventually("the run to finish", func() bool { return h.runs(ResultSucceeded) == 1 })
}

func TestResumedRunHoldsItsPlace(t *testing.T) {
	key := goKey("motion-light")
	runs := make(chan string, 2)
	stop := make(chan struct{})
	c, err := NewFlow(key, Flow{
		Trigger: Trigger{Changes: []Change{{Device: "hallway-motion", Capability: capability.Occupancy}}},
		Body:    func(context.Context) string { runs <- "new"; return ResultSucceeded },
	})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, time.Now(), func(e *Engine) {
		e.Load(c)
		if !e.Resume(context.Background(), key, func(context.Context) string {
			runs <- "resumed"
			<-stop
			return ResultSucceeded
		}) {
			t.Error("no such flow")
		}
		if e.Resume(context.Background(), goKey("nothing"), nil) {
			t.Error("resumed a flow that isn't loaded")
		}
	})
	if got := <-runs; got != "resumed" {
		t.Fatalf("got %s", got)
	}
	h.motion(true)
	h.eventually("the trigger to count as busy", func() bool { return h.runs(ResultBusy) == 1 })
	close(stop)
}

func TestWaitForWakesOnAChangeAndAtItsLimit(t *testing.T) {
	h := newHarness(t, time.Now())
	key := goKey("motion-light")
	occupied := func(v Vars) (bool, error) {
		devices, _ := v["devices"].(map[string]map[string]any)
		on, ok := devices["hallway-motion"][capability.Occupancy].(bool)
		if !ok {
			return false, errors.New("not reported")
		}
		return on, nil
	}
	woke := make(chan bool, 1)
	go func() {
		ok, err := h.e.WaitFor(context.Background(), key, occupied, time.Time{})
		if err != nil {
			t.Error(err)
		}
		woke <- ok
	}()
	report := func(v bool) {
		h.e.Store.Changed(driver.Update{Device: types.NamespacedName{Namespace: ns, Name: "hallway-motion"}, Capability: capability.Occupancy, Value: v})
		h.motion(v)
	}
	report(false)
	select {
	case <-woke:
		t.Fatal("woke with the condition false")
	case <-time.After(50 * time.Millisecond):
	}
	report(true)
	select {
	case ok := <-woke:
		if !ok {
			t.Fatal("woke with no result")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a change didn't wake the wait")
	}

	if ok, err := h.e.WaitFor(context.Background(), key, func(Vars) (bool, error) { return false, nil }, h.clock.Now()); ok || err != nil {
		t.Fatalf("at its limit: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.e.WaitFor(ctx, key, occupied, time.Time{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestApplyRunsOneAction(t *testing.T) {
	h := newHarness(t, time.Now())
	key := goKey("motion-light")
	if err := h.e.Apply(context.Background(), key, Set{Device: "hallway-light", Capability: capability.LightOn, Value: true}); err != nil {
		t.Fatal(err)
	}
	h.expectCalls(call{"hallway-light", capability.LightOn, true})
	if err := h.e.Apply(context.Background(), key, Set{Device: "hallway-light", Capability: "no.such", Value: true}); err == nil {
		t.Fatal("an invalid set was applied")
	}
	h.setter.err = errors.New("offline")
	if err := h.e.Apply(context.Background(), key, Set{Device: "hallway-light", Capability: capability.LightOn, Value: true}); err == nil {
		t.Fatal("a failed write wasn't reported")
	}
}

func TestApplyIsAnErrorWhereAMomentOnlyLogs(t *testing.T) {
	eng, dev := arbitratedEngine(t, newMemoryStates[ControlState]())
	key := goKey("routine")
	unended := Set{Device: dev.Name, Capability: capability.SwitchOn, Value: false}
	if err := eng.Apply(context.Background(), key, unended); err == nil {
		t.Fatal("a set with no end on an arbitrated capability looked done")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ended := unended
	ended.For = time.Hour
	if err := eng.Apply(cancelled, key, ended); !errors.Is(err, context.Canceled) {
		t.Fatalf("an action cut short looked done: %v", err)
	}
}

func TestFlushReportsOnlyTheDeviceItWasAskedAbout(t *testing.T) {
	controls := &gatedControls{States: newMemoryStates[ControlState](), gate: make(chan struct{})}
	close(controls.gate)
	eng, dev := arbitratedEngine(t, controls)
	failure := errors.New("disk full")
	controls.err.Store(&failure)
	eng.mu.Lock()
	eng.arbiter(dev, capability.SwitchOn)
	eng.markSave(dev)
	eng.mu.Unlock()
	if err := eng.flushControls(context.Background(), goKey("another")); err != nil {
		t.Fatalf("another device's failure failed this one: %v", err)
	}
	if err := eng.flushControls(context.Background()); err == nil {
		t.Fatal("a failed save, still due, was reported as saved")
	}
	if err := eng.flushControls(context.Background(), dev); err == nil {
		t.Fatal("a failed save, still due, was reported as saved")
	}
}

// A panic with e.mu held would stop every rule and every heartbeat, and the replay would reach the
// same step after each restart.
func TestWaitForRefusesNoConditionAndLeavesTheEngineFree(t *testing.T) {
	eng, _ := arbitratedEngine(t, newMemoryStates[ControlState]())
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("it panicked: %v", p)
			}
		}()
		if _, err := eng.WaitFor(context.Background(), goKey("routine"), nil, time.Time{}); err == nil {
			t.Error("a wait with no condition was accepted")
		}
	}()
	free := make(chan struct{})
	go func() { eng.Responsive(); close(free) }()
	select {
	case <-free:
	case <-time.After(2 * time.Second):
		t.Fatal("the engine's lock was left held")
	}
}

// failingFor is a store that can't save one device.
type failingFor struct {
	States[ControlState]
	broken types.NamespacedName
}

func (f *failingFor) Put(ctx context.Context, key types.NamespacedName, s ControlState) error {
	if key == f.broken {
		return errors.New("disk full")
	}
	return f.States.Put(ctx, key, s)
}

func TestReleaseClaimsOnlyAnswersForTheDevicesItChanged(t *testing.T) {
	broken := types.NamespacedName{Namespace: ns, Name: "broken"}
	eng, dev := arbitratedEngine(t, &failingFor{States: newMemoryStates[ControlState](), broken: broken})
	eng.Store.SetDevices(z2m, map[types.NamespacedName]hub.Meta{
		dev:    {Instance: z2m, Control: control.FromAPIList([]homev1alpha1.CapabilityControl{entry(capability.SwitchOn, "true", homev1alpha1.ControlResolvePreferOff, homev1alpha1.ControlExternalOverride, "1h")})},
		broken: {Instance: z2m, Control: control.FromAPIList([]homev1alpha1.CapabilityControl{entry(capability.SwitchOn, "true", homev1alpha1.ControlResolvePreferOff, homev1alpha1.ControlExternalOverride, "1h")})},
	})
	ctx := context.Background()
	if _, err := eng.Claim(ctx, dev, capability.SwitchOn, testClaim()); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Claim(ctx, broken, capability.SwitchOn, control.Claim{Source: "Rule/stuck", Level: control.Normal, Expires: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("the broken device saved")
	}

	if err := eng.ReleaseClaims(ctx, ns, "Rule/nothing", allLevels); err != nil {
		t.Fatalf("a release that changed nothing failed over another device: %v", err)
	}
	if err := eng.ReleaseClaims(ctx, ns, "Rule/x", allLevels); err != nil {
		t.Fatalf("a release failed over another device: %v", err)
	}
	if err := eng.ReleaseClaims(ctx, ns, "Rule/stuck", allLevels); err == nil {
		t.Fatal("a release that couldn't be saved looked saved")
	}
}

func TestReleaseFlowEndsEveryClaimButSafetyOnes(t *testing.T) {
	eng, dev := arbitratedEngine(t, newMemoryStates[ControlState]())
	other := types.NamespacedName{Namespace: ns, Name: "other"}
	third := types.NamespacedName{Namespace: ns, Name: "third"}
	meta := hub.Meta{Instance: z2m, Control: control.FromAPIList([]homev1alpha1.CapabilityControl{
		entry(capability.SwitchOn, "true", homev1alpha1.ControlResolvePreferOff, homev1alpha1.ControlExternalOverride, "1h"),
	})}
	eng.Store.SetDevices(z2m, map[types.NamespacedName]hub.Meta{dev: meta, other: meta, third: meta})
	ctx := context.Background()
	key := goKey("routine")
	expires := time.Now().Add(time.Hour)
	for d, level := range map[types.NamespacedName]control.Level{dev: control.Normal, other: control.Override, third: control.Safety} {
		claim := control.Claim{Source: ruleSource(key.Name), Level: level, Expires: expires}
		if _, err := eng.Claim(ctx, d, capability.SwitchOn, claim); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.ReleaseFlow(ctx, key); err != nil {
		t.Fatal(err)
	}
	claims := func(d types.NamespacedName) int { return len(eng.arbs[d][capability.SwitchOn].a.State().Claims) }
	if claims(dev) != 0 || claims(other) != 0 {
		t.Fatalf("claims stayed: normal %d, override %d", claims(dev), claims(other))
	}
	if claims(third) != 1 {
		t.Fatal("a Safety claim was released")
	}
}

func TestReleaseWithAllLevelsEndsSafetyClaims(t *testing.T) {
	eng, dev := arbitratedEngine(t, newMemoryStates[ControlState]())
	ctx := context.Background()
	claim := control.Claim{Source: "Rule/x", Level: control.Safety, Expires: time.Now().Add(time.Hour)}
	if _, err := eng.Claim(ctx, dev, capability.SwitchOn, claim); err != nil {
		t.Fatal(err)
	}
	if err := eng.ReleaseClaims(ctx, ns, "Rule/x", allLevels); err != nil {
		t.Fatal(err)
	}
	if got := eng.arbs[dev][capability.SwitchOn].a.State().Claims; len(got) != 0 {
		t.Fatalf("an explicit release left %+v", got)
	}
}

func TestNewFlowNeedsABody(t *testing.T) {
	if _, err := NewFlow(goKey("x"), Flow{Trigger: Trigger{Changes: []Change{{Device: "a", Capability: capability.Occupancy}}}}); err == nil {
		t.Fatal("a flow with no body compiled")
	}
}

func TestKeepSinceCatchesUpOnAMissedTime(t *testing.T) {
	timer := func(deadline time.Duration) *Compiled {
		c, err := NewMoment(goKey("motion-light"), Moment{
			Trigger: Trigger{Timers: []Timer{At{Time: "07:00"}}, StartingDeadline: deadline},
			Do:      []Action{Set{Device: "hallway-light", Capability: capability.LightOn, Value: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		c.KeepSince(local("2026-06-15 06:00 BST"))
		return c
	}
	now := local("2026-06-15 07:05 BST")
	h := newHarness(t, now)
	h.e.Load(timer(10 * time.Minute))
	h.expectCalls(call{"hallway-light", capability.LightOn, true})

	h = newHarness(t, now)
	h.e.Load(timer(time.Minute))
	h.eventually("the missed time to be skipped", func() bool { return h.runs(ResultMissed) == 1 })
	h.expectCalls()
}

func TestRuleClaimsAndEndRule(t *testing.T) {
	eng, dev := arbitratedEngine(t, newMemoryStates[ControlState]())
	ctx := context.Background()
	for _, c := range []control.Claim{
		{Source: "Rule/leak", Level: control.Safety, Expires: time.Now().Add(time.Hour)},
		{Source: "Rule/leak-too", Level: control.Normal, Expires: time.Now().Add(time.Hour)},
		{Source: "Device", Level: control.Override, Expires: time.Now().Add(time.Hour)},
	} {
		if _, err := eng.Claim(ctx, dev, capability.SwitchOn, c); err != nil {
			t.Fatal(err)
		}
	}
	held := eng.RuleClaims(ns)
	if len(held) != 2 || held["leak"] != control.Safety || held["leak-too"] != control.Normal {
		t.Fatalf("got %v", held)
	}
	if got := eng.RuleClaims("elsewhere"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if err := eng.EndRule(ctx, ns, "leak"); err != nil {
		t.Fatal(err)
	}
	if held := eng.RuleClaims(ns); len(held) != 1 || held["leak-too"] != control.Normal {
		t.Fatalf("a Safety claim outlived its rule: %v", held)
	}
}

// A host deals with removed rules as it starts, before the engine has looked at any device.
func TestRuleClaimsCountsClaimsStillOnDisk(t *testing.T) {
	controls := newMemoryStates[ControlState]()
	eng, dev := arbitratedEngine(t, controls)
	ctx := context.Background()
	claim := control.Claim{Source: "Rule/leak", Level: control.Safety, Expires: time.Now().Add(time.Hour)}
	if _, err := eng.Claim(ctx, dev, capability.SwitchOn, claim); err != nil {
		t.Fatal(err)
	}
	restarted, _ := arbitratedEngine(t, controls)
	if held := restarted.RuleClaims(ns); held["leak"] != control.Safety {
		t.Fatalf("got %v", held)
	}
	if err := restarted.EndRule(ctx, ns, "leak"); err != nil {
		t.Fatal(err)
	}
	if held := restarted.RuleClaims(ns); len(held) != 0 {
		t.Fatalf("got %v", held)
	}
}
