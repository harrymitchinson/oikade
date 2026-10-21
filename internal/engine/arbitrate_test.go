package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

// entry is a bool control entry.
func entry(name, def string, resolve homev1alpha1.ControlResolve, external homev1alpha1.ControlExternal, hold string) homev1alpha1.CapabilityControl {
	return homev1alpha1.CapabilityControl{
		Capability: name, Default: apiextensionsv1.JSON{Raw: []byte(def)}, Resolve: resolve, External: external, OverrideHold: hold,
	}
}

func controlStates(t *testing.T, url string) *Retained[ControlState] {
	t.Helper()
	b := busStates(t, url)
	c := &Retained[ControlState]{Bus: b.Bus, Root: ControlStatesRoot, Log: b.Log}
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

// status returns the device's reported control status for one capability.
func (r *rig) status(dev, name string) homev1alpha1.CapabilityControlStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.control[dev] {
		if s.Capability == name {
			return s
		}
	}
	return homev1alpha1.CapabilityControlStatus{}
}

// winner returns the winning claim's source and level, or the default.
func winner(s homev1alpha1.CapabilityControlStatus) string {
	for _, c := range s.Claims {
		if c.Winner {
			return c.Source + " " + string(c.Level)
		}
	}
	return string(s.Level)
}

func TestScenario6WallSwitchOverridesUntilNext(t *testing.T) {
	light := holdRule(t, "hall-motion-light", `
  while: {when: 'devices["hall-motion"]["sensor.occupancy-detected"]', offDelay: 5m}
  hold: {device: hall-light, capability: light.on, value: true}
`)
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 19:59 BST"),
		values:  map[string]map[string]any{"hall-motion": {capability.Occupancy: false}, "hall-light": {capability.LightOn: false}},
		control: map[string][]homev1alpha1.CapabilityControl{"hall-light": {entry(capability.LightOn, "false", homev1alpha1.ControlResolvePreferOn, homev1alpha1.ControlExternalOverride, "Next")}},
	}, light)
	r.expect()
	r.at("2026-10-21 20:00 BST")
	r.change("hall-motion", capability.Occupancy, true)
	r.expect(call{"hall-light", capability.LightOn, true})
	if w := winner(r.status("hall-light", capability.LightOn)); w != "Rule/hall-motion-light Normal" {
		t.Errorf("20:00 winner %q", w)
	}

	// A wall switch turns it off at 20:05: an Override claim from the device.
	r.at("2026-10-21 20:05 BST")
	r.change("hall-light", capability.LightOn, false)
	if w := winner(r.status("hall-light", capability.LightOn)); w != "Device Override" {
		t.Errorf("20:05 winner %q", w)
	}
	// Motion clears at 20:10, and the hold ends at 20:15. The override ends with it.
	r.at("2026-10-21 20:10 BST")
	r.change("hall-motion", capability.Occupancy, false)
	r.at("2026-10-21 20:15 BST")
	if s := r.status("hall-light", capability.LightOn); len(s.Claims) != 0 || s.Level != homev1alpha1.ControlLevelDefault {
		t.Errorf("20:15 status %+v, want the default with no claims", s)
	}
	// The next motion turns it on.
	r.at("2026-10-21 20:20 BST")
	r.change("hall-motion", capability.Occupancy, true)
	r.expect(call{"hall-light", capability.LightOn, true}, call{"hall-light", capability.LightOn, true})
}

func TestScenario7BoostEndsAtTheFirstOfForAndNext(t *testing.T) {
	url := mqtttest.StartBroker(t)
	rail := entry(capability.SwitchOn, "false", homev1alpha1.ControlResolvePreferOn, homev1alpha1.ControlExternalOverride, "Next")
	rules := func() []*homev1alpha1.Rule {
		evening := holdRule(t, "towel-rail-evenings", `
  while: {window: {start: "18:00", end: "21:00"}}
  hold: {device: towel-rail, capability: switch.on, value: true}
`)
		boost := holdRule(t, "towel-rail-boost", `
  triggers: [{at: {time: "21:30"}}]
  do: [{set: {device: towel-rail, capability: switch.on, value: true, for: 1h, until: Next}}]
`)
		boost.CreationTimestamp = metav1.NewTime(local("2026-10-21 12:00 BST"))
		return []*homev1alpha1.Rule{evening, boost}
	}
	start := func(now string, on bool) *rig {
		return startRigWith(t, rigOptions{
			now: local(now), holds: busStates(t, url), controls: controlStates(t, url),
			values:  map[string]map[string]any{"towel-rail": {capability.SwitchOn: on}},
			control: map[string][]homev1alpha1.CapabilityControl{"towel-rail": {rail}},
		}, rules()...)
	}
	r := start("2026-10-21 17:59 BST", false)
	r.at("2026-10-21 18:00 BST")
	r.at("2026-10-21 21:00 BST")
	r.at("2026-10-21 21:30 BST")
	s := r.status("towel-rail", capability.SwitchOn)
	if w := winner(s); w != "Rule/towel-rail-boost Normal" || !s.Claims[0].Expires.Time.Equal(local("2026-10-21 22:30 BST")) {
		t.Errorf("21:30 status %+v", s)
	}
	r.at("2026-10-21 21:45 BST")
	r.stop()

	// The boost's claim and expiry survive a restart.
	r = start("2026-10-21 21:45 BST", true)
	r.expect()
	r.at("2026-10-21 22:30 BST")
	r.expect(call{"towel-rail", capability.SwitchOn, false})
}

func TestScenario8LeakClosesTheValveUntilReleased(t *testing.T) {
	url := mqtttest.StartBroker(t)
	valve := entry(capability.SwitchOn, "true", homev1alpha1.ControlResolvePreferOff, homev1alpha1.ControlExternalRevert, "")
	shutoff := func() *homev1alpha1.Rule {
		return holdRule(t, "leak-shutoff", `
  priority: Safety
  while: {when: 'devices["leak-sensor"]["sensor.contact-open"]', latch: true}
  hold: {device: main-valve, capability: switch.on, value: false}
`)
	}
	reset := holdRule(t, "leak-reset", `
  triggers: [{at: {time: "16:00"}}]
  do: [{release: {rule: leak-shutoff}}]
`)
	reset.CreationTimestamp = metav1.NewTime(local("2026-10-21 12:00 BST"))
	start := func(now string, open bool, rules ...*homev1alpha1.Rule) *rig {
		return startRigWith(t, rigOptions{
			now: local(now), holds: busStates(t, url), controls: controlStates(t, url),
			values:  map[string]map[string]any{"leak-sensor": {capability.ContactOpen: false}, "main-valve": {capability.SwitchOn: open}},
			control: map[string][]homev1alpha1.CapabilityControl{"main-valve": {valve}},
		}, rules...)
	}
	r := start("2026-10-21 13:59 BST", true, shutoff())
	r.expect()
	r.at("2026-10-21 14:00 BST")
	r.change("leak-sensor", capability.ContactOpen, true)
	r.expect(call{"main-valve", capability.SwitchOn, false})
	r.change("main-valve", capability.SwitchOn, false)

	// The Home app opens it at 14:05, and the runtime closes it again.
	r.at("2026-10-21 14:05 BST")
	r.change("main-valve", capability.SwitchOn, true)
	r.expect(call{"main-valve", capability.SwitchOn, false}, call{"main-valve", capability.SwitchOn, false})
	s := r.status("main-valve", capability.SwitchOn)
	if w := winner(s); w != "Rule/leak-shutoff Safety" || !s.Claims[0].Latched || !s.Claims[len(s.Claims)-1].Reverted {
		t.Errorf("14:05 status %+v", s)
	}
	// It dries at 15:00, and stays closed: it's latched.
	r.at("2026-10-21 15:00 BST")
	r.change("leak-sensor", capability.ContactOpen, false)
	r.at("2026-10-21 15:30 BST")
	r.stop()

	// A restart keeps it closed, until the reset at 16:00.
	r = start("2026-10-21 15:30 BST", false, shutoff(), reset)
	r.expect()
	r.at("2026-10-21 16:00 BST")
	r.expect(call{"main-valve", capability.SwitchOn, true})
}

func TestScenario5LimitsHoldThePumpOn(t *testing.T) {
	pump := entry(capability.SwitchOn, "false", homev1alpha1.ControlResolvePreferOn, homev1alpha1.ControlExternalOverride, "2h")
	pump.MinimumOn = &metav1.Duration{Duration: time.Hour}
	pump.MinimumOff = &metav1.Duration{Duration: 30 * time.Minute}
	pump.MaximumStartsPerDay = new(int32(4))
	slot := holdRule(t, "pump-slot", `
  while: {window: {start: "22:00", end: "02:00"}}
  hold: {device: pump, capability: switch.on, value: true}
`)
	frost := holdRule(t, "pump-frost", `
  priority: Safety
  while: {when: 'devices["workshop-temp"]["sensor.temperature"] < 0'}
  hold: {device: pump, capability: switch.on, value: true}
`)
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-22 01:00 BST"),
		values:  map[string]map[string]any{"pump": {capability.SwitchOn: true}, "workshop-temp": {capability.Temperature: 5.0}},
		control: map[string][]homev1alpha1.CapabilityControl{"pump": {pump}},
	}, slot, frost)
	r.expect()
	// A Home app tap turns it off at 01:40: an Override until 03:40.
	r.at("2026-10-22 01:40 BST")
	r.change("pump", capability.SwitchOn, false)
	r.at("2026-10-22 02:50 BST")
	r.change("workshop-temp", capability.Temperature, -1.0)
	r.expect(call{"pump", capability.SwitchOn, true})
	// Frost ends at 03:30. The Override (off) wins, but minimumOn keeps it on until 03:50.
	r.at("2026-10-22 03:30 BST")
	r.change("workshop-temp", capability.Temperature, 1.0)
	r.expect(call{"pump", capability.SwitchOn, true})
	r.at("2026-10-22 03:49 BST")
	r.expect(call{"pump", capability.SwitchOn, true})
	r.at("2026-10-22 03:50 BST")
	r.expect(call{"pump", capability.SwitchOn, true}, call{"pump", capability.SwitchOn, false})
}

func TestStatusChangesAtTransitionsOnly(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 12:00 BST"),
		values:  map[string]map[string]any{"towel-rail": {capability.SwitchOn: false}},
		control: map[string][]homev1alpha1.CapabilityControl{"towel-rail": {entry(capability.SwitchOn, "false", homev1alpha1.ControlResolvePreferOn, homev1alpha1.ControlExternalOverride, "1h")}},
	})
	r.mu.Lock()
	before := r.reports
	r.mu.Unlock()
	for range 5 {
		r.change("towel-rail", capability.SwitchOn, false)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reports != before {
		t.Errorf("reported %d more times for the same value", r.reports-before)
	}
}

func TestEndlessSetsSkipArbitratedDevices(t *testing.T) {
	rail := entry(capability.SwitchOn, "false", homev1alpha1.ControlResolvePreferOn, homev1alpha1.ControlExternalOverride, "1h")
	r := startRigWith(t, rigOptions{
		now:     local("2026-10-21 12:00 BST"),
		values:  map[string]map[string]any{"towel-rail": {capability.SwitchOn: false}},
		control: map[string][]homev1alpha1.CapabilityControl{"towel-rail": {rail}},
	})
	metas := map[types.NamespacedName]hub.Meta{
		{Namespace: ns, Name: "towel-rail"}:  {Instance: z2m, Control: []homev1alpha1.CapabilityControl{rail}},
		{Namespace: ns, Name: "hall-motion"}: {Instance: z2m},
		{Namespace: ns, Name: "bath-rail"}:   {Instance: z2m, Room: "bathroom", Control: []homev1alpha1.CapabilityControl{rail}},
		{Namespace: ns, Name: "bath-fan"}:    {Instance: z2m, Room: "bathroom"},
	}
	r.store.SetDevices(z2m, metas)
	for dev := range metas {
		r.store.Discovered(dev, driver.Info{Found: true, Capabilities: []string{capability.SwitchOn, capability.Occupancy}})
	}
	c, err := Compile(holdRule(t, "bathroom-on", `
  triggers: [{change: {device: hall-motion, capability: sensor.occupancy-detected}}]
  do:
  - set: {room: bathroom, capability: switch.on, value: true}
  - set: {device: towel-rail, capability: switch.on, value: true}
  - set: {room: bathroom, capability: switch.on, value: true, for: 10m}
`))
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	r.change("hall-motion", capability.Occupancy, true)
	r.eventually("the skipped report", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.skipped["bathroom-on"]) > 0
	})
	r.settle()
	// The bath rail gets its default when it's discovered with no value. Then the endless sets
	// write the fan and skip both rails, and the timed one claims the bath rail.
	r.expect(call{"bath-rail", capability.SwitchOn, false}, call{"bath-fan", capability.SwitchOn, true},
		call{"bath-fan", capability.SwitchOn, true}, call{"bath-rail", capability.SwitchOn, true})
	r.mu.Lock()
	defer r.mu.Unlock()
	if got := strings.Join(r.skipped["bathroom-on"], ","); got != "bath-rail" {
		t.Errorf("skipped %q, want bath-rail", got)
	}
}

func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	for range 300 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

// gate holds every Set until it's opened.
type gate struct {
	*fakeSetter
	open chan struct{}
}

func (g *gate) Set(ctx context.Context, dev types.NamespacedName, c string, v any) error {
	<-g.open
	return g.fakeSetter.Set(ctx, dev, c, v)
}

var railEntry = entry(capability.SwitchOn, "false", homev1alpha1.ControlResolvePreferOn, homev1alpha1.ControlExternalOverride, "1h")

func railRig(t *testing.T, rules ...*homev1alpha1.Rule) *rig {
	return startRigWith(t, rigOptions{
		now:     local("2026-10-21 12:00 BST"),
		values:  map[string]map[string]any{"towel-rail": {capability.SwitchOn: false}},
		control: map[string][]homev1alpha1.CapabilityControl{"towel-rail": {railEntry}},
	}, rules...)
}

var railHold = `
  while: {window: {start: "00:00", end: "00:00"}}
  hold: {device: towel-rail, capability: switch.on, value: true}
`

func (r *rig) setControl(entries ...homev1alpha1.CapabilityControl) {
	metas := map[types.NamespacedName]hub.Meta{}
	for name := range holdDevices {
		metas[types.NamespacedName{Namespace: ns, Name: name}] = hub.Meta{Instance: z2m}
	}
	metas[types.NamespacedName{Namespace: ns, Name: "towel-rail"}] = hub.Meta{Instance: z2m, Control: entries}
	r.store.SetDevices(z2m, metas)
}

func TestEditingControlDuringAWriteKeepsWriting(t *testing.T) {
	r := railRig(t)
	g := &gate{fakeSetter: r.setter, open: make(chan struct{})}
	r.e.Setter = g
	c, err := Compile(holdRule(t, "rail", railHold))
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	r.eventually("the write to start", func() bool {
		r.e.mu.Lock()
		defer r.e.mu.Unlock()
		ar := r.e.arbs[types.NamespacedName{Namespace: ns, Name: "towel-rail"}][capability.SwitchOn]
		return ar != nil && ar.writing
	})
	edited := railEntry
	edited.MinimumOff = &metav1.Duration{Duration: time.Minute}
	r.setControl(edited)
	time.Sleep(50 * time.Millisecond)
	close(g.open)
	r.settle()
	r.expect(call{"towel-rail", capability.SwitchOn, true})
	// The arbiter still writes: removing the hold turns the rail back to its default.
	r.e.Remove(c.Key)
	r.settle()
	r.expect(call{"towel-rail", capability.SwitchOn, true}, call{"towel-rail", capability.SwitchOn, false})
}

func TestAReportDuringAWriteIsLookedAtAgain(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.expect(call{"towel-rail", capability.SwitchOn, true})
	// A wall switch turns it off two seconds after the write: too soon to tell from a stale report.
	r.clock.Step(2 * time.Second)
	r.change("towel-rail", capability.SwitchOn, false)
	if w := winner(r.status("towel-rail", capability.SwitchOn)); w != "Rule/rail Normal" {
		t.Fatalf("winner %q straight after", w)
	}
	r.at("2026-10-21 12:01 BST")
	if w := winner(r.status("towel-rail", capability.SwitchOn)); w != "Device Override" {
		t.Errorf("winner %q once the write settled, want the device's override", w)
	}
}

func TestEditingAHoldsPriorityClaimsAgain(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	safety := holdRule(t, "rail", "  priority: Safety\n"+railHold)
	safety.Generation = 2
	c, err := Compile(safety)
	if err != nil {
		t.Fatal(err)
	}
	r.e.Load(c)
	r.settle()
	if w := winner(r.status("towel-rail", capability.SwitchOn)); w != "Rule/rail Safety" {
		t.Errorf("winner %q, want the hold at Safety", w)
	}
}

func TestADeviceThatLeavesKeepsItsClaims(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	controls := r.e.Controls
	r.store.SetDevices(z2m, map[types.NamespacedName]hub.Meta{})
	r.settle()
	// Give the saver its turn: it mustn't delete the state.
	time.Sleep(100 * time.Millisecond)
	if _, ok := controls.Get(types.NamespacedName{Namespace: ns, Name: "towel-rail"}); !ok {
		t.Fatal("a device that left the store lost its saved state")
	}
	r.setControl(railEntry)
	r.store.Discovered(types.NamespacedName{Namespace: ns, Name: "towel-rail"}, driver.Info{Found: true, Capabilities: []string{capability.SwitchOn}})
	r.settle()
	if w := winner(r.status("towel-rail", capability.SwitchOn)); w != "Rule/rail Normal" {
		t.Errorf("winner %q after it came back", w)
	}
}

// homekit announces a HomeKit write, as hap does before writing the device.
func (r *rig) homekit(dev, c string, v any) {
	r.e.HomeKitWrote(types.NamespacedName{Namespace: ns, Name: dev}, c, v)
}

func TestAHomeKitWriteIsAnOverrideFromHomeKit(t *testing.T) {
	r := railRig(t)
	r.homekit("towel-rail", capability.SwitchOn, true)
	r.clock.Step(2 * time.Second)
	r.change("towel-rail", capability.SwitchOn, true)
	s := r.status("towel-rail", capability.SwitchOn)
	if w := winner(s); w != "HomeKit Override" || !s.Claims[0].Expires.Time.Equal(local("2026-10-21 13:00 BST").Add(2*time.Second)) {
		t.Errorf("status %+v, want a HomeKit override for an hour", s)
	}
	r.expect()
	// The write is used up: a wall switch afterwards is the device's.
	r.at("2026-10-21 12:10 BST")
	r.change("towel-rail", capability.SwitchOn, false)
	r.change("towel-rail", capability.SwitchOn, true)
	if w := winner(r.status("towel-rail", capability.SwitchOn)); w != "Device Override" {
		t.Errorf("winner %q after a wall switch, want the device's", w)
	}
}

func TestAHomeKitWriteDuringARuntimeWriteIsFromHomeKit(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.expect(call{"towel-rail", capability.SwitchOn, true})
	// The Home app turns it off two seconds after the runtime's write. The report is held back
	// until the write settles, and still matches.
	r.clock.Step(2 * time.Second)
	r.homekit("towel-rail", capability.SwitchOn, false)
	r.change("towel-rail", capability.SwitchOn, false)
	r.clock.Step(staleReport)
	r.settle()
	if w := winner(r.status("towel-rail", capability.SwitchOn)); w != "HomeKit Override" {
		t.Errorf("winner %q once the write settled, want HomeKit's override", w)
	}
}

func TestChangesThatDontMatchAHomeKitWriteAreTheDevices(t *testing.T) {
	for name, tc := range map[string]struct {
		device   string
		from, to bool
		after    time.Duration
	}{
		// The rail was already on, and a wall switch turns it off.
		"another value":  {"towel-rail", true, false, time.Second},
		"too late":       {"towel-rail", false, true, homekitMatch},
		"another device": {"fan", false, true, time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			r := startRigWith(t, rigOptions{
				now:    local("2026-10-21 12:00 BST"),
				values: map[string]map[string]any{"towel-rail": {capability.SwitchOn: tc.from}},
				// Its default is what it starts as, so the runtime doesn't write it.
				control: map[string][]homev1alpha1.CapabilityControl{"towel-rail": {
					entry(capability.SwitchOn, fmt.Sprint(tc.from), homev1alpha1.ControlResolvePreferOn, homev1alpha1.ControlExternalOverride, "1h"),
				}},
			})
			r.expect()
			r.homekit(tc.device, capability.SwitchOn, true)
			r.clock.Step(tc.after)
			r.change("towel-rail", capability.SwitchOn, tc.to)
			if w := winner(r.status("towel-rail", capability.SwitchOn)); w != "Device Override" {
				t.Errorf("winner %q, want the device's override", w)
			}
		})
	}
}

func TestAnOverrideExpiresWithNoRules(t *testing.T) {
	r := railRig(t)
	r.change("towel-rail", capability.SwitchOn, true)
	r.at("2026-10-21 12:59 BST")
	r.expect()
	r.at("2026-10-21 13:00 BST")
	r.expect(call{"towel-rail", capability.SwitchOn, false})
}

func TestAHomeKitDimmerWriteMatchesTheRoundedLevel(t *testing.T) {
	r := startRigWith(t, rigOptions{
		now:    local("2026-10-21 20:00 BST"),
		values: map[string]map[string]any{"hall-light": {capability.LightLevel: 0.0}},
		control: map[string][]homev1alpha1.CapabilityControl{"hall-light": {
			entry(capability.LightLevel, "0", homev1alpha1.ControlResolveHighest, homev1alpha1.ControlExternalOverride, "1h"),
		}},
	})
	// The Home app's slider passes 10 on its way to 40. The dimmer reports 10.2 as it ramps, then
	// 40.2, as Zigbee2MQTT rounds 40 to 102 of 254.
	r.homekit("hall-light", capability.LightLevel, 40.0)
	r.change("hall-light", capability.LightLevel, 10.2)
	if w := winner(r.status("hall-light", capability.LightLevel)); w != "Device Override" {
		t.Errorf("winner %q on the way, want the device's", w)
	}
	r.change("hall-light", capability.LightLevel, 40.2)
	if w := winner(r.status("hall-light", capability.LightLevel)); w != "HomeKit Override" {
		t.Errorf("winner %q at 40.2, want HomeKit's", w)
	}
	r.expect()
}

func TestAHomeKitWriteThatChangesNothingIsForgotten(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	r.expect(call{"towel-rail", capability.SwitchOn, true})
	r.change("towel-rail", capability.SwitchOn, true)
	// The Home app turns on a rail that's already on: no change will come.
	r.at("2026-10-21 12:01 BST")
	r.homekit("towel-rail", capability.SwitchOn, true)
	// A wall switch turns it off, and on again, soon after.
	r.clock.Step(time.Second)
	r.change("towel-rail", capability.SwitchOn, false)
	r.change("towel-rail", capability.SwitchOn, true)
	if w := winner(r.status("towel-rail", capability.SwitchOn)); w != "Device Override" {
		t.Errorf("winner %q, want the device's", w)
	}
}
