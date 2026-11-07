package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
)

// gatedControls is a store whose saves wait for the test, and can fail.
type gatedControls struct {
	States[ControlState]
	gate chan struct{}
	err  atomic.Pointer[error]
}

func (g *gatedControls) Put(ctx context.Context, key ident.ID, s ControlState) error {
	<-g.gate
	if err := g.err.Load(); err != nil {
		return *err
	}
	return g.States.Put(ctx, key, s)
}

func arbitratedEngine(t *testing.T, controls ControlStates) (*Engine, ident.ID) {
	t.Helper()
	dev := ident.ID{Namespace: ns, Name: "valve"}
	eng := &Engine{Clock: clock.NewFake(time.Now()), Log: logging.Discard(), Controls: controls}
	store := hub.NewStore(logging.Discard(), eng)
	eng.Store, eng.Setter = store, &fakeSetter{}
	eng.Init()
	store.SetDevices(z2m, map[ident.ID]hub.Meta{dev: {
		Instance: z2m,
		Control:  []control.Entry{entry(capability.On, "true", control.NamePreferOff, control.NameOverride, "1h")},
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
		_, err := eng.Claim(context.Background(), dev, capability.On, testClaim())
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
	if !ok || len(saved[capability.On].Claims) != 1 {
		t.Fatalf("saved %+v", saved)
	}
}

func TestClaimReportsAFailedSave(t *testing.T) {
	controls := &gatedControls{States: newMemoryStates[ControlState](), gate: make(chan struct{})}
	close(controls.gate)
	failure := errors.New("disk full")
	controls.err.Store(&failure)
	eng, dev := arbitratedEngine(t, controls)
	if _, err := eng.Claim(context.Background(), dev, capability.On, testClaim()); err == nil {
		t.Fatal("a claim that wasn't saved was reported as saved")
	}
	// It stays marked, so the next save still carries it.
	controls.err.Store(nil)
	if err := eng.flushControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if saved, _ := controls.Get(dev); len(saved[capability.On].Claims) != 1 {
		t.Fatalf("saved %+v", saved)
	}
}

func TestReleaseAnswersOnlyOnceSaved(t *testing.T) {
	controls := newMemoryStates[ControlState]()
	eng, dev := arbitratedEngine(t, controls)
	c := testClaim()
	if ok, err := eng.Claim(context.Background(), dev, capability.On, c); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := eng.ReleaseClaim(context.Background(), dev, capability.On, c.Source); !ok || err != nil {
		t.Fatalf("release: %v %v", ok, err)
	}
	if saved, _ := controls.Get(dev); len(saved[capability.On].Claims) != 0 {
		t.Fatalf("still saved: %+v", saved)
	}
}

func TestClaimOnAnUnarbitratedCapabilityIsRefused(t *testing.T) {
	eng, dev := arbitratedEngine(t, newMemoryStates[ControlState]())
	if ok, err := eng.Claim(context.Background(), dev, capability.Brightness, testClaim()); ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestFlowBodyRunsAsARunInProgress(t *testing.T) {
	started := make(chan struct{}, 4)
	stop := make(chan struct{})
	key := goKey("motion-light")
	c, err := NewFlow(key, Flow{
		Changes: []Change{{Device: "hallway-motion", Capability: capability.Occupied}},
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
		Changes: []Change{{Device: "hallway-motion", Capability: capability.Occupied}},
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
		on, ok := devices["hallway-motion"][capability.Occupied].(bool)
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
		h.e.Store.Changed(driver.Update{Device: ident.ID{Namespace: ns, Name: "hallway-motion"}, Capability: capability.Occupied, Value: v})
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
	if err := h.e.Apply(context.Background(), key, Set{Device: "hallway-light", Capability: capability.On, Value: true}); err != nil {
		t.Fatal(err)
	}
	h.expectCalls(call{"hallway-light", capability.On, true})
	if err := h.e.Apply(context.Background(), key, Set{Device: "hallway-light", Capability: "no.such", Value: true}); err == nil {
		t.Fatal("an invalid set was applied")
	}
	h.setter.err = errors.New("offline")
	if err := h.e.Apply(context.Background(), key, Set{Device: "hallway-light", Capability: capability.On, Value: true}); err == nil {
		t.Fatal("a failed write wasn't reported")
	}
}

func TestApplyIsAnErrorWhereAMomentOnlyLogs(t *testing.T) {
	eng, dev := arbitratedEngine(t, newMemoryStates[ControlState]())
	key := goKey("routine")
	unended := Set{Device: dev.Name, Capability: capability.On, Value: false}
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
	eng.arbiter(dev, capability.On)
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
	broken ident.ID
}

func (f *failingFor) Put(ctx context.Context, key ident.ID, s ControlState) error {
	if key == f.broken {
		return errors.New("disk full")
	}
	return f.States.Put(ctx, key, s)
}

func TestReleaseClaimsOnlyAnswersForTheDevicesItChanged(t *testing.T) {
	broken := ident.ID{Namespace: ns, Name: "broken"}
	eng, dev := arbitratedEngine(t, &failingFor{States: newMemoryStates[ControlState](), broken: broken})
	eng.Store.SetDevices(z2m, map[ident.ID]hub.Meta{
		dev:    {Instance: z2m, Control: []control.Entry{entry(capability.On, "true", control.NamePreferOff, control.NameOverride, "1h")}},
		broken: {Instance: z2m, Control: []control.Entry{entry(capability.On, "true", control.NamePreferOff, control.NameOverride, "1h")}},
	})
	ctx := context.Background()
	if _, err := eng.Claim(ctx, dev, capability.On, testClaim()); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Claim(ctx, broken, capability.On, control.Claim{Source: "Rule/stuck", Level: control.Normal, Expires: time.Now().Add(time.Hour)}); err == nil {
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
	other := ident.ID{Namespace: ns, Name: "other"}
	third := ident.ID{Namespace: ns, Name: "third"}
	meta := hub.Meta{Instance: z2m, Control: []control.Entry{
		entry(capability.On, "true", control.NamePreferOff, control.NameOverride, "1h"),
	}}
	eng.Store.SetDevices(z2m, map[ident.ID]hub.Meta{dev: meta, other: meta, third: meta})
	ctx := context.Background()
	key := goKey("routine")
	expires := time.Now().Add(time.Hour)
	for d, level := range map[ident.ID]control.Level{dev: control.Normal, other: control.Override, third: control.Safety} {
		claim := control.Claim{Source: ruleSource(key.Name), Level: level, Expires: expires}
		if _, err := eng.Claim(ctx, d, capability.On, claim); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.ReleaseFlow(ctx, key); err != nil {
		t.Fatal(err)
	}
	claims := func(d ident.ID) int { return len(eng.arbs[d][capability.On].a.State().Claims) }
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
	if _, err := eng.Claim(ctx, dev, capability.On, claim); err != nil {
		t.Fatal(err)
	}
	if err := eng.ReleaseClaims(ctx, ns, "Rule/x", allLevels); err != nil {
		t.Fatal(err)
	}
	if got := eng.arbs[dev][capability.On].a.State().Claims; len(got) != 0 {
		t.Fatalf("an explicit release left %+v", got)
	}
}

func TestNewFlowNeedsABody(t *testing.T) {
	if _, err := NewFlow(goKey("x"), Flow{Changes: []Change{{Device: "a", Capability: capability.Occupied}}}); err == nil {
		t.Fatal("a flow with no body compiled")
	}
}

func TestKeepSinceCatchesUpOnAMissedTime(t *testing.T) {
	timer := func(deadline time.Duration) *Compiled {
		c, err := NewMoment(goKey("motion-light"), Moment{
			Timers: []Timer{At{Time: "07:00"}}, StartingDeadline: deadline,
			Do: []Action{Set{Device: "hallway-light", Capability: capability.On, Value: true}},
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
	h.expectCalls(call{"hallway-light", capability.On, true})

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
		if _, err := eng.Claim(ctx, dev, capability.On, c); err != nil {
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
	if _, err := eng.Claim(ctx, dev, capability.On, claim); err != nil {
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

// A wait that ends early has to take its timer with it: simhome reads anything left on the clock as
// the engine having armed its next wake-up, and settles too soon.
func TestAWaitThatEndsEarlyLeavesNothingOnTheClock(t *testing.T) {
	key := goKey("motion-light")
	never := func(Vars) (bool, error) { return false, nil }
	waiting := func(t *testing.T, c *clock.Fake) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !c.HasWaiters() {
			if time.Now().After(deadline) {
				t.Fatal("nothing waited on the clock")
			}
			time.Sleep(time.Millisecond)
		}
	}
	finished := func(t *testing.T, done <-chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the wait didn't end")
		}
	}
	bare := func(t *testing.T) (*Engine, *clock.Fake) {
		t.Helper()
		c := clock.NewFake(time.Now())
		eng := &Engine{Clock: c, Log: logging.Discard()}
		eng.Store = hub.NewStore(logging.Discard(), eng)
		eng.Init()
		return eng, c
	}

	t.Run("WaitFor cancelled", func(t *testing.T) {
		eng, c := bare(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = eng.WaitFor(ctx, key, never, time.Time{})
		}()
		waiting(t, c)
		cancel()
		finished(t, done)
		if c.HasWaiters() {
			t.Fatal("a cancelled wait left its timer on the clock")
		}
	})

	t.Run("WaitFor woken by a change", func(t *testing.T) {
		eng, c := bare(t)
		var met atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = eng.WaitFor(context.Background(), key, func(Vars) (bool, error) { return met.Load(), nil }, time.Time{})
		}()
		waiting(t, c)
		met.Store(true)
		eng.mu.Lock()
		close(eng.changes)
		eng.changes = make(chan struct{})
		eng.mu.Unlock()
		finished(t, done)
		if c.HasWaiters() {
			t.Fatal("a wait woken by a change left its timer on the clock")
		}
	})

	t.Run("WaitFor woken by a device announced again", func(t *testing.T) {
		eng, c := bare(t)
		var met atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = eng.WaitFor(context.Background(), key, func(Vars) (bool, error) { return met.Load(), nil }, time.Time{})
		}()
		waiting(t, c)
		met.Store(true)
		eng.membersChanged(ns)
		finished(t, done)
	})

	t.Run("a delay cancelled", func(t *testing.T) {
		eng, c := bare(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			eng.runActions(ctx, key, &Compiled{actions: []action{{delay: time.Minute}}})
		}()
		waiting(t, c)
		cancel()
		finished(t, done)
		if c.HasWaiters() {
			t.Fatal("a cancelled delay left its timer on the clock")
		}
	})
}
