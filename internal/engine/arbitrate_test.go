package engine

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
)

// entry is a bool control entry.
func entry(name, def, resolve, external, hold string) control.Entry {
	return control.Entry{Capability: name, Default: []byte(def), Resolve: resolve, External: external, OverrideHold: hold}
}

// status returns the device's control status for one capability.
func (r *rig) status(dev, name string) control.Status {
	for _, s := range r.e.ControlOf(ident.ID{Namespace: ns, Name: dev}) {
		if s.Capability == name {
			return s
		}
	}
	return control.Status{}
}

// winner returns the winning claim's source and level, or the default.
func winner(s control.Status) string {
	for _, c := range s.Claims {
		if c.Winner {
			return c.Source + " " + c.Level
		}
	}
	return s.Level
}

func TestScenario6WallSwitchOverridesUntilNext(t *testing.T) {
	light := holdRule(t, "hall-motion-light", Hold{Device: "hall-light", Capability: capability.On, Value: true, When: isTrue("hall-motion", capability.Occupied), OffDelay: 5 * time.Minute})
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 19:59 BST"),
		values:  map[string]map[string]any{"hall-motion": {capability.Occupied: false}, "hall-light": {capability.On: false}},
		control: map[string][]control.Entry{"hall-light": {entry(capability.On, "false", control.NamePreferOn, control.NameOverride, "Next")}},
	}, light)
	r.expect()
	r.at("2026-10-21 20:00 BST")
	r.change("hall-motion", capability.Occupied, true)
	r.expect(call{"hall-light", capability.On, true})
	if w := winner(r.status("hall-light", capability.On)); w != "Rule/hall-motion-light Normal" {
		t.Errorf("20:00 winner %q", w)
	}

	// A wall switch turns it off at 20:05: an Override claim from the device.
	r.at("2026-10-21 20:05 BST")
	r.change("hall-light", capability.On, false)
	if w := winner(r.status("hall-light", capability.On)); w != "Device Override" {
		t.Errorf("20:05 winner %q", w)
	}
	// Motion clears at 20:10, and the hold ends at 20:15. The override ends with it.
	r.at("2026-10-21 20:10 BST")
	r.change("hall-motion", capability.Occupied, false)
	r.at("2026-10-21 20:15 BST")
	if s := r.status("hall-light", capability.On); len(s.Claims) != 0 || s.Level != control.NameDefault {
		t.Errorf("20:15 status %+v, want the default with no claims", s)
	}
	// The next motion turns it on.
	r.at("2026-10-21 20:20 BST")
	r.change("hall-motion", capability.Occupied, true)
	r.expect(call{"hall-light", capability.On, true}, call{"hall-light", capability.On, true})
}

func testScenario7BoostEndsAtTheFirstOfForAndNext(t *testing.T, states *storeBackend) {
	rail := entry(capability.On, "false", control.NamePreferOn, control.NameOverride, "Next")
	rules := func() []*Compiled {
		evening := holdRule(t, "towel-rail-evenings", Hold{Device: "towel-rail", Capability: capability.On, Value: true, Window: &Window{Start: At{Time: "18:00"}, End: At{Time: "21:00"}}})
		boost := momentRule(t, "towel-rail-boost", Moment{Timers: []Timer{At{Time: "21:30"}}, Since: local("2026-10-21 12:00 BST"), Do: []Action{Set{Device: "towel-rail", Capability: capability.On, Value: true, For: time.Hour, Until: UntilNext}}})
		return []*Compiled{evening, boost}
	}
	start := func(now string, on bool) *rig {
		return startRigWith(t, rigOptions{
			now: local(now), holds: states.holds(t), controls: states.controls(t),
			values:  map[string]map[string]any{"towel-rail": {capability.On: on}},
			control: map[string][]control.Entry{"towel-rail": {rail}},
		}, rules()...)
	}
	r := start("2026-10-21 17:59 BST", false)
	r.at("2026-10-21 18:00 BST")
	r.at("2026-10-21 21:00 BST")
	r.at("2026-10-21 21:30 BST")
	s := r.status("towel-rail", capability.On)
	if w := winner(s); w != "Rule/towel-rail-boost Normal" || !s.Claims[0].Expires.Equal(local("2026-10-21 22:30 BST")) {
		t.Errorf("21:30 status %+v", s)
	}
	r.at("2026-10-21 21:45 BST")
	r.stop()

	// The boost's claim and expiry survive a restart.
	r = start("2026-10-21 21:45 BST", true)
	r.expect()
	r.at("2026-10-21 22:30 BST")
	r.expect(call{"towel-rail", capability.On, false})
}

func testScenario8LeakClosesTheValveUntilReleased(t *testing.T, states *storeBackend) {
	valve := entry(capability.On, "true", control.NamePreferOff, control.NameRevert, "")
	shutoff := func() *Compiled {
		return holdRule(t, "leak-shutoff", Hold{Device: "main-valve", Capability: capability.On, Value: false, Priority: PrioritySafety, When: isTrue("leak-sensor", capability.Open), Latch: true})
	}
	reset := momentRule(t, "leak-reset", Moment{Timers: []Timer{At{Time: "16:00"}}, Since: local("2026-10-21 12:00 BST"), Do: []Action{Release("leak-shutoff")}})
	start := func(now string, open bool, rules ...*Compiled) *rig {
		return startRigWith(t, rigOptions{
			now: local(now), holds: states.holds(t), controls: states.controls(t),
			values:  map[string]map[string]any{"leak-sensor": {capability.Open: false}, "main-valve": {capability.On: open}},
			control: map[string][]control.Entry{"main-valve": {valve}},
		}, rules...)
	}
	r := start("2026-10-21 13:59 BST", true, shutoff())
	r.expect()
	r.at("2026-10-21 14:00 BST")
	r.change("leak-sensor", capability.Open, true)
	r.expect(call{"main-valve", capability.On, false})
	r.change("main-valve", capability.On, false)

	// The Home app opens it at 14:05, and the engine closes it again.
	r.at("2026-10-21 14:05 BST")
	r.change("main-valve", capability.On, true)
	r.expect(call{"main-valve", capability.On, false}, call{"main-valve", capability.On, false})
	s := r.status("main-valve", capability.On)
	if w := winner(s); w != "Rule/leak-shutoff Safety" || !s.Claims[0].Latched || !s.Claims[len(s.Claims)-1].Reverted {
		t.Errorf("14:05 status %+v", s)
	}
	// It dries at 15:00, and stays closed: it's latched.
	r.at("2026-10-21 15:00 BST")
	r.change("leak-sensor", capability.Open, false)
	r.at("2026-10-21 15:30 BST")
	r.stop()

	// A restart keeps it closed, until the reset at 16:00.
	r = start("2026-10-21 15:30 BST", false, shutoff(), reset)
	r.expect()
	r.at("2026-10-21 16:00 BST")
	r.expect(call{"main-valve", capability.On, true})
}

func TestScenario5LimitsHoldThePumpOn(t *testing.T) {
	pump := entry(capability.On, "false", control.NamePreferOn, control.NameOverride, "2h")
	pump.MinimumOn = control.Duration(time.Hour)
	pump.MinimumOff = control.Duration(30 * time.Minute)
	pump.MaximumStartsPerDay = 4
	slot := holdRule(t, "pump-slot", Hold{Device: "pump", Capability: capability.On, Value: true, Window: &Window{Start: At{Time: "22:00"}, End: At{Time: "02:00"}}})
	frost := holdRule(t, "pump-frost", Hold{Device: "pump", Capability: capability.On, Value: true, Priority: PrioritySafety, When: below("workshop-temp", capability.Temperature, 0)})
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-22 01:00 BST"),
		values:  map[string]map[string]any{"pump": {capability.On: true}, "workshop-temp": {capability.Temperature: 5.0}},
		control: map[string][]control.Entry{"pump": {pump}},
	}, slot, frost)
	r.expect()
	// A Home app tap turns it off at 01:40: an Override until 03:40.
	r.at("2026-10-22 01:40 BST")
	r.change("pump", capability.On, false)
	r.at("2026-10-22 02:50 BST")
	r.change("workshop-temp", capability.Temperature, -1.0)
	r.expect(call{"pump", capability.On, true})
	// Frost ends at 03:30. The Override (off) wins, but minimumOn keeps it on until 03:50.
	r.at("2026-10-22 03:30 BST")
	r.change("workshop-temp", capability.Temperature, 1.0)
	r.expect(call{"pump", capability.On, true})
	r.at("2026-10-22 03:49 BST")
	r.expect(call{"pump", capability.On, true})
	r.at("2026-10-22 03:50 BST")
	r.expect(call{"pump", capability.On, true}, call{"pump", capability.On, false})
}

func TestEndlessSetsSkipArbitratedDevices(t *testing.T) {
	rail := entry(capability.On, "false", control.NamePreferOn, control.NameOverride, "1h")
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 12:00 BST"),
		values:  map[string]map[string]any{"towel-rail": {capability.On: false}},
		control: map[string][]control.Entry{"towel-rail": {rail}},
	})
	metas := map[ident.ID]hub.Meta{
		{Namespace: ns, Name: "towel-rail"}:  {Instance: z2m, Control: []control.Entry{rail}},
		{Namespace: ns, Name: "hall-motion"}: {Instance: z2m},
		{Namespace: ns, Name: "bath-rail"}:   {Instance: z2m, Room: "bathroom", Control: []control.Entry{rail}},
		{Namespace: ns, Name: "bath-fan"}:    {Instance: z2m, Room: "bathroom"},
	}
	r.store.SetDevices(z2m, metas)
	for dev := range metas {
		r.store.Discovered(dev, driver.Info{Found: true, Capabilities: []string{capability.On, capability.Occupied}})
	}
	// Let the bath rail's default write land before the run, so the order below is fixed.
	r.settle()
	c := momentRule(t, "bathroom-on", Moment{Changes: []Change{{Device: "hall-motion", Capability: capability.Occupied}}, Do: []Action{Set{Room: "bathroom", Capability: capability.On, Value: true}, Set{Device: "towel-rail", Capability: capability.On, Value: true}, Set{Room: "bathroom", Capability: capability.On, Value: true, For: 10 * time.Minute}}})
	r.e.Load(c)
	r.change("hall-motion", capability.Occupied, true)
	r.eventually("the run", func() bool { return len(r.setter.got()) == 4 })
	r.settle()
	// The bath rail gets its default when it's discovered with no value. Then the endless sets
	// write the fan and skip both rails, and the timed one claims the bath rail.
	r.expect(call{"bath-rail", capability.On, false}, call{"bath-fan", capability.On, true},
		call{"bath-fan", capability.On, true}, call{"bath-rail", capability.On, true})
	r.logs.mu.Lock()
	defer r.logs.mu.Unlock()
	if !slices.ContainsFunc(r.logs.lines, func(l string) bool {
		return strings.Contains(l, "Skipped Room members") && strings.Contains(l, "bath-rail")
	}) {
		t.Errorf("no log of the skipped bath rail in %q", r.logs.lines)
	}
}

func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	poll(r.t, what, cond)
}

// gate holds every Set until it's opened.
type gate struct {
	*fakeSetter
	open chan struct{}
}

func (g *gate) Set(ctx context.Context, dev ident.ID, c string, v any) error {
	<-g.open
	return g.fakeSetter.Set(ctx, dev, c, v)
}

var railEntry = entry(capability.On, "false", control.NamePreferOn, control.NameOverride, "1h")

func railRig(t *testing.T, rules ...*Compiled) *rig {
	return startRigWith(t, rigOptions{
		now:     local("2026-10-21 12:00 BST"),
		values:  map[string]map[string]any{"towel-rail": {capability.On: false}},
		control: map[string][]control.Entry{"towel-rail": {railEntry}},
	}, rules...)
}

var railHold = Hold{Device: "towel-rail", Capability: capability.On, Value: true, Window: &Window{Start: At{Time: "00:00"}, End: At{Time: "00:00"}}}

func (r *rig) setControl(entries ...control.Entry) {
	metas := map[ident.ID]hub.Meta{}
	for name := range holdDevices {
		metas[ident.ID{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m}
	}
	metas[ident.ID{Namespace: ns, Name: "towel-rail"}] = hub.Meta{Instance: z2m, Control: entries}
	r.store.SetDevices(z2m, metas)
}

func TestEditingControlDuringAWriteKeepsWriting(t *testing.T) {
	r := railRig(t)
	g := &gate{fakeSetter: r.setter, open: make(chan struct{})}
	r.e.Setter = g
	c := holdRule(t, "rail", railHold)
	r.e.Load(c)
	r.eventually("the write to start", func() bool {
		r.e.mu.Lock()
		defer r.e.mu.Unlock()
		ar := r.e.arbs[ident.ID{Namespace: ns, Name: "towel-rail"}][capability.On]
		return ar != nil && ar.writing
	})
	edited := railEntry
	edited.MinimumOff = control.Duration(time.Minute)
	r.setControl(edited)
	r.eventually("the edit to reach the arbiter", func() bool {
		r.e.mu.Lock()
		defer r.e.mu.Unlock()
		ar := r.e.arbs[ident.ID{Namespace: ns, Name: "towel-rail"}][capability.On]
		return ar != nil && ar.spec.MinimumOff == edited.MinimumOff
	})
	close(g.open)
	r.settle()
	r.expect(call{"towel-rail", capability.On, true})
	// The arbiter still writes: removing the hold turns the rail back to its default.
	r.e.Remove(c.Key)
	r.settle()
	r.expect(call{"towel-rail", capability.On, true}, call{"towel-rail", capability.On, false})
}

func TestAReportDuringAWriteIsLookedAtAgain(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.expect(call{"towel-rail", capability.On, true})
	// A wall switch turns it off two seconds after the write: too soon to tell from a stale report.
	r.clock.Step(2 * time.Second)
	r.change("towel-rail", capability.On, false)
	if w := winner(r.status("towel-rail", capability.On)); w != "Rule/rail Normal" {
		t.Fatalf("winner %q straight after", w)
	}
	r.at("2026-10-21 12:01 BST")
	if w := winner(r.status("towel-rail", capability.On)); w != "Device Override" {
		t.Errorf("winner %q once the write settled, want the device's override", w)
	}
}

func TestAWriteLostWithItsDriverIsNotAnOverride(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.expect(call{"towel-rail", capability.On, true})
	dev := ident.ID{Namespace: ns, Name: "towel-rail"}
	snap, _ := r.store.Get(dev)
	r.clock.Step(2 * time.Second)
	r.e.DriverReturned(snap.Instance)
	// The new process reports what the old one never changed, which the store already holds.
	r.e.Discovered(dev, driver.Info{})
	r.eventually("the write again", func() bool { return len(r.setter.got()) == 2 })
	r.at("2026-10-21 12:01 BST")
	if w := winner(r.status("towel-rail", capability.On)); w != "Rule/rail Normal" {
		t.Errorf("winner %q, want the rule's claim to stand", w)
	}
}

func TestAReportAfterALostWriteIsNotAnOverride(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.change("towel-rail", capability.On, true)
	r.at("2026-10-21 12:01 BST")
	dev := ident.ID{Namespace: ns, Name: "towel-rail"}
	claim := control.Claim{Source: "Rule/leak", Level: control.Safety, Value: 0, Expires: r.clock.Now().Add(time.Hour)}
	if _, err := r.e.Claim(context.Background(), dev, capability.On, claim); err != nil {
		t.Fatal(err)
	}
	r.eventually("the Safety write", func() bool { return len(r.setter.got()) == 2 })
	if _, err := r.e.ReleaseClaim(context.Background(), dev, capability.On, claim.Source); err != nil {
		t.Fatal(err)
	}
	r.eventually("the rule's write", func() bool { return len(r.setter.got()) == 3 })
	snap, _ := r.store.Get(dev)
	r.e.DriverReturned(snap.Instance)
	// The rule wrote on: a new process says it's off, which is news to the store.
	r.change("towel-rail", capability.On, false)
	r.at("2026-10-21 12:02 BST")
	if w := winner(r.status("towel-rail", capability.On)); w != "Rule/rail Normal" {
		t.Errorf("winner %q, want the rule's claim to stand", w)
	}
}

func TestAWriteThatFailedIsTriedAgainWhenItsDeviceReports(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.expect(call{"towel-rail", capability.On, true})
	dev := ident.ID{Namespace: ns, Name: "towel-rail"}
	r.failing("towel-rail")
	claim := control.Claim{Source: "Rule/leak", Level: control.Safety, Value: 0, Expires: r.clock.Now().Add(time.Hour)}
	if _, err := r.e.Claim(context.Background(), dev, capability.On, claim); err != nil {
		t.Fatal(err)
	}
	r.eventually("the write that fails", func() bool { return len(r.setter.got()) == 2 })
	r.failing()
	r.e.Discovered(dev, driver.Info{})
	r.eventually("the write again, with no time passing", func() bool { return len(r.setter.got()) == 3 })
}

func TestEditingAHoldsPriorityClaimsAgain(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	safety := railHold
	safety.Priority, safety.Generation = PrioritySafety, 2
	r.e.Load(holdRule(t, "rail", safety))
	r.settle()
	if w := winner(r.status("towel-rail", capability.On)); w != "Rule/rail Safety" {
		t.Errorf("winner %q, want the hold at Safety", w)
	}
}

func TestADeviceThatLeavesKeepsItsClaims(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	controls := r.e.Controls
	r.store.SetDevices(z2m, map[ident.ID]hub.Meta{})
	r.settle() // the saver too: it mustn't delete the state
	if _, ok := controls.Get(ident.ID{Namespace: ns, Name: "towel-rail"}); !ok {
		t.Fatal("a device that left the store lost its saved state")
	}
	r.setControl(railEntry)
	r.store.Discovered(ident.ID{Namespace: ns, Name: "towel-rail"}, driver.Info{Found: true, Capabilities: []string{capability.On}})
	r.settle()
	if w := winner(r.status("towel-rail", capability.On)); w != "Rule/rail Normal" {
		t.Errorf("winner %q after it came back", w)
	}
}

func TestAForgottenDeviceLeavesNoClaims(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	dev := ident.ID{Namespace: ns, Name: "towel-rail"}
	r.store.SetDevices(z2m, map[ident.ID]hub.Meta{})
	r.settle()
	if err := r.e.ForgetDevice(context.Background(), dev); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if _, ok := r.e.Controls.Get(dev); ok {
		t.Fatal("its saved claims stayed for the next device of that name")
	}
}

// matter announces a Matter write, as the matter role does before writing the device.
func (r *rig) matter(dev, c string, v any) {
	r.e.MatterWrote(ident.ID{Namespace: ns, Name: dev}, c, v)
}

func TestAMatterWriteThatFailedLeavesTheDevicesReportItsOwn(t *testing.T) {
	r := railRig(t)
	r.matter("towel-rail", capability.On, true)
	r.e.ForgetMatterWrite(ident.ID{Namespace: ns, Name: "towel-rail"}, capability.On)
	r.clock.Step(2 * time.Second)
	r.change("towel-rail", capability.On, true)
	if w := winner(r.status("towel-rail", capability.On)); w != "Device Override" {
		t.Errorf("winner %q, want the device's own", w)
	}
}

func TestAMatterWriteIsAnOverrideFromMatter(t *testing.T) {
	r := railRig(t)
	r.matter("towel-rail", capability.On, true)
	r.clock.Step(2 * time.Second)
	r.change("towel-rail", capability.On, true)
	s := r.status("towel-rail", capability.On)
	if w := winner(s); w != "Matter Override" || !s.Claims[0].Expires.Equal(local("2026-10-21 13:00 BST").Add(2*time.Second)) {
		t.Errorf("status %+v, want a Matter override for an hour", s)
	}
	r.expect()
	// The write is used up: a wall switch afterwards is the device's.
	r.at("2026-10-21 12:10 BST")
	r.change("towel-rail", capability.On, false)
	r.change("towel-rail", capability.On, true)
	if w := winner(r.status("towel-rail", capability.On)); w != "Device Override" {
		t.Errorf("winner %q after a wall switch, want the device's", w)
	}
}

func TestAMatterWriteDuringARuntimeWriteIsFromMatter(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.expect(call{"towel-rail", capability.On, true})
	// The Home app turns it off two seconds after the engine's write. The report is held back
	// until the write settles, and still matches.
	r.clock.Step(2 * time.Second)
	r.matter("towel-rail", capability.On, false)
	r.change("towel-rail", capability.On, false)
	r.clock.Step(staleReport)
	r.settle()
	if w := winner(r.status("towel-rail", capability.On)); w != "Matter Override" {
		t.Errorf("winner %q once the write settled, want Matter's override", w)
	}
}

func TestChangesThatDontMatchAMatterWriteAreTheDevices(t *testing.T) {
	for name, tc := range map[string]struct {
		device   string
		from, to bool
		after    time.Duration
	}{
		// The rail was already on, and a wall switch turns it off.
		"another value":  {"towel-rail", true, false, time.Second},
		"too late":       {"towel-rail", false, true, bridgeMatch},
		"another device": {"fan", false, true, time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			r := startRigWith(t, rigOptions{
				now:    local("2026-10-21 12:00 BST"),
				values: map[string]map[string]any{"towel-rail": {capability.On: tc.from}},
				// Its default is what it starts as, so the engine doesn't write it.
				control: map[string][]control.Entry{"towel-rail": {
					entry(capability.On, fmt.Sprint(tc.from), control.NamePreferOn, control.NameOverride, "1h"),
				}},
			})
			r.expect()
			r.matter(tc.device, capability.On, true)
			r.clock.Step(tc.after)
			r.change("towel-rail", capability.On, tc.to)
			if w := winner(r.status("towel-rail", capability.On)); w != "Device Override" {
				t.Errorf("winner %q, want the device's override", w)
			}
		})
	}
}

func TestAnOverrideExpiresWithNoRules(t *testing.T) {
	r := railRig(t)
	r.change("towel-rail", capability.On, true)
	r.at("2026-10-21 12:59 BST")
	r.expect()
	r.at("2026-10-21 13:00 BST")
	r.expect(call{"towel-rail", capability.On, false})
}

func TestAMatterDimmerWriteMatchesTheRoundedLevel(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now:    local("2026-10-21 20:00 BST"),
		values: map[string]map[string]any{"hall-light": {capability.Brightness: 0.0}},
		control: map[string][]control.Entry{"hall-light": {
			entry(capability.Brightness, "0", control.NameHighest, control.NameOverride, "1h"),
		}},
	})
	// The Home app's slider passes 10 on its way to 40. The dimmer reports 10.2 as it ramps, then
	// 40.2, as Zigbee2MQTT rounds 40 to 102 of 254.
	r.matter("hall-light", capability.Brightness, 40.0)
	r.change("hall-light", capability.Brightness, 10.2)
	if w := winner(r.status("hall-light", capability.Brightness)); w != "Device Override" {
		t.Errorf("winner %q on the way, want the device's", w)
	}
	r.change("hall-light", capability.Brightness, 40.2)
	if w := winner(r.status("hall-light", capability.Brightness)); w != "Matter Override" {
		t.Errorf("winner %q at 40.2, want Matter's", w)
	}
	r.expect()
}

func TestAMatterWriteThatChangesNothingIsForgotten(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.expect(call{"towel-rail", capability.On, true})
	r.change("towel-rail", capability.On, true)
	// The Home app turns on a rail that's already on: no change will come.
	r.at("2026-10-21 12:01 BST")
	r.matter("towel-rail", capability.On, true)
	// A wall switch turns it off, and on again, soon after.
	r.clock.Step(time.Second)
	r.change("towel-rail", capability.On, false)
	r.change("towel-rail", capability.On, true)
	if w := winner(r.status("towel-rail", capability.On)); w != "Device Override" {
		t.Errorf("winner %q, want the device's", w)
	}
}

// A moment firing all day says once which Room members it leaves out, and again when that changes.
func TestSkippedRoomMembersAreLoggedOnChange(t *testing.T) {
	var logs []string
	e := &Engine{Log: logging.Lines(func(line string) { logs = append(logs, line) }, slog.LevelInfo)}
	key := ident.ID{Name: "bathroom-on"}
	e.reportSkipped(key, []string{"rail", "towel", "rail"})
	e.reportSkipped(key, []string{"towel", "rail"})
	if len(logs) != 1 {
		t.Fatalf("logged %d times: %q", len(logs), logs)
	}
	e.reportSkipped(key, nil)
	e.reportSkipped(key, []string{"rail"})
	if len(logs) != 2 {
		t.Errorf("logged %d times: %q", len(logs), logs)
	}
}

// A room set limited to lights leaves the fridge plug in the same room alone, and says so. A hold
// limited the same way writes only the lights too.
func TestARoomRuleLimitedToTypesLeavesOtherTypesAlone(t *testing.T) {
	r := startRigWith(t, rigOptions{now: local("2026-10-21 22:00 BST")})
	lights := []devicetype.Type{devicetype.OnOffLight, devicetype.DimmableLight}
	metas := map[ident.ID]hub.Meta{
		{Namespace: ns, Name: "hall-motion"}:   {Instance: z2m},
		{Namespace: ns, Name: "floor-lamp"}:    {Instance: z2m, Room: "living-room", DeviceTypes: []devicetype.Type{devicetype.DimmableLight}},
		{Namespace: ns, Name: "ceiling-light"}: {Instance: z2m, Room: "living-room", DeviceTypes: []devicetype.Type{devicetype.OnOffLight}},
		{Namespace: ns, Name: "fridge-plug"}:   {Instance: z2m, Room: "living-room", DeviceTypes: []devicetype.Type{devicetype.OnOffPlugInUnit}},
	}
	r.store.SetDevices(z2m, metas)
	for dev := range metas {
		r.store.Discovered(dev, driver.Info{Found: true, Capabilities: []string{capability.On, capability.Occupied}})
	}
	c := momentRule(t, "living-room-off", Moment{
		Changes: []Change{{Device: "hall-motion", Capability: capability.Occupied}},
		Do:      []Action{Set{Room: "living-room", DeviceTypes: lights, Capability: capability.On, Value: false}},
	})
	r.e.Load(c)
	r.change("hall-motion", capability.Occupied, true)
	r.eventually("the run", func() bool { return len(r.setter.got()) == 2 })
	r.settle()
	r.expect(call{"ceiling-light", capability.On, false}, call{"floor-lamp", capability.On, false})
	r.logs.mu.Lock()
	skippedLogged := slices.ContainsFunc(r.logs.lines, func(l string) bool {
		return strings.Contains(l, "Skipped Room members") && strings.Contains(l, "fridge-plug (OnOffPlugInUnit")
	})
	r.logs.mu.Unlock()
	if !skippedLogged {
		t.Errorf("no log of the skipped fridge plug in %q", r.logs.lines)
	}

	if _, err := NewMoment(ident.ID{Namespace: ns, Name: "bad"}, Moment{
		Changes: []Change{{Device: "hall-motion", Capability: capability.Occupied}},
		Do:      []Action{Set{Device: "fridge-plug", DeviceTypes: lights, Capability: capability.On, Value: false}},
	}); err == nil {
		t.Error("device types on a device, not a room, compiled")
	}
	if _, err := NewHold(ident.ID{Namespace: ns, Name: "bad"}, Hold{
		Room: "living-room", DeviceTypes: []devicetype.Type{devicetype.OccupancySensor}, Capability: capability.On, Value: true,
		Window: &Window{Start: At{Time: "20:00"}, End: At{Time: "23:00"}},
	}); err == nil {
		t.Error("a hold limited to a sensor type compiled")
	}
}

// A plug retyped out of a typed hold while the hold holds it loses the hold's claim when the hold
// ends, rather than keeping it for good.
func TestATypedHoldLetsGoOfAMemberRetypedOutOfIt(t *testing.T) {
	plug := entry(capability.On, "false", control.NamePreferOn, control.NameOverride, "1h")
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 12:00 BST"),
		values:  map[string]map[string]any{"porch-plug": {capability.On: false}, "porch-door": {capability.Open: false}},
		control: map[string][]control.Entry{"porch-plug": {plug}},
	})
	plugs := []devicetype.Type{devicetype.OnOffPlugInUnit}
	metas := func(types ...devicetype.Type) map[ident.ID]hub.Meta {
		return map[ident.ID]hub.Meta{
			{Namespace: ns, Name: "porch-door"}: {Instance: z2m},
			{Namespace: ns, Name: "porch-plug"}: {Instance: z2m, Room: "porch", Control: []control.Entry{plug}, DeviceTypes: types},
		}
	}
	r.store.SetDevices(z2m, metas(plugs...))
	r.store.Discovered(ident.ID{Namespace: ns, Name: "porch-door"}, driver.Info{Found: true, Capabilities: []string{capability.Open}})
	r.store.Discovered(ident.ID{Namespace: ns, Name: "porch-plug"}, driver.Info{Found: true, Capabilities: []string{capability.On}})
	r.settle()
	r.e.Load(holdRule(t, "porch-plugs", Hold{
		Room: "porch", DeviceTypes: plugs, Capability: capability.On, Value: true, Priority: PrioritySafety,
		When: isTrue("porch-door", capability.Open),
	}))
	r.change("porch-door", capability.Open, true)
	r.eventually("the claim", func() bool { return winner(r.status("porch-plug", capability.On)) == "Rule/porch-plugs Safety" })

	r.store.SetDevices(z2m, metas(devicetype.OnOffLight))
	r.change("porch-door", capability.Open, false)
	r.eventually("the claim to go", func() bool {
		return !slices.ContainsFunc(r.status("porch-plug", capability.On).Claims, func(c control.ClaimStatus) bool {
			return c.Source == "Rule/porch-plugs"
		})
	})
}
