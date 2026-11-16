package oikade

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/sun"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

func TestCompileRejectsAnUnsoundHome(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	d := Bool{Device: "d", Capability: On}
	for name, tc := range map[string]struct {
		home Home
		want string
	}{
		"no name": {NewHome(Options{}, NewHold("", d.To(true)).When(when)), "needs a name"},
		"twice": {NewHome(Options{},
			NewHold("a", d.To(true)).When(when),
			NewMoment("a", Wait(time.Second)).At(Clock("07:00")),
		), "declared twice"},
		"unknown capability": {
			NewHome(Options{}, NewHold("a", Bool{Device: "d", Capability: "nope"}.To(true)).When(when)),
			"unknown capability",
		},
		"no trigger": {NewHome(Options{}, NewMoment("a", Wait(time.Second))), "trigger"},
		"nil action": {NewHome(Options{}, NewMoment("a", nil).At(Clock("07:00"))), "no action"},
		"no set":     {NewHome(Options{}, NewHold("a", Set{}).When(when)), "needs a capability"},
		"claim": {
			NewHome(Options{}, NewHold("a", d.To(true).For(time.Hour)).When(when)), "takes no For, Until or Priority",
		},
		"nil change": {
			NewHome(Options{}, NewMoment("a", Wait(time.Second)).On(nil)), "a nil change, at On[0]",
		},
		"a time that isn't one": {
			NewHome(Options{}, NewMoment("bedtime", Wait(time.Second)).At(Clock("25:00"))),
			`rule bedtime: timers[0]: time "25:00" is not HH:MM`,
		},
		"a window with no end": {
			NewHome(Options{}, NewHold("evening", d.To(true)).When(Daily(Clock("18:00"), Time{}))), "end: no time",
		},
		"a window that isn't one": {
			NewHome(Options{}, NewHold("evening", d.To(true)).When(All(d, Daily("18:00", "7")))), `time "7" is not HH:MM`,
		},
		"a date that isn't one": {
			NewHome(Options{}, NewHold("summer", d.To(true)).When(Yearly("1 Jun", "31 Jun"))), `"31 Jun" isn't a date`,
		},
		"a Within the wrong way round": {
			NewHome(Options{}, NewHold("comfy", d.To(true)).When(Number{"t", Temperature}.Within(25, 18))),
			"give the lower first",
		},
		"a nil condition in All": {
			NewHome(Options{}, NewHold("a", d.To(true)).When(All(d, nil))), "All has a nil condition, at 1",
		},
		"a hold with no When": {NewHome(Options{}, NewHold("a", d.To(true))), "a hold needs a When or a Budget"},
		"a ConditionFunc with no label": {
			NewHome(Options{}, NewHold("a", d.To(true)).When(ConditionFunc("", func(Snapshot) bool { return true }))),
			"a ConditionFunc needs a label",
		},
		"a moment's condition": {
			NewHome(Options{}, NewMoment("a", d.To(true)).On(d).If(Yearly("1 Jun", "June 30"))), `"June 30" isn't a date`,
		},
		"a sunset with no site": {
			NewHome(Options{}, NewMoment("porch-at-dusk", Wait(time.Second)).At(Sunset(0))),
			"rule porch-at-dusk: a sunrise or sunset needs the home's location",
		},
		"a sunrise window with no site": {
			NewHome(Options{}, NewHold("a", d.To(true)).When(Daily(Sunrise(0), Clock("09:00")))),
			"rule a: a sunrise or sunset needs the home's location",
		},
		"a latitude off the earth": {
			NewHome(Options{Location: NewLocation(91, 0)}, NewHold("a", d.To(true)).When(when)), "latitude 91 isn't",
		},
		"a longitude off the earth": {
			NewHome(Options{Location: NewLocation(0, -181)}, NewHold("a", d.To(true)).When(when)),
			"longitude -181 isn't",
		},
		"a bridge name too long": {
			NewHome(Options{MatterName: "The cottage at the end of the lane"}, NewHold("a", d.To(true)).When(when)),
			"is 34 bytes, and it's at most 32",
		},
		"a bridge name that isn't UTF-8": {
			NewHome(Options{MatterName: "Caf\xe9"}, NewHold("a", d.To(true)).When(when)), "isn't valid UTF-8",
		},
		"an offset too far": {
			NewHome(Options{Location: NewLocation(51.48, 0)}, NewFlow("a", func(*FlowRun) error { return nil }).
				At(Sunrise(7*time.Hour))),
			"offset 7h0m0s is not between -6h and 6h",
		},
		"both untils": {
			NewHome(Options{}, NewMoment("a", d.To(true).UntilReleased().UntilNext()).At(Clock("07:00"))),
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
	door := Bool{Device: "door", Capability: Open}
	room := Number{Device: "room", Capability: Temperature}
	cond := toEngine(ConditionFunc("door and room", func(s Snapshot) bool {
		return s.Get(door) && s.Get(room) > 20 && s.Now().Equal(now)
	}))
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
	door := Bool{Device: "door", Capability: Open}
	cond := toEngine(Not(door))
	devices := map[string]map[string]any{"door": {}}
	if _, err := cond(engine.Vars{"devices": devices}); err == nil {
		t.Fatal("got no error")
	}
}

// A device that doesn't exist will never report, so it mustn't pass for one that hasn't yet. The
// engine recovers this panic and logs it.
func TestConditionReadingAnUndeclaredDevicePanics(t *testing.T) {
	cond := toEngine(Bool{Device: "dor", Capability: Open})
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "no device dor") {
			t.Fatalf("got %v", r)
		}
	}()
	_, _ = cond(engine.Vars{"devices": map[string]map[string]any{"door": {}}})
}

func TestLookupCarriesOnWithoutAValue(t *testing.T) {
	cellar := Number{Device: "cellar", Capability: Humidity}
	attic := Number{Device: "attic", Capability: Humidity}
	cond := toEngine(ConditionFunc("attic damp", func(s Snapshot) bool {
		if _, ok := s.Lookup(cellar); ok {
			t.Error("Lookup found a value nobody reported")
		}
		v, ok := s.Lookup(attic)
		return ok && v > 60
	}))
	ok, err := cond(engine.Vars{"devices": map[string]map[string]any{"cellar": {}, "attic": {"humidity": 70.0}}})
	if err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}

func TestLookupOfAnUndeclaredDevicePanics(t *testing.T) {
	cond := toEngine(ConditionFunc("dor", func(s Snapshot) bool {
		_, ok := s.Lookup(Bool{Device: "dor", Capability: Open})
		return ok
	}))
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
		cond func(Snapshot) bool
		want bool
	}{
		"any":         {func(s Snapshot) bool { return s.Any(InRoom("hallway", Everything, Occupied)) }, true},
		"all":         {func(s Snapshot) bool { return s.All(InRoom("hallway", Everything, Occupied)) }, false},
		"count":       {func(s Snapshot) bool { return s.Count(InRoom("hallway", Everything, Occupied)) == 1 }, true},
		"all on":      {func(s Snapshot) bool { return s.All(InRoom("hallway", Everything, On)) }, false},
		"lights only": {func(s Snapshot) bool { return s.All(InRoom("hallway", Lights, On)) }, true},
		"plugs only":  {func(s Snapshot) bool { return s.Any(InRoom("hallway", Kinds(OnOffPlugInUnit), On)) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := toEngine(ConditionFunc(name, tc.cond))(hallway())
			if err != nil || ok != tc.want {
				t.Fatalf("got %v, %v; want %v", ok, err, tc.want)
			}
		})
	}
}

func TestReadingsAreInDeviceNameOrder(t *testing.T) {
	var got []float64
	cond := toEngine(ConditionFunc("cellar damp", func(s Snapshot) bool {
		got = s.Readings(InRoom("cellar", Everything, Humidity))
		return slices.Max(got) > 65
	}))
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
	for name, cond := range map[string]func(Snapshot) bool{
		"nothing reported": func(s Snapshot) bool { return s.Any(InRoom("cellar", Everything, Occupied)) },
		"no such room":     func(s Snapshot) bool { return s.Any(InRoom("loft", Everything, Occupied)) },
		"no room":          func(s Snapshot) bool { return s.Any(InRoom("", Everything, Occupied)) },
		"no such type":     func(s Snapshot) bool { return s.Any(InRoom("hallway", Kinds(DimmableLight), On)) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := toEngine(ConditionFunc(name, cond))(hallway()); err == nil {
				t.Fatal("got no error")
			}
		})
	}
}

func is(t *testing.T, c Condition, at time.Time) bool {
	t.Helper()
	ok, err := toEngine(c)(engine.Vars{"now": at, "location": at.Location()})
	if err != nil {
		t.Fatalf("got %v, %v", ok, err)
	}
	return ok
}

func TestDailyIsOpenFromItsStartUntilTheNextEnd(t *testing.T) {
	evening := Daily("18:00", "07:00")
	// Friday 6 November 2026, in Paris.
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, paris) }
	for _, tc := range []struct {
		c    Hours
		at   time.Time
		want bool
	}{
		{evening, at(6, 23, 0), true},
		{evening, at(7, 6, 59), true},
		{evening, at(7, 7, 0), false},
		{evening, at(7, 12, 0), false},
		{evening, at(6, 18, 0), true},
		// Starting on Fridays, it's still open early on Saturday, and not early on Friday.
		{Daily("20:00", "04:00").Days(time.Friday), at(7, 2, 0), true},
		{Daily("20:00", "04:00").Days(time.Friday), at(6, 2, 0), false},
	} {
		if got := is(t, tc.c, tc.at); got != tc.want {
			t.Errorf("Daily(%v) at %s = %v, want %v", tc.c, tc.at, got, tc.want)
		}
	}
}

// The clocks go forward at 02:00 on Sunday 29 March 2026 in London, and back at 02:00 on Sunday 25
// October. A Daily is in wall-clock time either side.
func TestDailyKeepsToTheWallClockAcrossAClockChange(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	night := Daily("01:00", "03:00")
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 3, 29, 0, 59, 0, 0, london), false},
		{time.Date(2026, 3, 29, 1, 30, 0, 0, london), true},
		// 02:00 GMT is 03:00 BST, so the window has just closed: it was an hour long.
		{time.Date(2026, 3, 29, 2, 0, 0, 0, time.UTC), false},
		{time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), true}, // 01:30 BST
		{time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), true}, // 01:30 GMT, the second time round
		{time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC), false}, // 03:00 GMT
	} {
		if got := is(t, night, tc.at.In(london)); got != tc.want {
			t.Errorf("Daily(%v) at %s = %v, want %v", night, tc.at.In(london), got, tc.want)
		}
	}
}

func TestYearlyIsWholeDaysAndCrossesTheYearsEnd(t *testing.T) {
	summer := Yearly("1 Jun", "15 September")
	winter := Yearly("1 Dec", "28 Feb")
	day := func(month time.Month, d, hour int) time.Time {
		return time.Date(2026, month, d, hour, 0, 0, 0, time.UTC)
	}
	for _, tc := range []struct {
		c    Dates
		at   time.Time
		want bool
	}{
		{summer, day(5, 31, 23), false},
		{summer, day(6, 1, 0), true},
		{summer, day(9, 15, 23), true},
		{summer, day(9, 16, 0), false},
		{winter, day(12, 31, 23), true},
		{winter, day(1, 1, 0), true},
		{winter, day(2, 28, 12), true},
		{winter, day(3, 1, 0), false},
		{winter, day(11, 30, 23), false},
		{Yearly("29 Feb", "29 Feb"), time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC), true},
	} {
		if got := is(t, tc.c, tc.at); got != tc.want {
			t.Errorf("Yearly(%s, %s) on %s = %v, want %v", tc.c.start, tc.c.end, tc.at, got, tc.want)
		}
	}
}

// Yearly's day is the host's, so New Year's Eve in Sydney is already 1 January there.
func TestYearlyIsInTheHostsTimeZone(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatal(err)
	}
	newYear := Yearly("1 Jan", "1 Jan")
	if !is(t, newYear, time.Date(2026, 12, 31, 14, 0, 0, 0, time.UTC).In(sydney)) {
		t.Error("it's 1 January in Sydney at 14:00 UTC on 31 December")
	}
}

func TestConditionsCombine(t *testing.T) {
	water := Number{Device: "pool", Capability: Temperature}
	pump := Bool{Device: "pump", Capability: On}
	vars := engine.Vars{
		"now": time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC), "location": time.UTC,
		"devices": map[string]map[string]any{"pool": {"temperature": 24.5}, "pump": {"on": true}, "unheard": {}},
	}
	unheard := Bool{Device: "unheard", Capability: Open}
	summer, swim := Yearly("1 Jun", "15 Sep"), Daily("10:00", "18:00")
	for name, tc := range map[string]struct {
		c    Condition
		want bool
	}{
		"a bool":           {pump, true},
		"not":              {Not(pump), false},
		"above":            {water.Above(24), true},
		"above at":         {water.Above(24.5), false},
		"below":            {water.Below(3), false},
		"within":           {water.Within(24.5, 26), true},
		"within, outside":  {water.Within(10, 20), false},
		"all":              {All(summer, swim, pump), true},
		"all but one":      {All(summer, swim, Not(pump)), false},
		"any":              {Any(water.Below(3), swim), true},
		"any, none":        {Any(water.Below(3), Not(pump)), false},
		"not of any":       {Not(Any(water.Below(3), Not(pump))), true},
		"a func":           {ConditionFunc("warm", func(s Snapshot) bool { return s.Get(water) > 20 }), true},
		"a func that's Is": {ConditionFunc("is", func(s Snapshot) bool { return s.Is(All(summer, pump)) }), true},
		"any past unheard": {Any(unheard, pump), true},
		"all past unheard": {All(unheard, Not(pump)), false},
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := toEngine(tc.c)(vars)
			if err != nil || ok != tc.want {
				t.Fatalf("got %v, %v; want %v", ok, err, tc.want)
			}
		})
	}
}

// Any and All stop at a value nobody has reported only when the others can't decide without it.
func TestAnyAndAllWaitForAnUnreportedValueOnlyIfItDecides(t *testing.T) {
	vars := engine.Vars{"devices": map[string]map[string]any{"pump": {"on": false}, "unheard": {}}}
	pump, unheard := Bool{Device: "pump", Capability: On}, Bool{Device: "unheard", Capability: Open}
	for name, c := range map[string]Condition{"any": Any(pump, unheard), "all": All(Not(pump), unheard)} {
		if _, err := toEngine(c)(vars); err == nil || !strings.Contains(err.Error(), "unheard open hasn't reported") {
			t.Errorf("%s: got %v, want it to wait for unheard", name, err)
		}
	}
}

func TestAConditionHasALabelForTheJournal(t *testing.T) {
	water := Number{Device: "pool", Capability: Temperature}
	c := All(Not(Bool{Device: "door", Capability: Open}), water.Within(1, 2.5), Any(Daily("22:00", "06:00"),
		Yearly("1 Jun", "15 Sep")), ConditionFunc("damp", func(Snapshot) bool { return true }))
	want := "all(not(door open), pool temperature within 1 to 2.5, " +
		"any(daily 22:00 to 06:00, yearly 1 Jun to 15 Sep), damp)"
	if got := label(c); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAConditionSaysWhyItsFalse(t *testing.T) {
	water := Number{Device: "pool-water", Capability: Temperature}
	door := Bool{Device: "garage-door", Capability: Open}
	vars := engine.Vars{
		"now": time.Date(2026, 11, 16, 9, 12, 0, 0, time.UTC), "location": time.UTC,
		"devices": map[string]map[string]any{
			"pool-water": {"temperature": 5.0}, "garage-door": {"open": false}, "unheard": {},
		},
	}
	for name, tc := range map[string]struct {
		c    Condition
		want string
	}{
		"true":      {water.Above(3), ""},
		"a bool":    {door, "garage-door open is false"},
		"not":       {Not(Not(door)), "garage-door open is false"},
		"below":     {water.Below(3), "pool-water temperature is 5, not below 3"},
		"not above": {Not(water.Above(3)), "pool-water temperature is 5, above 3"},
		"within":    {water.Within(18, 25.5), "pool-water temperature is 5, not within 18 to 25.5"},
		"daily":     {Daily("10:00", "18:00"), "it's 09:12, outside 10:00 to 18:00"},
		"days":      {Daily("10:00", "18:00").Days(time.Saturday), "it's Mon 09:12, outside 10:00 to 18:00 starting Sat"},
		"not daily": {Not(Daily("09:00", "10:00")), "it's 09:12, inside 09:00 to 10:00"},
		"sunset": {
			Daily(Sunset(-20*time.Minute), Sunrise(time.Hour)), "it's 09:12, outside sunset-20m0s to sunrise+1h0m0s",
		},
		"yearly": {Yearly("1 Jun", "15 Sep"), "it's 16 Nov, outside 1 Jun to 15 Sep"},
		"all":    {All(water.Above(3), door, water.Below(3)), "garage-door open is false"},
		"any":    {Any(door, water.Below(3)), "garage-door open is false, and pool-water temperature is 5, not below 3"},
		"not all": {
			Not(All(water.Above(3), Not(door))), "pool-water temperature is 5, above 3, and garage-door open is false",
		},
		"a func":     {ConditionFunc("cellar damp", func(Snapshot) bool { return false }), "cellar damp is false"},
		"unreported": {Bool{Device: "unheard", Capability: Open}, "unheard open hasn't reported"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := explain(tc.c, vars); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A hold's Daily, on its own or in an All, is the engine's window, which changes on its edge.
func TestAHoldsDailyIsItsWindow(t *testing.T) {
	d := Bool{Device: "d", Capability: On}
	night := Daily("22:00", "06:00")
	for name, tc := range map[string]struct {
		c      Condition
		window bool
		when   Condition
	}{
		"alone":       {night, true, nil},
		"in all":      {All(d, night), true, d},
		"in all of 3": {All(d, night, Not(d)), true, allOf{d, Not(d)}},
		"in any":      {Any(d, night), false, Any(d, night)},
		"none":        {d, false, d},
	} {
		t.Run(name, func(t *testing.T) {
			hours, when := splitHours(tc.c)
			if (hours != nil) != tc.window || !reflect.DeepEqual(when, tc.when) {
				t.Fatalf("got %v, %#v", hours, when)
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
	changed(mirror, wire.Message{Device: "lamp", Changes: []wire.Change{{Capability: "on", Value: false}}})
	if snap, _ := mirror.Get(deviceKey("lamp")); snap.Values["on"] != false {
		t.Fatalf("a later change didn't reach it: %+v", snap)
	}
}

type reports struct {
	hub.NopSink
	got [][]driver.Update
}

func (r *reports) Changed(changes ...driver.Update) { r.got = append(r.got, changes) }

// A change message is one report, so the engine takes in all of it at once (OIK-212).
func TestAChangeReachesTheEngineAsOneReport(t *testing.T) {
	r := &reports{}
	mirror := hub.NewStore(logging.Discard(), r)
	discovered(mirror, map[ident.ID]hub.Meta{}, nil, wire.Device{
		Name: "hall", Discovered: true, Capabilities: []string{"humidity", "occupied"},
		Values: map[string]any{"humidity": 40.0, "occupied": false},
	})
	r.got = nil
	changed(mirror, wire.Message{Device: "hall", Changes: []wire.Change{
		{Capability: "humidity", Value: 72.0, Previous: 40.0},
		{Capability: "occupied", Value: true, Previous: false},
	}})
	if len(r.got) != 1 || len(r.got[0]) != 2 {
		t.Fatalf("reports %+v, want one with both values", r.got)
	}
}

func TestManifestDescribesTheHome(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	m := NewHome(Options{},
		NewHold("leak", Set{}).Priority(Safety).Latch().When(when),
		NewHold("fan", Set{}).When(when),
		NewMoment("alarm", Set{}.Priority(Safety)),
		NewMoment("lamp", Set{}, Wait(time.Second)),
		NewFlow("shutoff", nil).Priority(Safety),
		NewFlow("bedtime", nil),
	).manifest()
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
	for name, parts := range map[string][]Part{
		"no name":  {NewPlugin("", made)},
		"bad name": {NewPlugin("a/b", made)},
		"twice":    {NewPlugin("a", made), NewPlugin("a", made)},
		"no New":   {NewPlugin("a", nil)},
	} {
		if _, _, err := NewHome(Options{}, parts...).compile(); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	if m := NewHome(Options{}, NewPlugin("a", made)).manifest(); !slices.Equal(m.Plugins, []string{"a"}) {
		t.Errorf("got %v", m.Plugins)
	}
}

var (
	pump  = Bool{Device: "pump", Capability: On}
	water = Number{Device: "water", Capability: Temperature}
)

func valveControl() Control {
	return NewControl(pump.To(false), PreferOn, ExternalOverride).
		OverrideHold(time.Hour).
		MinimumOn(time.Hour).
		MinimumOff(30 * time.Minute)
}

func budgetHome(c Control, b Budget) Home {
	return NewHome(Options{},
		c,
		NewHold("pump-daily", pump.To(true)).Budget(b),
	)
}

func TestHomeChecksControlAndBudgets(t *testing.T) {
	minutes := func(Snapshot) int { return 120 }
	limited := valveControl().MinimumOn(45 * time.Minute)
	control := func(c Control) Home { return NewHome(Options{}, c) }
	arbitrate := func(s Set, r Resolve) Home { return control(NewControl(s, r, ExternalRevert)) }
	for name, tc := range map[string]struct {
		home Home
		want string
	}{
		"fine": {budgetHome(valveControl(), NewBudget(minutes)), ""},
		"bands": {budgetHome(valveControl(), NewBandedBudget(Bands{
			Reading: water, Ranges: []Band{{Below: 12, Minutes: 60}}, Otherwise: 90,
		})), ""},
		"no minutes": {budgetHome(valveControl(), Budget{}), "exactly one of bands or minutes"},
		"bad slot":   {budgetHome(valveControl(), NewBudget(minutes).SlotMinutes(20)), "slotMinutes"},
		"bad window": {
			budgetHome(valveControl(), NewBudget(minutes).Forbid(Daily("7", "8"))),
			"not HH:MM",
		},
		"limit off the slots": {budgetHome(limited, NewBudget(minutes)), "isn't a multiple"},
		"no control": {NewHome(Options{}, NewHold("pump-daily", pump.To(true)).
			Budget(NewBudget(minutes))), "needs a Control"},
		"twice":        {NewHome(Options{}, valveControl(), valveControl()), "declared twice"},
		"a room":       {arbitrate(InRoom("hall", Everything, On).To(false), PreferOn), "names a device, not a room"},
		"no default":   {arbitrate(Set{}, PreferOn), "needs a capability"},
		"no device":    {arbitrate(Bool{Capability: On}.To(false), PreferOn), "needs a device"},
		"a claim":      {arbitrate(pump.To(false).UntilNext(), PreferOn), "takes no For, Until or Priority"},
		"bool resolve": {arbitrate(pump.To(false), Highest), "PreferOn or PreferOff"},
		"read-only":    {arbitrate(Number{Device: "x", Capability: Temperature}.To(1), Highest), "read-only"},
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
	nothing := Number{Device: "nothing", Capability: Temperature}
	for name, f := range map[string]func(Snapshot) int{
		"hasn't reported": func(s Snapshot) int { return int(s.Get(water)) },
		"no such device":  func(s Snapshot) int { return int(s.Get(nothing)) },
		"a bug":           func(Snapshot) int { panic("oops") },
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBudget(f)
			spec := b.toSchedule()
			if _, err := spec.Minutes(vars, time.Now(), time.UTC); err == nil {
				t.Fatal("no error")
			}
		})
	}
	ninety := NewBudget(func(Snapshot) int { return 90 })
	spec := ninety.toSchedule()
	if got, err := spec.Minutes(vars, time.Now(), time.UTC); err != nil || got != 90 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestMetaHasTheLimitsOfTheHomesControl(t *testing.T) {
	h := NewHome(Options{}, valveControl())
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
	lights := InRoom("hall", Lights, On)
	m := NewHome(Options{},
		NewHold("night-lights", lights.To(true)).
			Priority(Safety).
			When(Daily("22:00", "06:00")),
		NewHold("evening", lights.To(true)).When(Daily("18:00", "22:00")),
		NewMoment("alarm", lights.To(true).For(time.Hour).
			Priority(Safety), InRoom("hall", Everything, On).To(true).For(time.Hour).
			Priority(Safety)).
			At(Clock("03:00")),
	).manifest()
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

func TestABudgetTakesClockTimesOnly(t *testing.T) {
	b := NewBudget(func(Snapshot) int { return 60 }).Prefer(Daily(Sunset(0), Clock("23:00")), 3)
	home := NewHome(Options{Location: NewLocation(51.48, -0.01)},
		valveControl(), NewHold("pump-daily", pump.To(true)).Budget(b))
	if _, _, err := home.compile(); err == nil || !strings.Contains(err.Error(), "clock times only") {
		t.Fatalf("got %v, want a budget's windows to take clock times only", err)
	}
}

func TestASunTimeReachesTheEngine(t *testing.T) {
	m := NewMoment("a", Wait(time.Second)).At(Sunset(-20*time.Minute), Sunrise(0), Clock("18:00"))
	want := []engine.Timer{
		engine.SunTime{Event: sun.Sunset, Offset: -20 * time.Minute}, engine.SunTime{Event: sun.Sunrise},
		engine.At{Time: "18:00"},
	}
	if got := m.toEngine().Timers; !reflect.DeepEqual(got, want) {
		t.Errorf("timers %+v, want %+v", got, want)
	}
}

func TestAWindowReachesTheEngine(t *testing.T) {
	h := NewHold("a", Bool{Device: "d", Capability: On}.To(true)).
		When(Daily("22:00", "06:00").Days(time.Friday))
	want := &engine.Window{
		Start: engine.At{Time: "22:00"}, End: engine.At{Time: "06:00"}, Days: []time.Weekday{time.Friday},
	}
	if got := h.toEngine().Window; !reflect.DeepEqual(got, want) {
		t.Errorf("window %+v, want %+v", got, want)
	}
}

func TestNewHomeTakesPartsFromNestedBundles(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	made := func(plugin.Environment, []contractv1.DeclaredDevice) (plugin.Handler, error) { return nil, nil }
	dehumidifier := func() Bundle {
		return NewBundle(valveControl(), NewHold("inner", pump.To(true)).When(when))
	}
	h := NewHome(Options{},
		NewHold("first", pump.To(true)).When(when),
		NewBundle(),
		NewBundle(
			dehumidifier(),
			NewMoment("moment", Wait(time.Second)).At(Clock("07:00")),
			NewFlow("flow", func(*FlowRun) error { return nil }).At(Clock("08:00")),
			NewPlugin("relays", made),
		),
		NewHold("last", pump.To(true)).When(when),
	)
	if _, _, err := h.compile(); err != nil {
		t.Fatal(err)
	}
	m := h.manifest()
	names := make([]string, 0, len(m.Rules))
	for _, r := range m.Rules {
		names = append(names, r.Name)
	}
	// A rule's name is its state key, so a Bundle leaves it exactly as written.
	if want := []string{"first", "inner", "last", "moment", "flow"}; !slices.Equal(names, want) {
		t.Errorf("got rules %v, want %v", names, want)
	}
	if !slices.Equal(m.Plugins, []string{"relays"}) {
		t.Errorf("got plugins %v", m.Plugins)
	}
	if len(m.Control) != 1 {
		t.Errorf("got control %+v, want the Control from the Bundle", m.Control)
	}
}

func TestNewBundleKeepsItsOwnParts(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	parts := []Part{NewHold("kept", pump.To(true)).When(when)}
	b := NewBundle(parts...)
	parts[0] = NewHold("changed", pump.To(true)).When(when)
	m := NewHome(Options{}, b).manifest()
	if len(m.Rules) != 1 || m.Rules[0].Name != "kept" {
		t.Errorf("got %+v", m.Rules)
	}
}

func TestNewHomeRefusesWhatIsntAPart(t *testing.T) {
	type wrapped struct{ Hold }
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	hold := NewHold("a", pump.To(true)).When(when)
	var noHold *Hold
	for name, tc := range map[string]struct {
		parts []Part
		want  string
	}{
		"nil":                  {[]Part{hold, nil}, "a nil part, at parts[1]"},
		"nil in a bundle":      {[]Part{hold, NewBundle(hold, NewBundle(nil))}, "a nil part, at parts[1][1][0]"},
		"a pointer":            {[]Part{&hold}, "a *oikade.Hold, at parts[0]"},
		"a nil pointer":        {[]Part{noHold}, "a *oikade.Hold, at parts[0]"},
		"a pointer to bundle":  {[]Part{&Bundle{}}, "a *oikade.Bundle, at parts[0]"},
		"a type embedding one": {[]Part{wrapped{hold}}, "a oikade.wrapped, at parts[0]"},
	} {
		_, _, err := NewHome(Options{}, tc.parts...).compile()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

func TestARuleNameClashesAcrossBundles(t *testing.T) {
	motion := func(name string) Bundle {
		return NewBundle(NewHold(name, pump.To(true)).When(ConditionFunc("always", func(Snapshot) bool { return true })))
	}
	_, _, err := NewHome(Options{}, motion("landing"), motion("landing")).compile()
	if err == nil || !strings.Contains(err.Error(), `rule "landing" is declared twice`) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := NewHome(Options{}, motion("landing"), motion("porch")).compile(); err != nil {
		t.Fatal(err)
	}
}

func TestALocationComparesByValue(t *testing.T) {
	if NewLocation(51.48, -0.01) != NewLocation(51.48, -0.01) {
		t.Error("two locations at one place differ")
	}
	if NewLocation(0, 0) == (Location{}) {
		t.Error("a place at 0, 0 is the same as no location")
	}
}

func TestAZeroCoordinateIsARealPlace(t *testing.T) {
	dusk := NewMoment("a", Wait(time.Second)).At(Sunset(0))
	for _, o := range []Options{
		{Location: NewLocation(51.48, 0)}, {Location: NewLocation(0, -0.01)}, {Location: NewLocation(0, 0)},
	} {
		if _, _, err := NewHome(o, dusk).compile(); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if _, _, err := NewHome(Options{}, dusk).compile(); err == nil {
		t.Error("no location: compiled")
	}
}

func TestAnEmptyHomeIsValid(t *testing.T) {
	for name, h := range map[string]Home{"zero": {}, "made": NewHome(Options{})} {
		if _, _, err := h.compile(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestASimulatedHomeReportsAMistakeInItsParts(t *testing.T) {
	_, err := NewHome(Options{}, nil).simulated()
	if err == nil || !strings.Contains(err.Error(), "a nil part, at parts[0]") {
		t.Fatalf("got %v", err)
	}
}

func TestAnOptionGivenTwiceFailsNamingTheRuleAndTheOption(t *testing.T) {
	always := ConditionFunc("always", func(Snapshot) bool { return true })
	run := func(*FlowRun) error { return nil }
	hold := NewHold("h", pump.To(true))
	moment := NewMoment("m", pump.To(true)).On(pump)
	flow := NewFlow("f", run).On(pump)
	budget := func() Budget { return NewBudget(func(Snapshot) int { return 60 }) }
	for name, tc := range map[string]struct {
		part Part
		want []string
	}{
		"Hold.When":     {hold.When(always).When(always), []string{"rule h", "When is given twice"}},
		"Hold.OnDelay":  {hold.When(always).OnDelay(0).OnDelay(time.Second), []string{"rule h", "OnDelay is given twice"}},
		"Hold.OffDelay": {hold.When(always).OffDelay(time.Second).OffDelay(0), []string{"rule h", "OffDelay is given twice"}},
		"Hold.Priority": {hold.When(always).Priority(Safety).Priority(Safety), []string{"rule h", "Priority is given twice"}},
		"Hold.Latch":    {hold.When(always).Latch().Latch(), []string{"rule h", "Latch is given twice"}},
		"Hold.Budget":   {hold.Budget(budget()).Budget(budget()), []string{"rule h", "Budget is given twice"}},
		"Budget.PlanningTime": {
			hold.Budget(budget().PlanningTime("01:00").PlanningTime("02:00")),
			[]string{"rule h", "a budget's PlanningTime is given twice"},
		},
		"Budget.SlotMinutes": {
			hold.Budget(budget().SlotMinutes(15).SlotMinutes(30)),
			[]string{"rule h", "a budget's SlotMinutes is given twice"},
		},
		"Moment.If":      {moment.If(always).If(always), []string{"rule m", "If is given twice"}},
		"Moment.Restart": {moment.Restart().Restart(), []string{"rule m", "Restart is given twice"}},
		"Moment.Late":    {moment.Late(time.Minute).Late(time.Hour), []string{"rule m", "Late is given twice"}},
		"Flow.If":        {flow.If(always).If(always), []string{"rule f", "If is given twice"}},
		"Flow.Restart":   {flow.Restart().Restart(), []string{"rule f", "Restart is given twice"}},
		"Flow.Priority":  {flow.Priority(Safety).Priority(Normal), []string{"rule f", "Priority is given twice"}},
		"Flow.Late":      {flow.Late(time.Minute).Late(time.Hour), []string{"rule f", "Late is given twice"}},
		"Control.OverrideHold": {
			valveControl().OverrideHold(time.Minute),
			[]string{"control on on pump", "OverrideHold is given twice"},
		},
		"Control.OverrideUntilNext": {
			NewControl(pump.To(false), PreferOn, ExternalOverride).OverrideUntilNext().OverrideUntilNext(),
			[]string{"control on on pump", "OverrideUntilNext is given twice"},
		},
		"Control.MinimumOn": {valveControl().MinimumOn(time.Hour), []string{"MinimumOn is given twice"}},
		"Control.MaximumOn": {valveControl().
			MaximumOn(time.Hour).
			MaximumOn(time.Hour), []string{"MaximumOn is given twice"}},
		"Control.MinimumOff": {valveControl().MinimumOff(time.Hour), []string{"MinimumOff is given twice"}},
		"Control.MaximumStartsPerDay": {
			valveControl().MaximumStartsPerDay(1).MaximumStartsPerDay(2),
			[]string{"MaximumStartsPerDay is given twice"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := NewHome(Options{}, tc.part).compile()
			if err == nil {
				t.Fatal("compiled")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("got %v, want %q", err, want)
				}
			}
		})
	}
}

func TestOptionsThatAddCanBeGivenAgain(t *testing.T) {
	run := func(*FlowRun) error { return nil }
	late, early := Clock("23:00"), Clock("05:00")
	moment := NewMoment("m", pump.To(true)).On(pump).On(water2).At(late).At(early)
	if len(moment.on) != 2 || len(moment.at) != 2 {
		t.Errorf("moment has %d changes and %d times", len(moment.on), len(moment.at))
	}
	flow := NewFlow("f", run).On(pump).On(water2).At(late).At(early)
	if len(flow.on) != 2 || len(flow.at) != 2 {
		t.Errorf("flow has %d changes and %d times", len(flow.on), len(flow.at))
	}
	window := Daily(late, early).Days(time.Monday).Days(time.Tuesday)
	if !slices.Equal(window.days, []time.Weekday{time.Monday, time.Tuesday}) {
		t.Errorf("window has days %v", window.days)
	}
	budget := NewBudget(func(Snapshot) int { return 60 }).
		Prefer(window, 1).Prefer(window, 2).Forbid(window).Forbid(window)
	if len(budget.prefer) != 2 || len(budget.forbid) != 2 {
		t.Errorf("budget has %d prefers and %d forbids", len(budget.prefer), len(budget.forbid))
	}
	if _, _, err := NewHome(Options{}, moment, flow).compile(); err != nil {
		t.Error(err)
	}
}

// water2 is a second change for the rules that add them.
var water2 = Bool{Device: "water2", Capability: On}

func TestAChainedMethodLeavesItsRuleAlone(t *testing.T) {
	always := ConditionFunc("always", func(Snapshot) bool { return true })
	// Spare capacity behind the base's slices is what would join two chains from it.
	base := NewMoment("m", pump.To(true)).On(pump).On(water2).On(pump).Restart()
	one, two := base.On(water2).If(always), base.On(pump).Late(time.Minute)
	if base.cond != nil || base.late != 0 || len(base.on) != 3 {
		t.Errorf("the base changed: %+v", base)
	}
	if got := one.on[3]; got != Change(water2) {
		t.Errorf("one's last change is %v", got)
	}
	if got := two.on[3]; got != Change(pump) {
		t.Errorf("two's last change is %v", got)
	}
	if err := one.once.err(); err != nil {
		t.Errorf("one: %v", err)
	}
	if err := two.once.err(); err != nil {
		t.Errorf("two: %v", err)
	}

	// The same for what records a repeat.
	hold := NewHold("h", pump.To(true)).When(always).OnDelay(time.Second).OffDelay(time.Second).Priority(Safety)
	twice, once := hold.When(always), hold.Latch()
	if twice.once.err() == nil {
		t.Error("the repeat went unreported")
	}
	if err := once.once.err(); err != nil {
		t.Errorf("the other chain has the repeat: %v", err)
	}
}

func TestAnEmptyLiteralStillFailsCheck(t *testing.T) {
	for name, part := range map[string]Part{
		"hold": Hold{}, "moment": Moment{}, "flow": Flow{}, "control": Control{}, "plugin": Plugin{},
	} {
		if _, _, err := NewHome(Options{}, part).compile(); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
}

func TestWhatNeedsMoreThanOneSettingStaysInCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		part Part
		want string
	}{
		"a hold with no condition": {NewHold("h", pump.To(true)), ""},
		"a moment with no trigger": {NewMoment("m", pump.To(true)), ""},
		"both overrides, one of them zero": {
			NewControl(pump.To(false), PreferOn, ExternalOverride).OverrideHold(0).OverrideUntilNext(),
			"alternatives",
		},
		"both overrides": {
			NewControl(pump.To(false), PreferOn, ExternalOverride).OverrideHold(time.Hour).OverrideUntilNext(),
			"alternatives",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := NewHome(Options{}, tc.part).compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
