package framework

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

func TestCompileRejectsAnUnsoundHome(t *testing.T) {
	when := func(State) bool { return true }
	d := Handle[bool]{Device: "d", Capability: On}
	for name, tc := range map[string]struct {
		home Home
		want string
	}{
		"no name": {Home{Holds: []Hold{{Set: d.To(true), When: when}}}, "needs a name"},
		"twice": {Home{
			Holds:   []Hold{{Name: "a", Set: d.To(true), When: when}},
			Moments: []Moment{{Name: "a", At: []Time{Clock("07:00")}, Do: []Action{Wait(time.Second)}}},
		}, "declared twice"},
		"unknown capability": {
			Home{Holds: []Hold{{Name: "a", Set: Handle[bool]{Device: "d", Capability: "nope"}.To(true), When: when}}},
			"unknown capability",
		},
		"no trigger": {Home{Moments: []Moment{{Name: "a", Do: []Action{Wait(time.Second)}}}}, "trigger"},
		"nil action": {Home{Moments: []Moment{{Name: "a", At: []Time{Clock("07:00")}, Do: []Action{nil}}}}, "no action"},
		"no set":     {Home{Holds: []Hold{{Name: "a", When: when}}}, "needs a capability"},
		"claim": {
			Home{Holds: []Hold{{Name: "a", Set: d.To(true).For(time.Hour), When: when}}}, "takes no For, Until or Priority",
		},
		"nil change": {
			Home{Moments: []Moment{{Name: "a", On: []Change{nil}, Do: []Action{Wait(time.Second)}}}}, "a nil change, at On[0]",
		},
		"a time that isn't one": {
			Home{Moments: []Moment{{Name: "bedtime", At: []Time{Clock("25:00")}, Do: []Action{Wait(time.Second)}}}},
			`rule bedtime: timers[0]: time "25:00" is not HH:MM`,
		},
		"a window with no end": {
			Home{Holds: []Hold{{Name: "evening", Set: d.To(true), Window: &Window{Start: Clock("18:00")}}}}, "end: no time",
		},
		"both untils": {
			Home{Moments: []Moment{{
				Name: "a", At: []Time{Clock("07:00")}, Do: []Action{d.To(true).UntilReleased().UntilNext()},
			}}},
			"not both",
		},
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
	door := Handle[bool]{Device: "door", Capability: Open}
	room := Handle[float64]{Device: "room", Capability: Temperature}
	cond := Condition(func(s State) bool {
		return s.Get(door) && s.Get(room) > 20 && s.Now().Equal(now)
	}).toEngine()
	ok, err := cond(engine.Vars{"now": now, "devices": map[string]map[string]any{
		"door": {"open": true},
		"room": {"temperature": 21.5},
	}})
	if err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}

// A hold has to keep its state until the devices it reads have reported, so that isn't false.
func TestConditionReadingAnUnreportedValueIsAnError(t *testing.T) {
	door := Handle[bool]{Device: "door", Capability: Open}
	cond := Condition(func(s State) bool { return !s.Get(door) }).toEngine()
	devices := map[string]map[string]any{"door": {}}
	if _, err := cond(engine.Vars{"devices": devices}); err == nil {
		t.Fatal("got no error")
	}
}

// A device that doesn't exist will never report, so it mustn't pass for one that hasn't yet. The
// engine recovers this panic and logs it.
func TestConditionReadingAnUndeclaredDevicePanics(t *testing.T) {
	cond := Condition(func(s State) bool { return s.Get(Handle[bool]{Device: "dor", Capability: Open}) }).toEngine()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "no device dor") {
			t.Fatalf("got %v", r)
		}
	}()
	_, _ = cond(engine.Vars{"devices": map[string]map[string]any{"door": {}}})
}

func TestLookupCarriesOnWithoutAValue(t *testing.T) {
	cellar := Handle[float64]{Device: "cellar", Capability: Humidity}
	attic := Handle[float64]{Device: "attic", Capability: Humidity}
	cond := Condition(func(s State) bool {
		if _, ok := s.Lookup(cellar); ok {
			t.Error("Lookup found a value nobody reported")
		}
		v, ok := s.Lookup(attic)
		return ok && v > 60
	}).toEngine()
	ok, err := cond(engine.Vars{"devices": map[string]map[string]any{"cellar": {}, "attic": {"humidity": 70.0}}})
	if err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}

func TestLookupOfAnUndeclaredDevicePanics(t *testing.T) {
	cond := Condition(func(s State) bool {
		_, ok := s.Lookup(Handle[bool]{Device: "dor", Capability: Open})
		return ok
	}).toEngine()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "no device dor") {
			t.Fatalf("got %v", r)
		}
	}()
	_, _ = cond(engine.Vars{"devices": map[string]map[string]any{"door": {}}})
}

// hallway has three motion sensors, one of which has never reported, a light and a plug, and the
// kitchen has a sensor that's occupied.
func hallway() engine.Vars {
	return engine.Vars{
		"devices": map[string]map[string]any{
			"hall-motion-1": {"occupied": false},
			"hall-motion-2": {"occupied": true},
			"hall-motion-3": {},
			"hall-light":    {"on": true},
			"hall-plug":     {"on": false},
			"kitchen":       {"occupied": true},
			"cellar":        {},
			"porch":         {"occupied": true},
		},
		"members": map[string]hub.Member{
			"hall-motion-1": {Room: "hallway"},
			"hall-motion-2": {Room: "hallway"},
			"hall-motion-3": {Room: "hallway"},
			"hall-light":    {Room: "hallway", DeviceTypes: []devicetype.Type{devicetype.Type(OnOffLight)}},
			"hall-plug":     {Room: "hallway", DeviceTypes: []devicetype.Type{devicetype.Type(OnOffPlugInUnit)}},
			"kitchen":       {Room: "kitchen"},
			"cellar":        {Room: "cellar"},
			"porch":         {},
		},
	}
}

func TestRoomReadsCoverTheDevicesThatHaveReported(t *testing.T) {
	for name, tc := range map[string]struct {
		cond Condition
		want bool
	}{
		"any":         {func(s State) bool { return s.Any(InRoom("hallway", Occupied)) }, true},
		"all":         {func(s State) bool { return s.All(InRoom("hallway", Occupied)) }, false},
		"count":       {func(s State) bool { return s.Count(InRoom("hallway", Occupied)) == 1 }, true},
		"all on":      {func(s State) bool { return s.All(InRoom("hallway", On)) }, false},
		"lights only": {func(s State) bool { return s.All(InRoom("hallway", On).Types(OnOffLight, DimmableLight)) }, true},
		"plugs only":  {func(s State) bool { return s.Any(InRoom("hallway", On).Types(OnOffPlugInUnit)) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := tc.cond.toEngine()(hallway())
			if err != nil || ok != tc.want {
				t.Fatalf("got %v, %v; want %v", ok, err, tc.want)
			}
		})
	}
}

func TestReadingsAreInDeviceNameOrder(t *testing.T) {
	var got []float64
	cond := Condition(func(s State) bool {
		got = s.Readings(InRoom("cellar", Humidity))
		return slices.Max(got) > 65
	}).toEngine()
	ok, err := cond(engine.Vars{
		"devices": map[string]map[string]any{"b": {"humidity": 70.0}, "a": {"humidity": 50.0}, "c": {}},
		"members": map[string]hub.Member{"a": {Room: "cellar"}, "b": {Room: "cellar"}, "c": {Room: "cellar"}},
	})
	if err != nil || !ok || !slices.Equal(got, []float64{50, 70}) {
		t.Fatalf("got %v, %v, %v", ok, err, got)
	}
}

// A hold keeps its state while it can't tell, as with a single device that hasn't reported.
func TestARoomWhereNothingHasReportedIsAnError(t *testing.T) {
	for name, cond := range map[string]Condition{
		"nothing reported": func(s State) bool { return s.Any(InRoom("cellar", Occupied)) },
		"no such room":     func(s State) bool { return s.Any(InRoom("loft", Occupied)) },
		"no room":          func(s State) bool { return s.Any(InRoom("", Occupied)) },
		"no such type":     func(s State) bool { return s.Any(InRoom("hallway", On).Types(DimmableLight)) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cond.toEngine()(hallway()); err == nil {
				t.Fatal("got no error")
			}
		})
	}
}

func TestDiscoveredAddsADeviceNewSinceTheWelcome(t *testing.T) {
	mirror := hub.NewStore(logging.Discard())
	metas := map[ident.ID]hub.Meta{}
	discovered(mirror, metas, nil, wire.Device{
		Name: "lamp", Room: "hall", Arbitrated: []string{"on"}, Discovered: true,
		Capabilities: []string{"on"}, Values: map[string]any{"on": true},
	})
	snap, ok := mirror.Get(deviceKey("lamp"))
	if !ok || !snap.Discovered || snap.Room != "hall" || !snap.Controls("on") || snap.Values["on"] != true {
		t.Fatalf("got %+v, %v", snap, ok)
	}
	changed(mirror, wire.Message{Device: "lamp", Capability: "on", Value: false})
	if snap, _ := mirror.Get(deviceKey("lamp")); snap.Values["on"] != false {
		t.Fatalf("a later change didn't reach it: %+v", snap)
	}
}

func TestManifestDescribesTheHome(t *testing.T) {
	when := func(State) bool { return true }
	m := Home{
		Holds: []Hold{{Name: "leak", Priority: Safety, Latch: true, When: when}, {Name: "fan", When: when}},
		Moments: []Moment{
			{Name: "alarm", Do: []Action{Set{}.Priority(Safety)}},
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
	if !reflect.DeepEqual(m.Rules, want) {
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

var (
	pump  = Handle[bool]{Device: "pump", Capability: On}
	water = Handle[float64]{Device: "water", Capability: Temperature}
)

func valveControl() Control {
	return Control{
		Default: pump.To(false), Resolve: PreferOn,
		External: ExternalOverride, OverrideHold: time.Hour, MinimumOn: time.Hour, MinimumOff: 30 * time.Minute,
	}
}

func budgetHome(c Control, b Budget) Home {
	return Home{
		Control: []Control{c},
		Holds:   []Hold{{Name: "pump-daily", Set: pump.To(true), Budget: &b}},
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
			Reading: water, Ranges: []Band{{Below: 12, Minutes: 60}}, Otherwise: 90,
		}}), ""},
		"no minutes":   {budgetHome(valveControl(), Budget{}), "exactly one of bands or minutes"},
		"both minutes": {budgetHome(valveControl(), Budget{Minutes: minutes, Bands: &Bands{}}), "exactly one"},
		"bad slot":     {budgetHome(valveControl(), Budget{Minutes: minutes, SlotMinutes: 20}), "slotMinutes"},
		"bad window": {
			budgetHome(valveControl(), Budget{Minutes: minutes, Forbid: []Window{{Start: Clock("7"), End: Clock("8")}}}),
			"not HH:MM",
		},
		"limit off the slots": {budgetHome(limited, Budget{Minutes: minutes}), "isn't a multiple"},
		"no control": {Home{Holds: []Hold{{
			Name: "pump-daily", Set: pump.To(true), Budget: &Budget{Minutes: minutes},
		}}}, "needs a Control"},
		"twice": {Home{Control: []Control{valveControl(), valveControl()}}, "declared twice"},
		"a room": {control(Control{
			Default: InRoom("hall", On).To(false), Resolve: PreferOn, External: ExternalRevert,
		}), "names a device, not a room"},
		"no default": {control(Control{Resolve: PreferOn, External: ExternalRevert}), "needs a capability"},
		"no device": {control(Control{
			Default: Handle[bool]{Capability: On}.To(false), Resolve: PreferOn, External: ExternalRevert,
		}), "needs a device"},
		"a claim": {control(Control{
			Default: pump.To(false).UntilNext(), Resolve: PreferOn, External: ExternalRevert,
		}), "takes no For, Until or Priority"},
		"bool resolve": {control(Control{
			Default: pump.To(false), Resolve: Highest, External: ExternalRevert,
		}), "PreferOn or PreferOff"},
		"read-only": {control(Control{
			Default: Handle[float64]{Device: "x", Capability: Temperature}.To(1), Resolve: Highest, External: ExternalRevert,
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
	vars := map[string]any{"devices": map[string]map[string]any{"water": {}}}
	nothing := Handle[float64]{Device: "nothing", Capability: Temperature}
	for name, f := range map[string]func(State) int{
		"hasn't reported": func(s State) int { return int(s.Get(water)) },
		"no such device":  func(s State) int { return int(s.Get(nothing)) },
		"a bug":           func(State) int { panic("oops") },
	} {
		t.Run(name, func(t *testing.T) {
			spec := (&Budget{Minutes: f}).toSchedule()
			if _, err := spec.Minutes(vars, time.Now(), time.UTC); err == nil {
				t.Fatal("no error")
			}
		})
	}
	spec := (&Budget{Minutes: func(State) int { return 90 }}).toSchedule()
	if got, err := spec.Minutes(vars, time.Now(), time.UTC); err != nil || got != 90 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestMetaHasTheLimitsOfTheHomesControl(t *testing.T) {
	h := Home{Control: []Control{valveControl()}}
	settings, err := h.settings()
	if err != nil {
		t.Fatal(err)
	}
	m := meta(wire.Device{Name: "pump", Arbitrated: []string{"on", "brightness"}}, settings)
	if len(m.Control) != 2 || time.Duration(m.Control[0].MinimumOn) != time.Hour ||
		m.Control[1].Capability != "brightness" {
		t.Fatalf("got %+v", m.Control)
	}
}

// A Safety rule's room writes limited to device types are in the manifest, which the host checks a
// device's new type against. Other rules' aren't.
func TestASafetyRulesTypedRoomsAreInTheManifest(t *testing.T) {
	lights := InRoom("hall", On).Types(OnOffLight, DimmableLight)
	m := Home{
		Holds: []Hold{
			{
				Name: "night-lights", Priority: Safety, Set: lights.To(true),
				Window: &Window{Start: Clock("22:00"), End: Clock("06:00")},
			},
			{Name: "evening", Set: lights.To(true), Window: &Window{Start: Clock("18:00"), End: Clock("22:00")}},
		},
		Moments: []Moment{{Name: "alarm", At: []Time{Clock("03:00")}, Do: []Action{
			lights.To(true).For(time.Hour).Priority(Safety), InRoom("hall", On).To(true).For(time.Hour).Priority(Safety),
		}}},
	}.manifest()
	want := []wire.RoomOfTypes{{
		Room: "hall", Capability: "on", DeviceTypes: []devicetype.Type{devicetype.OnOffLight, devicetype.DimmableLight},
	}}
	for _, r := range m.Rules {
		got := r.Rooms
		if r.Name == "evening" {
			if got != nil {
				t.Errorf("evening isn't Safety, and has rooms %+v", got)
			}
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s has rooms %+v, want %+v", r.Name, got, want)
		}
	}
}

func TestAWindowReachesTheEngine(t *testing.T) {
	h := Hold{Name: "a", Set: Handle[bool]{Device: "d", Capability: On}.To(true), Window: &Window{
		Start: Clock("22:00"), End: Clock("06:00"), Days: []time.Weekday{time.Friday},
	}}
	want := &engine.Window{
		Start: engine.At{Time: "22:00"}, End: engine.At{Time: "06:00"}, Days: []time.Weekday{time.Friday},
	}
	if got := h.toEngine().Window; !reflect.DeepEqual(got, want) {
		t.Errorf("window %+v, want %+v", got, want)
	}
}
