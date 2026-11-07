package hometest_test

import (
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/framework"
	"github.com/harrymitchinson/oikade/framework/hometest"
	"github.com/harrymitchinson/oikade/internal/sun"
)

const (
	on       = string(framework.On)
	humidity = string(framework.Humidity)
)

// The devices' handles, as oikade types would write them.
var (
	sensor = framework.Handle[float64]{Device: "sensor", Capability: framework.Humidity}
	fan    = framework.Handle[bool]{Device: "fan", Capability: framework.On}
	pump   = framework.Handle[bool]{Device: "pump", Capability: framework.On}
	light  = framework.Handle[bool]{Device: "light", Capability: framework.On}
)

func devices() []hometest.Device {
	return []hometest.Device{
		{Name: "sensor", Capabilities: []string{humidity}, Values: map[string]any{humidity: 40.0}},
		{Name: "fan", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "pump", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "light", Capabilities: []string{on}, Values: map[string]any{on: true}},
	}
}

func fanHold() framework.Hold {
	return framework.Hold{
		Name: "fan", Set: fan.To(true), OffDelay: 10 * time.Minute,
		When: func(s framework.State) bool { return s.Get(sensor) > 70 },
	}
}

func TestAHoldFollowsADeviceAndTheClock(t *testing.T) {
	h := hometest.New(t, framework.Home{Holds: []framework.Hold{fanHold()}}, hometest.Options{Devices: devices()})
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

func bedtime() framework.Home {
	return framework.Home{Flows: []framework.Flow{{
		Name: "bedtime", At: []framework.Time{framework.Clock("22:30")},
		Run: func(r *framework.FlowRun) error {
			r.Sleep(30 * time.Minute)
			return r.Set(light.To(false))
		},
	}}}
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

func arbitrated() framework.Home {
	return framework.Home{
		Control: []framework.Control{{
			Default: pump.To(false), Resolve: framework.PreferOn,
			External: framework.ExternalOverride, OverrideHold: time.Hour,
		}},
		Holds: []framework.Hold{{
			Name: "pump", Set: pump.To(true), Priority: framework.Safety,
			Window: &framework.Window{Start: framework.Clock("01:00"), End: framework.Clock("02:00")},
		}},
	}
}

func TestClaimsAreArbitrated(t *testing.T) {
	h := hometest.New(t, arbitrated(), hometest.Options{Devices: devices()})
	h.Advance(90 * time.Minute)
	h.ExpectValue(pump, true)
	h.ExpectClaim(pump, "Rule/pump", "Safety")
	// A tap in an app loses to a Safety claim.
	h.Set(pump, false)
	h.ExpectClaim(pump, "Rule/pump", "Safety")
	h.Advance(time.Hour)
	h.ExpectValue(pump, false)
	if got := h.Claims(pump); len(got) != 0 {
		t.Errorf("claims %+v after the window", got)
	}
}

func TestABudgetRunsItsMinutes(t *testing.T) {
	home := framework.Home{
		Control: []framework.Control{{
			Default: pump.To(false), Resolve: framework.PreferOn,
			External: framework.ExternalOverride, OverrideHold: time.Hour,
		}},
		Holds: []framework.Hold{{
			Name: "pump", Set: pump.To(true),
			Budget: &framework.Budget{Minutes: func(framework.State) int { return 60 }},
		}},
	}
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

func TestAMomentWaits(t *testing.T) {
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{sensor},
		Do: []framework.Action{
			fan.To(true),
			framework.Wait(5 * time.Minute),
			fan.To(false),
		},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set(sensor, 41.0)
	h.ExpectValue(fan, true)
	h.Advance(6 * time.Minute)
	h.ExpectValue(fan, false)
	h.ExpectFinished("porch")
}

func TestAWaitEndsWhenItSaysInsideALongerMove(t *testing.T) {
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{sensor},
		Do: []framework.Action{
			framework.Wait(90 * time.Second),
			fan.To(true),
		},
	}}}
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
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{sensor},
		Do: []framework.Action{framework.Wait(5 * time.Minute)},
	}}}
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
	brightness := string(framework.Brightness)
	lamp := framework.Handle[bool]{Device: "floor-lamp", Capability: framework.On}
	ceiling := framework.Handle[bool]{Device: "ceiling-light", Capability: framework.On}
	fridge := framework.Handle[bool]{Device: "fridge-plug", Capability: framework.On}
	home := framework.Home{Moments: []framework.Moment{{
		Name: "living-room-off", At: []framework.Time{framework.Clock("23:00")},
		Do: []framework.Action{
			framework.InRoom("living-room", framework.On).Types(framework.OnOffLight, framework.DimmableLight).To(false),
		},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{
			Name: "floor-lamp", Room: "living-room", Capabilities: []string{on, brightness},
			Values: map[string]any{on: true, brightness: 80.0},
		},
		{
			Name: "ceiling-light", Room: "living-room", Capabilities: []string{on}, DeviceType: framework.OnOffLight,
			Values: map[string]any{on: true},
		},
		{Name: "fridge-plug", Room: "living-room", Capabilities: []string{on}, Values: map[string]any{on: true}},
	}})
	h.Advance(23*time.Hour + time.Minute)
	h.ExpectValue(lamp, false)
	h.ExpectValue(ceiling, false)
	h.ExpectValue(fridge, true)
}

func TestAClockTimeFiresAcrossADaylightSavingChange(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	home := framework.Home{Moments: []framework.Moment{{
		Name: "lights-out", At: []framework.Time{framework.Clock("23:30")},
		Do: []framework.Action{light.To(false)},
	}}}
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

func TestAWindowIsOpenOvernight(t *testing.T) {
	home := framework.Home{Holds: []framework.Hold{{
		Name: "night-pump", Set: pump.To(true),
		Window: &framework.Window{Start: framework.Clock("22:00"), End: framework.Clock("06:00")},
	}}}
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
	home := framework.Home{Moments: []framework.Moment{{
		Name: "lights-out", At: []framework.Time{framework.Clock("23:30"), framework.Clock("23:30")},
		Do: []framework.Action{light.To(false)},
	}}}
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
	cellar := func(name string) framework.Handle[float64] {
		return framework.Handle[float64]{Device: name, Capability: framework.Humidity}
	}
	dehumidifier := framework.Handle[bool]{Device: "dehumidifier", Capability: framework.On}
	home := framework.Home{Holds: []framework.Hold{{
		Name: "cellar-damp", Set: dehumidifier.To(true),
		When: func(s framework.State) bool {
			return slices.Max(s.Readings(framework.InRoom("cellar", framework.Humidity))) > 65
		},
	}}}
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

// greenwich is a home's Site, rounded to two decimals.
var greenwich = &framework.Site{Latitude: 51.48, Longitude: -0.01}

// sunset is when the sun sets at a site on a day, which internal/sun checks against published tables.
func sunset(t *testing.T, site *framework.Site, day time.Time) time.Time {
	t.Helper()
	at, ok := sun.Site{Latitude: site.Latitude, Longitude: site.Longitude}.
		Time(sun.Sunset, day.Year(), day.Month(), day.Day(), day.Location())
	if !ok {
		t.Fatalf("the sun doesn't set on %v", day)
	}
	return at
}

func TestAMomentFiresBeforeSunset(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	home := framework.Home{Site: greenwich, Moments: []framework.Moment{{
		Name: "porch-at-dusk", At: []framework.Time{framework.Sunset(-20 * time.Minute)},
		Do: []framework.Action{light.To(false)},
	}}}
	// Midwinter and midsummer, whose sunsets are more than five hours apart.
	for _, day := range []time.Time{
		time.Date(2030, 1, 7, 0, 0, 0, 0, london), time.Date(2030, 6, 21, 0, 0, 0, 0, london),
	} {
		h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: day})
		h.Advance(24 * time.Hour)
		want := sunset(t, greenwich, day).Add(-20 * time.Minute)
		if got := h.Writes(); len(got) != 1 || !got[0].Time.Equal(want) {
			t.Errorf("on %s, writes %+v, want one at %v", day.Format(time.DateOnly), got, want)
		}
	}
}

func TestAWindowOpensAtSunset(t *testing.T) {
	home := framework.Home{Site: greenwich, Holds: []framework.Hold{{
		Name: "garden-pump", Set: pump.To(true),
		Window: &framework.Window{Start: framework.Sunset(0), End: framework.Clock("23:30")},
	}}}
	start := time.Date(2030, 3, 20, 0, 0, 0, 0, time.UTC)
	h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: start})
	opens := sunset(t, greenwich, start)
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
	svalbard := &framework.Site{Latitude: 78.22, Longitude: 15.65}
	home := framework.Home{
		Site: svalbard,
		Moments: []framework.Moment{{
			Name: "dusk", At: []framework.Time{framework.Sunset(0)}, Do: []framework.Action{light.To(false)},
		}},
		Holds: []framework.Hold{{
			Name: "evening-pump", Set: pump.To(true),
			Window: &framework.Window{Start: framework.Sunset(0), End: framework.Clock("23:30")},
		}},
	}
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

// A hold whose When reads a window opens within a minute of it, with no device changing.
func TestAHoldWithinAWindowOpensWithinAMinute(t *testing.T) {
	home := framework.Home{Holds: []framework.Hold{{
		Name: "evening", Set: light.To(false),
		When: func(s framework.State) bool {
			return s.Within(framework.Window{Start: framework.Clock("18:00"), End: framework.Clock("07:00")})
		},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)})
	h.Advance(6*time.Hour - time.Second)
	h.ExpectValue(light, true)
	h.Advance(time.Minute + time.Second)
	h.ExpectValue(light, false)
}

// Within in a When and a hold's own Window agree, minute by minute, across the night the clocks go
// back, when 01:00 to 02:00 happens twice.
func TestWithinAgreesWithAHoldsWindowAcrossDaylightSaving(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	w := framework.Window{Start: framework.Clock("01:30"), End: framework.Clock("02:15")}
	// Clocks go back at 02:00 BST on Sunday 27 October 2030.
	agree(t, w, nil, time.Date(2030, 10, 26, 23, 0, 0, 0, london), 5*time.Hour)
}

// Within with a sunset edge agrees with a hold's Window with the same edge.
func TestWithinAgreesWithAHoldsWindowAtSunset(t *testing.T) {
	w := framework.Window{Start: framework.Sunset(0), End: framework.Clock("23:30")}
	start := time.Date(2030, 3, 20, 12, 0, 0, 0, time.UTC)
	agree(t, w, greenwich, start, 12*time.Hour)
}

// agree runs a hold with w as its Window beside one whose When is s.Within(w), and checks they
// agree after every minute.
func agree(t *testing.T, w framework.Window, site *framework.Site, start time.Time, d time.Duration) {
	t.Helper()
	home := framework.Home{Site: site, Holds: []framework.Hold{
		{Name: "by-window", Set: fan.To(true), Window: &w},
		{Name: "by-within", Set: pump.To(true), When: func(s framework.State) bool { return s.Within(w) }},
	}}
	h := hometest.New(t, home, hometest.Options{Devices: devices(), Start: start})
	opened := 0
	for range int(d / time.Minute) {
		h.Advance(time.Minute)
		byWindow, _ := h.Value(fan)
		byWithin, _ := h.Value(pump)
		if byWindow != byWithin {
			t.Fatalf("at %s the window says %v and Within says %v", h.Now(), byWindow, byWithin)
		}
		if byWindow {
			opened++
		}
	}
	if opened == 0 {
		t.Fatal("the window never opened")
	}
}
