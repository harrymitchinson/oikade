package hometest_test

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade"
	"github.com/harrymitchinson/oikade/hometest"
	"github.com/harrymitchinson/oikade/internal/sun"
)

const (
	on       = string(oikade.On)
	humidity = string(oikade.Humidity)
)

// The devices' handles, as oikade types would write them.
var (
	sensor = oikade.Number{Device: "sensor", Capability: oikade.Humidity}
	fan    = oikade.Bool{Device: "fan", Capability: oikade.On}
	pump   = oikade.Bool{Device: "pump", Capability: oikade.On}
	light  = oikade.Bool{Device: "light", Capability: oikade.On}
)

func devices() []hometest.Device {
	return []hometest.Device{
		{Name: "sensor", Capabilities: []string{humidity}, Values: map[string]any{humidity: 40.0}},
		{Name: "fan", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "pump", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "light", Capabilities: []string{on}, Values: map[string]any{on: true}},
	}
}

// fanHome is a fan that runs while the sensor is humid, and ten minutes after.
func fanHome() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(fan).
		Case("humid", true, sensor.Above(70)).Linger(10 * time.Minute).
		Default(false)
	return home
}

// decided is a home with one decision, on fan's on or pump's, of one case.
func decided(o oikade.Options, b oikade.Bool, name string, v bool, when ...oikade.Condition) *oikade.Home {
	home := oikade.NewHome(o)
	home.Decide(b).Case(name, v, when...).Default(!v)
	return home
}

func TestACaseFollowsADeviceAndTheClock(t *testing.T) {
	h := hometest.New(t, fanHome(), hometest.Options{Devices: devices()})
	h.ExpectValue(fan, false)
	h.Set(sensor, 80.0)
	h.ExpectValue(fan, true)
	h.Set(sensor, 50.0)
	h.Advance(9 * time.Minute)
	h.ExpectValue(fan, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(fan, false)
	if got := len(h.Writes()); got != 2 {
		t.Errorf("%d writes, want 2: %v", got, h.Writes())
	}
}

// setting is a flow's body that sets each of sets.
func setting(sets ...oikade.Set) func(*oikade.FlowRun) {
	return func(r *oikade.FlowRun) {
		for _, s := range sets {
			r.Set(s)
		}
	}
}

func bedtime() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("bedtime", func(r *oikade.FlowRun) {
		r.Sleep(30 * time.Minute)
		r.Set(light.To(false))
	}).At(oikade.Clock("22:30"))
	return home
}

func TestARestartMidFlowCarriesOn(t *testing.T) {
	h := hometest.New(t, bedtime(), hometest.Options{Devices: devices()})
	h.Advance(22*time.Hour + 40*time.Minute)
	if h.Finished("bedtime") {
		t.Fatal("finished before its wait ended")
	}
	h.ExpectValue(light, true)
	h.Restart()
	h.ExpectValue(light, true)
	h.Advance(19 * time.Minute)
	h.ExpectValue(light, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(light, false)
	h.ExpectFinished("bedtime")
}

// A flow's Get and Is read the home, and after a restart they return what they read the first time,
// so the run takes the same path.
func TestAFlowsReadsReplayAfterARestart(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("airing", func(r *oikade.FlowRun) {
		damp := r.Get(sensor) > 60
		dark := r.Is(oikade.All(oikade.Not(light), sensor.Above(60)))
		r.Sleep(time.Hour)
		if !damp || !dark {
			r.Set(pump.To(true)) // The wrong path: the run began damp and dark.
			return
		}
		r.Set(fan.To(true))
	}).When(light.Becomes(false))
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set(sensor, 70)
	h.Set(light, false)
	h.Advance(time.Minute)
	h.Set(sensor, 40)
	h.Set(light, true)
	h.Restart()
	h.Advance(time.Hour)
	h.ExpectFinished("airing")
	h.ExpectValue(fan, true)
	h.ExpectValue(pump, false)
}

// A Becomes fires on a change to its value, and not on one away from it.
func TestABecomesFiresOnlyOnItsValue(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("lights-out", setting(fan.To(true))).When(light.Becomes(false))
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set(light, false)
	h.ExpectValue(fan, true)
	h.Set(fan, false)
	h.Set(light, true)
	h.ExpectValue(fan, false)
}

// A flow's Get of a value nobody has reported yet ends the run as failed.
func TestAFlowsGetOfAnUnreportedValueFails(t *testing.T) {
	unheard := oikade.Number{Device: "unheard", Capability: oikade.Humidity}
	home := oikade.NewHome(oikade.Options{})
	home.Flow("reads", func(r *oikade.FlowRun) {
		if r.Get(unheard) > 60 {
			r.Set(fan.To(true))
		}
	}).When(light)
	h := hometest.New(t, home, hometest.Options{Devices: append(devices(),
		hometest.Device{Name: "unheard", Capabilities: []string{humidity}})})
	h.Set(light, false)
	if h.Finished("reads") {
		t.Fatal("a run that read nothing finished")
	}
}

func arbitrated() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(pump).
		Case("night", true, oikade.Daily("01:00", "02:00")).
		OverrideFor(time.Hour).
		Default(false)
	return home
}

func TestClaimsAreArbitrated(t *testing.T) {
	h := hometest.New(t, arbitrated(), hometest.Options{Devices: devices()})
	h.Advance(90 * time.Minute)
	h.ExpectValue(pump, true)
	h.ExpectClaim(pump, "Rule/pump on night", "Safety")
	// A tap in an app loses to a safety line.
	h.Set(pump, false)
	h.ExpectClaim(pump, "Rule/pump on night", "Safety")
	h.Advance(time.Hour)
	h.ExpectValue(pump, false)
	if got := h.Claims(pump); len(got) != 0 {
		t.Errorf("claims %+v after the window", got)
	}
}

func TestABudgetRunsItsMinutes(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(pump).
		OverrideFor(time.Hour).
		Budget("daily", true, oikade.NewBudget(func(oikade.Snapshot) int { return 60 })).
		Default(false)
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.ExpectValue(pump, true)
	h.Restart() // A deploy mustn't add to the day's run.
	h.Advance(61 * time.Minute)
	h.ExpectValue(pump, false)
	h.Advance(20 * time.Hour)
	h.ExpectValue(pump, false)
	if got := len(h.Writes()); got != 2 {
		t.Errorf("%d writes, want on and off: %v", got, h.Writes())
	}
}

func TestAFlowSleeps(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("porch", func(r *oikade.FlowRun) {
		r.Set(fan.To(true))
		r.Sleep(5 * time.Minute)
		r.Set(fan.To(false))
	}).When(sensor)
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set(sensor, 41.0)
	h.ExpectValue(fan, true)
	h.Advance(6 * time.Minute)
	h.ExpectValue(fan, false)
	h.ExpectFinished("porch")
}

func TestASleepEndsWhenItSaysInsideALongerMove(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("porch", func(r *oikade.FlowRun) {
		r.Sleep(90 * time.Second)
		r.Set(fan.To(true))
	}).When(sensor)
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	start := h.Now()
	h.Set(sensor, 41.0)
	h.Advance(time.Hour)
	writes := h.Writes()
	if len(writes) != 1 || !writes[0].Time.Equal(start.Add(90*time.Second)) {
		t.Fatalf("writes %v, want one at %s", writes, start.Add(90*time.Second))
	}
}

func TestARunInProgressIsntFinished(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("porch", func(r *oikade.FlowRun) { r.Sleep(5 * time.Minute) }).When(sensor)
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set(sensor, 41.0)
	h.Advance(6 * time.Minute)
	h.ExpectFinished("porch")
	h.Set(sensor, 42.0)
	if h.Finished("porch") {
		t.Fatal("finished while its second run waits")
	}
	h.Advance(6 * time.Minute)
	h.ExpectFinished("porch")
}

// "Living room off" limited to lights turns off the lamps and leaves the fridge's plug, in the same
// room, on.
func TestARoomWriteLimitedToLightsLeavesThePlugOn(t *testing.T) {
	brightness := string(oikade.Brightness)
	lamp := oikade.Bool{Device: "floor-lamp", Capability: oikade.On}
	ceiling := oikade.Bool{Device: "ceiling-light", Capability: oikade.On}
	fridge := oikade.Bool{Device: "fridge-plug", Capability: oikade.On}
	home := oikade.NewHome(oikade.Options{})
	lights := home.Room("living-room", oikade.Lights).On
	home.Flow("living-room-off", setting(lights.To(false))).At(oikade.Clock("23:00"))
	h := hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{
			Name: "floor-lamp", Room: "living-room", Capabilities: []string{on, brightness},
			Values: map[string]any{on: true, brightness: 80.0},
		},
		{
			Name: "ceiling-light", Room: "living-room", Capabilities: []string{on}, DeviceType: oikade.OnOffLight,
			Values: map[string]any{on: true},
		},
		{Name: "fridge-plug", Room: "living-room", Capabilities: []string{on}, Values: map[string]any{on: true}},
	}})
	h.Advance(23*time.Hour + time.Minute)
	h.ExpectValue(lamp, false)
	h.ExpectValue(ceiling, false)
	h.ExpectValue(fridge, true)
}

// studyHome turns the study's devices of kind k off at 23:00: a heater and a lamp, each on.
func studyHome(t *testing.T, k oikade.Kind) *hometest.Home {
	t.Helper()
	home := oikade.NewHome(oikade.Options{})
	off := home.Room("study", k).On.To(false)
	home.Flow("study-off", setting(off)).At(oikade.Clock("23:00"))
	setpoint := string(oikade.Setpoint)
	return hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{
			Name: "study-heater", Room: "study", Capabilities: []string{on, setpoint},
			Values: map[string]any{on: true, setpoint: 20.0},
		},
		{
			Name: "desk-lamp", Room: "study", Capabilities: []string{on}, DeviceType: oikade.OnOffLight,
			Values: map[string]any{on: true},
		},
	}})
}

var (
	studyHeater = oikade.Bool{Device: "study-heater", Capability: oikade.On}
	deskLamp    = oikade.Bool{Device: "desk-lamp", Capability: oikade.On}
)

func TestARoomsEverythingTurnsOffItsHeaterToo(t *testing.T) {
	h := studyHome(t, oikade.Everything)
	h.Advance(23*time.Hour + time.Minute)
	h.ExpectValue(studyHeater, false)
	h.ExpectValue(deskLamp, false)
}

func TestARoomsHeatersTurnsOffOnlyTheHeater(t *testing.T) {
	h := studyHome(t, oikade.Heaters)
	h.Advance(23*time.Hour + time.Minute)
	h.ExpectValue(studyHeater, false)
	h.ExpectValue(deskLamp, true)
}

func TestARoomsLightsLeavesTheHeaterOn(t *testing.T) {
	h := studyHome(t, oikade.Lights)
	h.Advance(23*time.Hour + time.Minute)
	h.ExpectValue(studyHeater, true)
	h.ExpectValue(deskLamp, false)
}

// A trigger on a room's heaters fires when the heater turns on, and not when the lamp beside it does.
func TestATriggerOnARoomsHeatersIgnoresItsLights(t *testing.T) {
	fan := oikade.Bool{Device: "study-fan", Capability: oikade.On}
	home := oikade.NewHome(oikade.Options{})
	home.Flow("fan-with-heat", setting(fan.To(true))).When(home.Room("study", oikade.Heaters).On)
	setpoint := string(oikade.Setpoint)
	h := hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{
			Name: "study-heater", Room: "study", Capabilities: []string{on, setpoint},
			Values: map[string]any{on: false, setpoint: 20.0},
		},
		{
			Name: "desk-lamp", Room: "study", Capabilities: []string{on}, DeviceType: oikade.OnOffLight,
			Values: map[string]any{on: false},
		},
		{Name: "study-fan", Room: "hall", Capabilities: []string{on}, Values: map[string]any{on: false}},
	}})
	h.Set(deskLamp, true)
	h.ExpectValue(fan, false)
	h.Set(studyHeater, true)
	h.ExpectValue(fan, true)
}

func TestAClockTimeFiresAcrossADaylightSavingChange(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	home := oikade.NewHome(oikade.Options{})
	home.Flow("lights-out", setting(light.To(false))).At(oikade.Clock("23:30"))
	// The clocks go back at 02:00 on Sunday 27 October 2030, so that day is 25 hours long.
	h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: time.Date(2030, 10, 26, 23, 0, 0, 0, london)})
	h.Advance(31 * time.Minute)
	h.ExpectValue(light, false)
	h.Set(light, true)
	h.Advance(24 * time.Hour) // 22:31 on Sunday
	h.ExpectValue(light, true)
	h.Advance(time.Hour)
	h.ExpectValue(light, false)
	if got := h.Writes(); len(got) != 2 || !got[1].Time.Equal(time.Date(2030, 10, 27, 23, 30, 0, 0, london)) {
		t.Errorf("writes %+v, want one at 23:30 each night", got)
	}
}

func TestADailyIsOpenOvernight(t *testing.T) {
	home := decided(oikade.Options{}, pump, "night", true, oikade.Daily("22:00", "06:00"))
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.ExpectValue(pump, true) // midnight is inside the window that opened at 22:00 the night before
	h.Advance(5*time.Hour + 59*time.Minute)
	h.ExpectValue(pump, true)
	h.Advance(2 * time.Minute) // 06:01
	h.ExpectValue(pump, false)
	h.Advance(15*time.Hour + 57*time.Minute) // 21:58
	h.ExpectValue(pump, false)
	h.Advance(2 * time.Minute)
	h.ExpectValue(pump, true)
}

func TestTheSameTimeTwiceFiresOnce(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("lights-out", setting(light.To(false))).At(oikade.Clock("23:30"), oikade.Clock("23:30"))
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Advance(23*time.Hour + 31*time.Minute)
	h.ExpectValue(light, false)
	h.Set(light, true)
	h.Advance(time.Minute)
	h.ExpectValue(light, true)
	if got := h.Writes(); len(got) != 1 {
		t.Errorf("writes %+v, want one", got)
	}
}

// A hold over a room runs on the sensors that have reported, and waits while none has.
func TestAHoldOverARoomRunsOnTheSensorsThatHaveReported(t *testing.T) {
	cellar := func(name string) oikade.Number {
		return oikade.Number{Device: name, Capability: oikade.Humidity}
	}
	dehumidifier := oikade.Bool{Device: "dehumidifier", Capability: oikade.On}
	home := oikade.NewHome(oikade.Options{})
	room := home.Room("cellar")
	home.Decide(dehumidifier).Case("damp", true, oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool {
		return slices.Max(s.Readings(room.Humidity)) > 65
	})).Default(false)
	h := hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{Name: "cellar-1", Room: "cellar", Capabilities: []string{humidity}},
		{Name: "cellar-2", Room: "cellar", Capabilities: []string{humidity}},
		{Name: "cellar-3", Room: "cellar", Capabilities: []string{humidity}},
		{Name: "attic", Room: "attic", Capabilities: []string{humidity}, Values: map[string]any{humidity: 90.0}},
		{Name: "dehumidifier", Capabilities: []string{on}, Values: map[string]any{on: false}},
	}})
	h.Advance(5 * time.Minute)
	if got := len(h.Writes()); got != 0 {
		t.Fatalf("wrote before any cellar sensor reported: %v", h.Writes())
	}
	h.Set(cellar("cellar-1"), 50.0)
	h.Advance(time.Minute)
	h.ExpectValue(dehumidifier, false)
	h.Set(cellar("cellar-2"), 70.0)
	h.ExpectValue(dehumidifier, true)
	h.Set(cellar("cellar-2"), 60.0)
	h.ExpectValue(dehumidifier, false)
}

// greenwich is a home's location, rounded to two decimals.
var greenwich = oikade.Options{Location: oikade.NewLocation(greenwichSite.Latitude, greenwichSite.Longitude)}

var greenwichSite = sun.Site{Latitude: 51.48, Longitude: -0.01}

// sunset is when the sun sets at greenwich on a day, which internal/sun checks against published tables.
func sunset(t *testing.T, day time.Time) time.Time {
	t.Helper()
	at, ok := greenwichSite.
		Time(sun.Sunset, day.Year(), day.Month(), day.Day(), day.Location())
	if !ok {
		t.Fatalf("the sun doesn't set on %v", day)
	}
	return at
}

func TestAFlowFiresBeforeSunset(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	home := oikade.NewHome(greenwich)
	home.Flow("porch-at-dusk", setting(light.To(false))).At(oikade.Sunset(-20 * time.Minute))
	// Midwinter and midsummer, whose sunsets are more than five hours apart.
	for _, day := range []time.Time{
		time.Date(2030, 1, 7, 0, 0, 0, 0, london), time.Date(2030, 6, 21, 0, 0, 0, 0, london),
	} {
		h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: day})
		h.Advance(24 * time.Hour)
		want := sunset(t, day).Add(-20 * time.Minute)
		if got := h.Writes(); len(got) != 1 || !got[0].Time.Equal(want) {
			t.Errorf("on %s, writes %+v, want one at %v", day.Format(time.DateOnly), got, want)
		}
	}
}

func TestADailyOpensAtSunset(t *testing.T) {
	home := decided(greenwich, pump, "evening", true, oikade.Daily(oikade.Sunset(0), oikade.Clock("23:30")))
	start := time.Date(2030, 3, 20, 0, 0, 0, 0, time.UTC)
	h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: start})
	opens := sunset(t, start)
	h.Advance(opens.Sub(start) - time.Minute)
	h.ExpectValue(pump, false)
	h.Advance(2 * time.Minute)
	h.ExpectValue(pump, true)
	h.Advance(start.Add(23*time.Hour + 29*time.Minute).Sub(h.Now()))
	h.ExpectValue(pump, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(pump, false)
}

// In Svalbard in June the sun doesn't set, so a sunset neither fires nor opens a window.
func TestADayWithNoSunsetDoesNothing(t *testing.T) {
	svalbard := oikade.Options{Location: oikade.NewLocation(78.22, 15.65)}
	home := oikade.NewHome(svalbard)
	home.Flow("dusk", setting(light.To(false))).At(oikade.Sunset(0))
	home.Decide(pump).Case("evening", true, oikade.Daily(oikade.Sunset(0), oikade.Clock("23:30"))).Default(false)
	h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: time.Date(2030, 6, 21, 0, 0, 0, 0, time.UTC)})
	for range 24 {
		h.Advance(time.Hour)
		h.ExpectValue(pump, false)
	}
	h.ExpectValue(light, true)
	if got := h.Writes(); len(got) != 0 {
		t.Errorf("writes %+v, want none", got)
	}
}

// A case whose Daily is inside another condition applies within a minute of it, with no device
// changing.
func TestACaseWithADailyInsideAnyAppliesWithinAMinute(t *testing.T) {
	home := decided(oikade.Options{}, light, "evening", false, oikade.Any(oikade.Daily("18:00", "07:00")))
	h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)})
	h.Advance(6*time.Hour - time.Second)
	h.ExpectValue(light, true)
	h.Advance(time.Minute + time.Second)
	h.ExpectValue(light, false)
}

// A Daily inside Any and a hold's own Daily agree, minute by minute, across the night the clocks go
// back, when 01:00 to 02:00 happens twice.
func TestADailyInsideAnyAgreesWithAHoldsDailyAcrossDaylightSaving(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	w := oikade.Daily("01:30", "02:15")
	// Clocks go back at 02:00 BST on Sunday 27 October 2030.
	agree(t, w, oikade.Options{}, time.Date(2030, 10, 26, 23, 0, 0, 0, london), 5*time.Hour)
}

// A Daily with a sunset edge inside Any agrees with a hold's own Daily with the same edge.
func TestADailyInsideAnyAgreesWithAHoldsDailyAtSunset(t *testing.T) {
	w := oikade.Daily(oikade.Sunset(0), oikade.Clock("23:30"))
	start := time.Date(2030, 3, 20, 12, 0, 0, 0, time.UTC)
	agree(t, w, greenwich, start, 12*time.Hour)
}

// agree runs a hold whose When is w, which the engine times by its edges, beside one whose When is
// Any(w), which it looks at each minute, and checks they agree after every minute.
func agree(t *testing.T, w oikade.Hours, site oikade.Options, start time.Time, d time.Duration) {
	t.Helper()
	home := oikade.NewHome(site)
	home.Decide(fan).Case("by-window", true, w).Default(false)
	home.Decide(pump).Case("by-any", true, oikade.Any(w)).Default(false)
	h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: start})
	opened := 0
	for range int(d / time.Minute) {
		h.Advance(time.Minute)
		byWindow, _ := h.Value(fan)
		byAny, _ := h.Value(pump)
		if byWindow != byAny {
			t.Fatalf("at %s the hold's Daily says %v and Any says %v", h.Now(), byWindow, byAny)
		}
		if byWindow {
			opened++
		}
	}
	if opened == 0 {
		t.Fatal("the window never opened")
	}
}

// fatalT records a Fatal or Fatalf inside fails instead of failing the test, so a test can see an
// Expect fail. Anywhere else, they fail the test as usual. fails runs expect on a goroutine of its
// own, so the test's must never see either: each is caught there, or passed on outside fails.
type fatalT struct {
	testing.TB
	catching bool
	failed   string
}

func (f *fatalT) Fatalf(format string, args ...any) {
	if !f.catching {
		f.TB.Fatalf(format, args...)
	}
	f.failed = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (f *fatalT) Fatal(args ...any) {
	if !f.catching {
		f.TB.Fatal(args...)
	}
	f.failed = fmt.Sprint(args...)
	runtime.Goexit()
}

// fails says whether expect called Fatal or Fatalf on f.
func (f *fatalT) fails(expect func()) bool {
	f.catching, f.failed = true, ""
	defer func() { f.catching = false }()
	done := make(chan struct{})
	go func() { defer close(done); expect() }()
	<-done
	return f.failed != ""
}

// A write counts once made, even after the device has moved on, unlike a value.
func TestExpectWriteSeesAWriteTheDeviceMovedOnFrom(t *testing.T) {
	ft := &fatalT{TB: t}
	h := hometest.New(ft, fanHome(), hometest.Options{Devices: devices()})
	if !ft.fails(func() { h.ExpectWrite(fan, true) }) {
		t.Error("ExpectWrite passed before anything wrote")
	}
	h.Set(sensor, 80.0)
	h.Set(sensor, 50.0)
	h.Advance(11 * time.Minute)
	h.ExpectValue(fan, false)
	if ft.fails(func() { h.ExpectWrite(fan, true) }) {
		t.Errorf("ExpectWrite missed the write: %s", ft.failed)
	}
	if !ft.fails(func() { h.ExpectWrite(light, false) }) {
		t.Error("ExpectWrite passed for a device nothing wrote to")
	}
}

// A report that sets off a safety line and a lower one on the same device at once writes only the
// safety line's value: the lower one never gets a moment to win (OIK-212). Either value may come
// first in the report, and the lower one may be a case or a flow.
func TestOneReportWritesOnlyTheSafetyValue(t *testing.T) {
	occupied := oikade.Bool{Device: "hall", Capability: oikade.Occupied}
	damp := oikade.Number{Device: "hall", Capability: oikade.Humidity}
	hot := oikade.Number{Device: "hall", Capability: oikade.Temperature}
	heater := oikade.Bool{Device: "heater", Capability: oikade.On}
	for _, tc := range []struct {
		name   string
		safety oikade.Condition
		report oikade.Set
		flow   bool
	}{
		// Humidity sorts before occupied, and temperature after it.
		{name: "safety first", safety: damp.Above(65), report: damp.To(72)},
		{name: "safety last", safety: hot.Above(30), report: hot.To(35)},
		{name: "a flow", safety: hot.Above(30), report: hot.To(35), flow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := oikade.NewHome(oikade.Options{})
			if tc.flow {
				home.Flow("welcome", setting(heater.To(true).For(time.Hour))).When(occupied)
			}
			d := home.Decide(heater).
				Case("cutoff", false, tc.safety).
				OverrideFor(time.Hour)
			if tc.flow {
				d = d.Flows()
			} else {
				d = d.Case("warm", true, occupied)
			}
			d.Default(false)
			// The bug was a race, so give it room to show.
			for range 20 {
				h := hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
					{
						Name: "hall", Capabilities: []string{humidity, string(oikade.Occupied), string(oikade.Temperature)},
						Values: map[string]any{humidity: 40.0, string(oikade.Occupied): false, string(oikade.Temperature): 20.0},
					},
					{Name: "heater", Capabilities: []string{on}, Values: map[string]any{on: false}},
				}})
				h.Report(occupied.To(true), tc.report)
				h.ExpectValue(heater, false)
				for _, w := range h.Writes() {
					if w.Device == "heater" && w.Value == true {
						t.Fatalf("wrote the heater on: %v", h.Writes())
					}
				}
				h.ExpectClaim(heater, "Rule/heater on cutoff", "Safety")
			}
		})
	}
}

// A latched case whose condition is a Daily latches at its start, and lets go when its LatchUntil
// is true.
func TestALatchedDailyCaseLatchesUntilItsCondition(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(fan).
		Case("night", true, oikade.Daily("01:00", "02:00")).LatchUntil(oikade.Not(light)).
		OverrideFor(time.Hour).
		Default(false)
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Advance(90 * time.Minute)
	h.ExpectValue(fan, true)
	h.Advance(2 * time.Hour)
	h.ExpectValue(fan, true)
	h.Set(light, false)
	h.ExpectValue(fan, false)
	h.Set(light, true)
	h.ExpectValue(fan, false)
}

// A flow's condition with a mistake in it, which oikade check can't see, fails the run.
func TestAFlowsConditionWithAMistakeFailsTheRun(t *testing.T) {
	for name, step := range map[string]func(*oikade.FlowRun){
		"Is":      func(r *oikade.FlowRun) { r.Is(oikade.Yearly("1 Nov", "31 Feb")) },
		"WaitFor": func(r *oikade.FlowRun) { r.WaitFor(sensor.Within(25, 18), time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			home := oikade.NewHome(oikade.Options{})
			home.Flow("broken", func(r *oikade.FlowRun) {
				step(r)
				r.Set(fan.To(true))
			}).When(light)
			h := hometest.New(t, home, hometest.Options{Devices: devices()})
			h.Set(light, false)
			h.ExpectValue(fan, false)
			if h.Finished("broken") {
				t.Fatal("the run finished")
			}
		})
	}
}

// Each Expect that fails says what it wanted, and what the home has instead.
func TestAFailedExpectSaysWhatItFound(t *testing.T) {
	ft := &fatalT{TB: t}
	unread := oikade.Bool{Device: "unread", Capability: oikade.On}
	home := fanHome()
	home.Flow("airing", func(r *oikade.FlowRun) { r.Fail(errors.New("the pump is off for the winter")) }).
		When(light.Becomes(false))
	h := hometest.New(ft, home, hometest.Options{Devices: append(devices(),
		hometest.Device{Name: "unread", Capabilities: []string{on}})})
	for _, tc := range []struct {
		name   string
		expect func()
		want   string
	}{
		{"ExpectValue", func() { h.ExpectValue(fan, true) }, "fan on is false, want true"},
		{"ExpectValue with no value", func() { h.ExpectValue(unread, true) }, "unread on has no value, want true"},
		{"ExpectWrite", func() { h.ExpectWrite(light, false) }, "nothing wrote false to light on. Writes: []"},
		{
			"ExpectClaim", func() { h.ExpectClaim(fan, "Rule/fan on humid", "Normal") },
			"Rule/fan on humid holds no Normal claim on fan on. Claims: []",
		},
		{"ExpectFinished", func() { h.ExpectFinished("airing") }, "airing hasn't finished. How it ended otherwise: []"},
	} {
		expectFails(t, ft, tc.name, tc.expect, tc.want)
	}

	// Once there's something to show, the message shows it.
	h.Set(sensor, 80)
	h.Set(light, false)
	expectFails(t, ft, "ExpectWrite", func() { h.ExpectWrite(fan, false) },
		"nothing wrote false to fan on. Writes: [{2030-01-07 00:00:00 +0000 UTC fan on true}]")
	expectFails(t, ft, "ExpectClaim", func() { h.ExpectClaim(fan, "Rule/fan on humid", "Safety") },
		"Rule/fan on humid holds no Safety claim on fan on. "+
			"Claims: [{Source:Rule/fan on humid Level:Normal Value:true Winner:true Latched:false}]")
	expectFails(t, ft, "ExpectFinished", func() { h.ExpectFinished("airing") },
		"airing hasn't finished. How it ended otherwise: [failed]")
}

// A Report that isn't one device's report fails the test, from Fatal as well as Fatalf.
func TestAReportOfNoOneDeviceFails(t *testing.T) {
	ft := &fatalT{TB: t}
	h := hometest.New(ft, fanHome(), hometest.Options{Devices: devices()})
	expectFails(t, ft, "no values", func() { h.Report() }, "Report has no values")
	expectFails(t, ft, "two devices", func() { h.Report(sensor.To(80), fan.To(true)) },
		"Report has sensor and fan: a report is one device's")
	expectFails(t, ft, "a value twice", func() { h.Report(sensor.To(80), sensor.To(90)) },
		"Report has sensor's humidity twice")
}

// expectFails checks that expect fails with the message want.
func expectFails(t *testing.T, ft *fatalT, name string, expect func(), want string) {
	t.Helper()
	if !ft.fails(expect) {
		t.Errorf("%s passed", name)
	} else if ft.failed != want {
		t.Errorf("%s said %q, want %q", name, ft.failed, want)
	}
}
