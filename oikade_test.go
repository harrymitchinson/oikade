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
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/sun"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// deciding is a home with one decision, on d's on, of one case.
func deciding(o Options, name string, when ...Condition) *Home {
	h := NewHome(o)
	h.Decide(Bool{Device: "d", Capability: On}).Case(name, true, when...).Default(false)
	return h
}

// decide is a home that f adds to.
func decide(f func(h *Home)) *Home { return homeWith(Options{}, f) }

// homeWith is a home of o that f adds to.
func homeWith(o Options, f func(h *Home)) *Home {
	h := NewHome(o)
	f(h)
	return h
}

// nothing is a flow's body that does nothing.
func nothing(*FlowRun) {}

func TestCompileRejectsAnUnsoundHome(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	d := Bool{Device: "d", Capability: On}
	for name, tc := range map[string]struct {
		home *Home
		want string
	}{
		"no name":           {deciding(Options{}, "", when), "Case \"\": it needs a name"},
		"a space in a name": {deciding(Options{}, "a b", when), "its name has a space in it"},
		"twice": {decide(func(h *Home) {
			h.Decide(d).Case("a", true, when).Case("a", false, when).Default(false)
		}), `Case "a" is given twice`},
		"a rule's name twice": {decide(func(h *Home) {
			h.Decide(d).Case("a", true, when).Default(false)
			h.Flow("d on a", nothing).At(Clock("07:00"))
		}), "declared twice"},
		"unknown capability": {
			decide(func(h *Home) {
				h.Decide(Bool{Device: "d", Capability: "nope"}).Case("a", true, when).Default(false)
			}),
			"unknown capability",
		},
		"no trigger": {decide(func(h *Home) { h.Flow("a", nothing) }), "trigger"},
		"no capability": {
			decide(func(h *Home) { h.Decide(Ref[bool](nil)).Default(false) }), "Decide needs a capability",
		},
		"no device": {
			decide(func(h *Home) { h.Decide(Bool{Capability: On}).Default(false) }), "needs a device's capability",
		},
		"nil change": {decide(func(h *Home) { h.Flow("a", nothing).When(nil) }), "a nil change, at When[0]"},
		"a time that isn't one": {
			decide(func(h *Home) { h.Flow("bedtime", nothing).At(Clock("25:00")) }),
			`rule bedtime: timers[0]: time "25:00" is not HH:MM`,
		},
		"a window with no end":    {deciding(Options{}, "evening", Daily(Clock("18:00"), Time{})), "end: no time"},
		"a window that isn't one": {deciding(Options{}, "evening", d, Daily("18:00", "7")), `time "7" is not HH:MM`},
		"a date that isn't one":   {deciding(Options{}, "summer", Yearly("1 Jun", "31 Jun")), `"31 Jun" isn't a date`},
		"a Within the wrong way round": {
			deciding(Options{}, "comfy", Number{"t", Temperature}.Within(25, 18)), "give the lower first",
		},
		"a nil condition in All":   {deciding(Options{}, "a", All(d, nil)), "All has a nil condition, at 1"},
		"a case with no condition": {deciding(Options{}, "a"), "it needs a condition: Default is for always"},
		"a ConditionFunc with no label": {
			deciding(Options{}, "a", ConditionFunc("", func(Snapshot) bool { return true })),
			"a ConditionFunc needs a label",
		},
		"a flow's condition": {
			decide(func(h *Home) { h.Flow("a", nothing).When(d).If(Yearly("1 Jun", "June 30")) }), `"June 30" isn't a date`,
		},
		"a sunset with no site": {
			decide(func(h *Home) { h.Flow("porch-at-dusk", nothing).At(Sunset(0)) }),
			"rule porch-at-dusk: a sunrise or sunset needs the home's location",
		},
		"a sunrise window with no site": {
			deciding(Options{}, "a", Daily(Sunrise(0), Clock("09:00"))),
			"decision d on: Case \"a\": a sunrise or sunset needs the home's location",
		},
		"a latitude off the earth":  {deciding(Options{Location: NewLocation(91, 0)}, "a", when), "latitude 91 isn't"},
		"a longitude off the earth": {deciding(Options{Location: NewLocation(0, -181)}, "a", when), "longitude -181 isn't"},
		"a bridge name too long": {
			deciding(Options{MatterName: "The cottage at the end of the lane"}, "a", when),
			"is 34 bytes, and it's at most 32",
		},
		"a bridge name that isn't UTF-8": {
			deciding(Options{MatterName: "Caf\xe9"}, "a", when), "isn't valid UTF-8",
		},
		"an offset too far": {
			homeWith(Options{Location: NewLocation(51.48, 0)}, func(h *Home) {
				h.Flow("a", nothing).At(Sunrise(7 * time.Hour))
			}),
			"offset 7h0m0s is not between -6h and 6h",
		},
		"Follow on a bool": {
			decide(func(h *Home) {
				h.Decide(d).Follow("w", Number{"t", Temperature}, func(float64) bool { return true }).Default(false)
			}), `Follow "w" needs a number capability`,
		},
		"Follow with no function": {
			decide(func(h *Home) {
				h.Decide(Number{"r", Setpoint}).Follow("w", Number{"t", Temperature}, nil).Default(18)
			}), `Follow "w" needs a function`,
		},
		"Follow of no reading": {
			decide(func(h *Home) {
				h.Decide(Number{"r", Setpoint}).Follow("w", Number{}, func(v float64) float64 { return v }).Default(18)
			}), "a condition reads a device's capability",
		},
		"Step with no Follow": {
			decide(func(h *Home) {
				h.Decide(Number{"r", Setpoint}).Case("a", 7, d).Step(1).Default(18)
			}), "Step needs a Follow before it",
		},
		"a Step of zero": {
			decide(func(h *Home) {
				h.Decide(Number{"r", Setpoint}).Follow("w", Number{"t", Temperature}, func(v float64) float64 { return v }).
					Step(0).Default(18)
			}), `Follow "w": Step 0 must be positive`,
		},
		"Step twice": {
			decide(func(h *Home) {
				h.Decide(Number{"r", Setpoint}).Follow("w", Number{"t", Temperature}, func(v float64) float64 { return v }).
					Step(1).Step(2).Default(18)
			}), "Step is given twice",
		},
		"an average of an average": {
			deciding(Options{}, "a", Number{"t", Temperature}.AverageOver(time.Hour).AverageOver(time.Hour).Below(3)),
			"t temperature over 1h is an average already",
		},
		"an average over seconds": {
			deciding(Options{}, "a", Number{"t", Temperature}.AverageOver(time.Second).Below(3)),
			"AverageOver(1s) must be at least a minute",
		},
		"a budget's condition": {
			decide(func(h *Home) {
				h.Decide(d).Budget("b", true, NewBudget(func(Snapshot) int { return 1 }), Yearly("1 Jun", "June 30")).Default(false)
			}), `"June 30" isn't a date`,
		},
		"a flow ranked twice": {
			decide(func(h *Home) {
				f := h.Flow("f", nothing).When(d)
				h.Decide(Bool{Device: "e", Capability: On}).Flows(f, f).Default(false)
			}),
			"Flows(f) is given twice",
		},
		"a flow from another home": {
			decide(func(h *Home) {
				h.Decide(Bool{Device: "e", Capability: On}).Flows(NewHome(Options{}).Flow("f", nothing)).Default(false)
			}),
			"Flows(f) is a flow this home doesn't have",
		},
		"a nil flow ranked": {
			decide(func(h *Home) { h.Decide(Bool{Device: "e", Capability: On}).Flows(nil).Default(false) }),
			"Flows is given a nil flow",
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
	valve, fan, siren := Bool{"valve", On}, Bool{"fan", On}, Bool{"siren", On}
	m := decide(func(h *Home) {
		leak := h.Flow("leak", nothing).When(valve)
		h.Flow("bedtime", nothing).At(Clock("22:00"))
		h.Decide(valve).NoOverride().Case("leak", false, when).LatchUntil(when).Default(true)
		h.Decide(fan).OverrideFor(time.Hour).Case("humid", true, when).Flows().Default(false)
		h.Decide(siren).Flows(leak).OverrideUntilNext().Flows().Default(false)
	}).manifest()
	if m.Protocol != wire.Version {
		t.Fatalf("protocol %d", m.Protocol)
	}
	want := []wire.RuleInfo{
		{Name: "valve on leak", Kind: "case", Safety: true, Latch: true, Device: "valve", Capability: "on", Case: "leak"},
		{Name: "fan on humid", Kind: "case", Device: "fan", Capability: "on", Case: "humid"},
		{Name: "leak", Kind: "flow", Safety: true}, {Name: "bedtime", Kind: "flow"},
	}
	if !reflect.DeepEqual(m.Rules, want) {
		t.Fatalf("got %+v", m.Rules)
	}
	lines := map[string][]string{}
	for _, c := range m.Control {
		lines[c.Device] = c.Entry.Lines
	}
	wantLines := map[string][]string{
		"valve": {"Rule/valve on leak"},
		"fan":   {"override", "Rule/fan on humid", "flows"},
		"siren": {"Rule/leak", "override", "flows"},
	}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Errorf("got lines %v, want %v", lines, wantLines)
	}
	if len(m.Warnings) != 0 {
		t.Errorf("got warnings %v", m.Warnings)
	}
}

// A decision ranks an outside change where its override line is, and above every line without one.
func TestADecisionsOverrideLineSaysWhatAnOutsideChangeDoes(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	for name, tc := range map[string]struct {
		decide          func(*Decision[bool]) *Decision[bool]
		external, holds string
		lines           []string
		safety, warns   bool
	}{
		"none": {
			func(d *Decision[bool]) *Decision[bool] { return d.Case("a", true, when) },
			"Override", "Next", []string{"override", "Rule/d on a"}, false, false,
		},
		"for an hour": {
			func(d *Decision[bool]) *Decision[bool] { return d.Case("a", true, when).OverrideFor(time.Hour) },
			"Override", "1h0m0s", []string{"Rule/d on a", "override"}, true, false,
		},
		"until next": {
			func(d *Decision[bool]) *Decision[bool] { return d.OverrideUntilNext().Case("a", true, when) },
			"Override", "Next", []string{"override", "Rule/d on a"}, false, false,
		},
		"no override": {
			func(d *Decision[bool]) *Decision[bool] { return d.NoOverride().Case("a", true, when) },
			"Revert", "", []string{"Rule/d on a"}, true, false,
		},
		"a latch with no override line": {
			func(d *Decision[bool]) *Decision[bool] { return d.Case("a", true, when).LatchUntil(when) },
			"Override", "Next", []string{"override", "Rule/d on a"}, false, true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := decide(func(h *Home) { tc.decide(h.Decide(Bool{"d", On})).Default(false) })
			m := h.manifest()
			e := m.Control[0].Entry
			if e.External != tc.external || e.OverrideHold != tc.holds || !slices.Equal(e.Lines, tc.lines) {
				t.Errorf("got %+v", e)
			}
			if m.Rules[0].Safety != tc.safety {
				t.Errorf("safety is %v", m.Rules[0].Safety)
			}
			if (len(m.Warnings) > 0) != tc.warns {
				t.Errorf("got warnings %v", m.Warnings)
			}
			if _, _, err := h.compile(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestPluginsAreChecked(t *testing.T) {
	made := func(plugin.Environment, []contractv1.DeclaredDevice) (plugin.Handler, error) { return nil, nil }
	for name, add := range map[string]func(*Home){
		"no name":  func(h *Home) { h.Plugin("", made) },
		"bad name": func(h *Home) { h.Plugin("a/b", made) },
		"twice":    func(h *Home) { h.Plugin("a", made); h.Plugin("a", made) },
		"no New":   func(h *Home) { h.Plugin("a", nil) },
	} {
		if _, _, err := decide(add).compile(); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	if m := decide(func(h *Home) { h.Plugin("a", made) }).manifest(); !slices.Equal(m.Plugins, []string{"a"}) {
		t.Errorf("got %v", m.Plugins)
	}
}

var (
	pump  = Bool{Device: "pump", Capability: On}
	water = Number{Device: "water", Capability: Temperature}
)

var pumpLimits = Limits{MinimumOn: time.Hour, MinimumOff: 30 * time.Minute}

func budgetHome(l Limits, b Budget) *Home {
	return decide(func(h *Home) {
		h.Decide(pump).OverrideFor(time.Hour).Limits(l).Budget("daily", true, b).Default(false)
	})
}

func TestHomeChecksDecisionsAndBudgets(t *testing.T) {
	minutes := func(Snapshot) int { return 120 }
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	limited := pumpLimits
	limited.MinimumOn = 45 * time.Minute
	pumped := func(f func(d *Decision[bool])) *Home { return decide(func(h *Home) { f(h.Decide(pump)) }) }
	for name, tc := range map[string]struct {
		home *Home
		want string
	}{
		"fine": {budgetHome(pumpLimits, NewBudget(minutes)), ""},
		"bands": {budgetHome(pumpLimits, NewBandedBudget(Bands{
			Reading: water, Ranges: []Band{{Below: 12, Minutes: 60}}, Otherwise: 90,
		})), ""},
		"no minutes": {budgetHome(pumpLimits, Budget{}), "exactly one of bands or minutes"},
		"bad slot":   {budgetHome(pumpLimits, NewBudget(minutes).SlotMinutes(20)), "slotMinutes"},
		"bad window": {
			budgetHome(pumpLimits, NewBudget(minutes).Forbid(Daily("7", "8"))),
			"not HH:MM",
		},
		"limit off the slots": {budgetHome(limited, NewBudget(minutes)), "isn't a multiple"},
		"a budget of false": {
			pumped(func(d *Decision[bool]) { d.Budget("daily", false, NewBudget(minutes)).Default(false) }),
			"a budget sets a Bool to true",
		},
		"twice": {decide(func(h *Home) {
			h.Decide(pump).Default(false)
			h.Decide(pump).Default(true)
		}), "decision pump on is decided twice"},
		"no Default":    {pumped(func(d *Decision[bool]) { d.Case("a", true, when) }), "it needs a Default"},
		"Default twice": {pumped(func(d *Decision[bool]) { d.Default(false).Default(true) }), "Default is given twice"},
		"a line after Default": {
			pumped(func(d *Decision[bool]) { d.Default(false).Case("late", true, when) }),
			`Case "late" comes after Default`,
		},
		"two override lines": {
			pumped(func(d *Decision[bool]) { d.OverrideFor(time.Hour).OverrideUntilNext().Default(false) }),
			"it has one override line",
		},
		"NoOverride after a line": {
			pumped(func(d *Decision[bool]) { d.Case("a", true, when).NoOverride().Default(false) }),
			"NoOverride goes before the first line",
		},
		"an override of zero": {
			pumped(func(d *Decision[bool]) { d.OverrideFor(0).Default(false) }), "OverrideFor 0s must be positive",
		},
		"Linger with no case": {
			pumped(func(d *Decision[bool]) { d.Flows().Linger(time.Minute).Default(false) }),
			"Linger needs a Case before it",
		},
		"Linger twice": {
			pumped(func(d *Decision[bool]) { d.Case("a", true, when).Linger(0).Linger(time.Minute).Default(false) }),
			`Case "a": Linger is given twice`,
		},
		"a negative Delay": {
			pumped(func(d *Decision[bool]) { d.Case("a", true, when).Delay(-time.Minute).Default(false) }),
			"Delay -1m0s must not be negative",
		},
		"LatchUntil nil": {
			pumped(func(d *Decision[bool]) { d.Case("a", true, when).LatchUntil(nil).Default(false) }),
			"LatchUntil needs a condition",
		},
		"Flows twice": {pumped(func(d *Decision[bool]) { d.Flows().Flows().Default(false) }), "Flows is given twice"},
		"Limits twice": {
			pumped(func(d *Decision[bool]) { d.Limits(pumpLimits).Limits(pumpLimits).Default(false) }),
			"Limits is given twice",
		},
		"limits the wrong way round": {
			pumped(func(d *Decision[bool]) {
				d.Limits(Limits{MinimumOn: time.Hour, MaximumOn: time.Minute}).Default(false)
			}),
			"maximumOn must not be shorter than minimumOn",
		},
		"read-only": {
			decide(func(h *Home) { h.Decide(Number{Device: "x", Capability: Temperature}).Default(1) }), "read-only",
		},
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

func TestMetaHasTheLimitsOfTheHomesDecision(t *testing.T) {
	h := budgetHome(pumpLimits, NewBudget(func(Snapshot) int { return 60 }))
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

func TestABudgetTakesClockTimesOnly(t *testing.T) {
	b := NewBudget(func(Snapshot) int { return 60 }).Prefer(Daily(Sunset(0), Clock("23:00")), 3)
	home := budgetHome(pumpLimits, b)
	home.options = Options{Location: NewLocation(51.48, -0.01)}
	if _, _, err := home.compile(); err == nil || !strings.Contains(err.Error(), "clock times only") {
		t.Fatalf("got %v, want a budget's windows to take clock times only", err)
	}
}

func TestASunTimeReachesTheEngine(t *testing.T) {
	f := NewHome(Options{}).Flow("a", nothing).At(Sunset(-20*time.Minute), Sunrise(0), Clock("18:00"))
	want := []engine.Timer{
		engine.SunTime{Event: sun.Sunset, Offset: -20 * time.Minute}, engine.SunTime{Event: sun.Sunrise},
		engine.At{Time: "18:00"},
	}
	if got := f.toEngine(&flow.Flow{}).Timers; !reflect.DeepEqual(got, want) {
		t.Errorf("timers %+v, want %+v", got, want)
	}
}

func TestAWindowReachesTheEngine(t *testing.T) {
	d := decide(func(h *Home) {
		h.Decide(pump).Case("night", true, Daily("22:00", "06:00").Days(time.Friday)).Default(false)
	}).decisions[0].compile(Options{})
	want := &engine.Window{
		Start: engine.At{Time: "22:00"}, End: engine.At{Time: "06:00"}, Days: []time.Weekday{time.Friday},
	}
	if got := d.cases[0].hold.Window; !reflect.DeepEqual(got, want) {
		t.Errorf("window %+v, want %+v", got, want)
	}
	if d.cases[0].hold.When != nil {
		t.Error("a Daily on its own left a When")
	}
}

// A home keeps its rules in the order they're added, and a helper that adds them keeps the names it
// was given.
func TestAHomeKeepsWhatItsHelpersAdd(t *testing.T) {
	made := func(plugin.Environment, []contractv1.DeclaredDevice) (plugin.Handler, error) { return nil, nil }
	motion := func(h *Home, name string) { h.Flow(name, nothing).At(Clock("05:00")) }
	h := decide(func(h *Home) {
		motion(h, "landing")
		h.Plugin("relays", made)
		motion(h, "porch")
	})
	if _, _, err := h.compile(); err != nil {
		t.Fatal(err)
	}
	m := h.manifest()
	names := make([]string, 0, len(m.Rules))
	for _, r := range m.Rules {
		names = append(names, r.Name)
	}
	if want := []string{"landing", "porch"}; !slices.Equal(names, want) {
		t.Errorf("got rules %v, want %v", names, want)
	}
	if !slices.Equal(m.Plugins, []string{"relays"}) {
		t.Errorf("got plugins %v", m.Plugins)
	}
	motion(h, "landing")
	if _, _, err := h.compile(); err == nil || !strings.Contains(err.Error(), `rule "landing" is declared twice`) {
		t.Fatalf("got %v", err)
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
	dusk := func(h *Home) { h.Flow("a", nothing).At(Sunset(0)) }
	for _, o := range []Options{
		{Location: NewLocation(51.48, 0)}, {Location: NewLocation(0, -0.01)}, {Location: NewLocation(0, 0)},
	} {
		if _, _, err := homeWith(o, dusk).compile(); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if _, _, err := decide(dusk).compile(); err == nil {
		t.Error("no location: compiled")
	}
}

func TestAnEmptyHomeIsValid(t *testing.T) {
	for name, h := range map[string]*Home{"zero": {}, "made": NewHome(Options{})} {
		if _, _, err := h.compile(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestASimulatedHomeReportsAMistakeInIt(t *testing.T) {
	_, err := decide(func(h *Home) { h.Flow("a", nothing) }).simulated()
	if err == nil || !strings.Contains(err.Error(), "rule a") {
		t.Fatalf("got %v", err)
	}
}

func TestAnOptionGivenTwiceFailsNamingTheRuleAndTheOption(t *testing.T) {
	always := ConditionFunc("always", func(Snapshot) bool { return true })
	flowed := func(f func(*Flow)) *Home { return decide(func(h *Home) { f(h.Flow("f", nothing).When(pump)) }) }
	budget := func() Budget { return NewBudget(func(Snapshot) int { return 60 }) }
	budgeted := func(b Budget) *Home {
		return decide(func(h *Home) { h.Decide(pump).Budget("b", true, b).Default(false) })
	}
	for name, tc := range map[string]struct {
		home *Home
		want []string
	}{
		"Case.Delay": {decide(func(h *Home) {
			h.Decide(pump).Case("c", true, always).Delay(0).Delay(time.Second).Default(false)
		}), []string{"decision pump on", `Case "c"`, "Delay is given twice"}},
		"Case.LatchUntil": {decide(func(h *Home) {
			h.Decide(pump).Case("c", true, always).LatchUntil(always).LatchUntil(always).Default(false)
		}), []string{"LatchUntil is given twice"}},
		"Budget.PlanningTime": {
			budgeted(budget().PlanningTime("01:00").PlanningTime("02:00")),
			[]string{`Budget "b"`, "a budget's PlanningTime is given twice"},
		},
		"Budget.SlotMinutes": {
			budgeted(budget().SlotMinutes(15).SlotMinutes(30)),
			[]string{`Budget "b"`, "a budget's SlotMinutes is given twice"},
		},
		"Flow.If":      {flowed(func(f *Flow) { f.If(always).If(always) }), []string{"rule f", "If is given twice"}},
		"Flow.Restart": {flowed(func(f *Flow) { f.Restart().Restart() }), []string{"rule f", "Restart is given twice"}},
		"Flow.Late": {
			flowed(func(f *Flow) { f.Late(time.Minute).Late(time.Hour) }), []string{"rule f", "Late is given twice"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := tc.home.compile()
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
	late, early := Clock("23:00"), Clock("05:00")
	h := NewHome(Options{})
	f := h.Flow("f", nothing).When(pump).When(water2).At(late).At(early)
	if len(f.on) != 2 || len(f.at) != 2 {
		t.Errorf("flow has %d changes and %d times", len(f.on), len(f.at))
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
	if _, _, err := h.compile(); err != nil {
		t.Error(err)
	}
}

// water2 is a second change for the rules that add them.
var water2 = Bool{Device: "water2", Capability: On}

func TestAFlowWithNoBodyFailsCheck(t *testing.T) {
	if _, _, err := decide(func(h *Home) { h.Flow("a", nil).At(Clock("05:00")) }).compile(); err == nil {
		t.Error("compiled")
	}
}
