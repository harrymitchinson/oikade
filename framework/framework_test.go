package framework

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

func TestCompileRejectsAnUnsoundHome(t *testing.T) {
	when := func(State) bool { return true }
	for name, tc := range map[string]struct {
		home Home
		want string
	}{
		"no name": {Home{Holds: []Hold{{Device: "d", Capability: "switch.on", Value: true, When: when}}}, "needs a name"},
		"twice": {Home{
			Holds:   []Hold{{Name: "a", Device: "d", Capability: "switch.on", Value: true, When: when}},
			Moments: []Moment{{Name: "a", At: []string{"07:00"}, Do: []Action{Wait(time.Second)}}},
		}, "declared twice"},
		"unknown capability": {
			Home{Holds: []Hold{{Name: "a", Device: "d", Capability: "nope", Value: true, When: when}}}, "unknown capability",
		},
		"no trigger": {Home{Moments: []Moment{{Name: "a", Do: []Action{Wait(time.Second)}}}}, "trigger"},
		"nil action": {Home{Moments: []Moment{{Name: "a", At: []string{"07:00"}, Do: []Action{nil}}}}, "no action"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := tc.home.compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

func TestConditionSeesTheHome(t *testing.T) {
	now := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	cond := Condition(func(s State) bool {
		return s.Bool("door", "sensor.contact-open") && s.Number("room", "sensor.temperature") > 20 && s.Now().Equal(now)
	}).toEngine()
	ok, err := cond(engine.Vars{"now": now, "devices": map[string]map[string]any{
		"door": {"sensor.contact-open": true},
		"room": {"sensor.temperature": 21.5},
	}})
	if err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}

// A hold has to keep its state until the devices it reads have reported, so that isn't false.
func TestConditionReadingAnUnreportedValueIsAnError(t *testing.T) {
	cond := Condition(func(s State) bool { return !s.Bool("door", "sensor.contact-open") }).toEngine()
	devices := map[string]map[string]any{"door": {}}
	if _, err := cond(engine.Vars{"devices": devices}); err == nil {
		t.Fatal("got no error")
	}
}

// A device that doesn't exist will never report, so it mustn't pass for one that hasn't yet. The
// engine recovers this panic and logs it.
func TestConditionReadingAnUndeclaredDevicePanics(t *testing.T) {
	cond := Condition(func(s State) bool { return s.Bool("dor", "sensor.contact-open") }).toEngine()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "no device dor") {
			t.Fatalf("got %v", r)
		}
	}()
	_, _ = cond(engine.Vars{"devices": map[string]map[string]any{"door": {}}})
}

func TestDiscoveredAddsADeviceNewSinceTheWelcome(t *testing.T) {
	mirror := hub.NewStore(logr.Discard())
	metas := map[ident.ID]hub.Meta{}
	discovered(mirror, metas, nil, wire.Device{
		Name: "lamp", Room: "hall", Arbitrated: []string{"switch.on"}, Discovered: true,
		Capabilities: []string{"switch.on"}, Values: map[string]any{"switch.on": true},
	})
	snap, ok := mirror.Get(deviceKey("lamp"))
	if !ok || !snap.Discovered || snap.Room != "hall" || !snap.Controls("switch.on") || snap.Values["switch.on"] != true {
		t.Fatalf("got %+v, %v", snap, ok)
	}
	changed(mirror, wire.Message{Device: "lamp", Capability: "switch.on", Value: false})
	if snap, _ := mirror.Get(deviceKey("lamp")); snap.Values["switch.on"] != false {
		t.Fatalf("a later change didn't reach it: %+v", snap)
	}
}

func TestManifestDescribesTheHome(t *testing.T) {
	when := func(State) bool { return true }
	m := Home{
		Holds: []Hold{{Name: "leak", Priority: Safety, Latch: true, When: when}, {Name: "fan", When: when}},
		Moments: []Moment{
			{Name: "alarm", Do: []Action{Set{Priority: Safety}}},
			{Name: "lamp", Do: []Action{Set{}, Wait(time.Second)}},
		},
		Flows: []Flow{{Name: "shutoff", Priority: Safety}, {Name: "bedtime"}},
	}.manifest()
	if m.Protocol != wire.Version {
		t.Fatalf("protocol %d", m.Protocol)
	}
	want := []wire.RuleInfo{
		{Name: "leak", Kind: "hold", Safety: true, Latch: true}, {Name: "fan", Kind: "hold"},
		{Name: "alarm", Kind: "moment", Safety: true}, {Name: "lamp", Kind: "moment"},
		{Name: "shutoff", Kind: "flow", Safety: true}, {Name: "bedtime", Kind: "flow"},
	}
	if !slices.Equal(m.Rules, want) {
		t.Fatalf("got %+v", m.Rules)
	}
}

func TestPluginsAreChecked(t *testing.T) {
	made := func(plugin.Environment, []contractv1.DeclaredDevice) (plugin.Handler, error) { return nil, nil }
	for name, plugins := range map[string][]Plugin{
		"no name":  {{New: made}},
		"bad name": {{Name: "a/b", New: made}},
		"twice":    {{Name: "a", New: made}, {Name: "a", New: made}},
		"no New":   {{Name: "a"}},
	} {
		if _, _, err := (Home{Plugins: plugins}).compile(); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	if m := (Home{Plugins: []Plugin{{Name: "a", New: made}}}).manifest(); !slices.Equal(m.Plugins, []string{"a"}) {
		t.Errorf("got %v", m.Plugins)
	}
}

func valveControl() Control {
	return Control{
		Device: "pump", Capability: "switch.on", Default: false, Resolve: PreferOn,
		External: ExternalOverride, OverrideHold: time.Hour, MinimumOn: time.Hour, MinimumOff: 30 * time.Minute,
	}
}

func budgetHome(c Control, b Budget) Home {
	return Home{
		Control: []Control{c},
		Holds:   []Hold{{Name: "pump-daily", Device: "pump", Capability: "switch.on", Value: true, Budget: &b}},
	}
}

func TestHomeChecksControlAndBudgets(t *testing.T) {
	minutes := func(State) int { return 120 }
	limited := valveControl()
	limited.MinimumOn = 45 * time.Minute
	control := func(c Control) Home { return Home{Control: []Control{c}} }
	for name, tc := range map[string]struct {
		home Home
		want string
	}{
		"fine": {budgetHome(valveControl(), Budget{Minutes: minutes}), ""},
		"bands": {budgetHome(valveControl(), Budget{Bands: &Bands{
			Device: "water", Capability: "sensor.temperature", Ranges: []Band{{Below: 12, Minutes: 60}}, Otherwise: 90,
		}}), ""},
		"no minutes":   {budgetHome(valveControl(), Budget{}), "exactly one of bands or minutes"},
		"both minutes": {budgetHome(valveControl(), Budget{Minutes: minutes, Bands: &Bands{}}), "exactly one"},
		"bad slot":     {budgetHome(valveControl(), Budget{Minutes: minutes, SlotMinutes: 20}), "slotMinutes"},
		"bad window": {
			budgetHome(valveControl(), Budget{Minutes: minutes, Forbid: []Window{{Start: "7", End: "8"}}}), "not HH:MM",
		},
		"limit off the slots": {budgetHome(limited, Budget{Minutes: minutes}), "isn't a multiple"},
		"no control": {Home{Holds: []Hold{{
			Name: "pump-daily", Device: "pump", Capability: "switch.on", Value: true, Budget: &Budget{Minutes: minutes},
		}}}, "needs a Control"},
		"twice": {Home{Control: []Control{valveControl(), valveControl()}}, "declared twice"},
		"no device": {control(Control{
			Capability: "switch.on", Default: false, Resolve: PreferOn, External: ExternalRevert,
		}), "a device is required"},
		"bool resolve": {control(Control{
			Device: "x", Capability: "switch.on", Default: false, Resolve: Highest, External: ExternalRevert,
		}), "PreferOn or PreferOff"},
		"read-only": {control(Control{
			Device: "x", Capability: "sensor.temperature", Default: 1.0, Resolve: Highest, External: ExternalRevert,
		}), "read-only"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := tc.home.compile()
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestBudgetMinutesThatFailEndAsAnError(t *testing.T) {
	devices := map[string]map[string]any{"water": {}}
	for name, f := range map[string]func(State) int{
		"hasn't reported": func(s State) int { return int(s.Number("water", "sensor.temperature")) },
		"no such device":  func(s State) int { return int(s.Number("nothing", "sensor.temperature")) },
		"a bug":           func(State) int { panic("oops") },
	} {
		t.Run(name, func(t *testing.T) {
			spec := (&Budget{Minutes: f}).toSchedule()
			if _, err := spec.Minutes(devices, time.Now(), time.UTC); err == nil {
				t.Fatal("no error")
			}
		})
	}
	spec := (&Budget{Minutes: func(State) int { return 90 }}).toSchedule()
	if got, err := spec.Minutes(devices, time.Now(), time.UTC); err != nil || got != 90 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestMetaHasTheLimitsOfTheHomesControl(t *testing.T) {
	h := Home{Control: []Control{valveControl()}}
	settings, err := h.settings()
	if err != nil {
		t.Fatal(err)
	}
	m := meta(wire.Device{Name: "pump", Arbitrated: []string{"switch.on", "light.on"}}, settings)
	if len(m.Control) != 2 || time.Duration(m.Control[0].MinimumOn) != time.Hour || m.Control[1].Capability != "light.on" {
		t.Fatalf("got %+v", m.Control)
	}
}
