package oikadetest_test

import (
	"testing"
	"time"

	"oikade.dev"
	"oikade.dev/oikadetest"
)

// wednesday is the midnight that ends Tuesday 8 January 2030, the day after the tests' first.
var wednesday = time.Date(2030, time.January, 9, 0, 0, 0, 0, time.UTC)

// seenTodayOrYesterday keeps the towel rail warm while anyone was seen today or yesterday.
func seenTodayOrYesterday() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	presence := home.Group("presence", HallSensor, BathroomSensor)
	anyoneHome := home.Signal("anyone-home", presence.Occupied.Any()).Linger(24 * time.Hour).LingerUntil("00:00")
	home.Decide(TowelRail).Case("away", 7, oikade.Not(anyoneHome)).Default(19)
	return home
}

func TestASignalIsTrueThroughTheDayAfterTheLastSighting(t *testing.T) {
	for name, seen := range map[string]time.Duration{
		"seen in the morning": 8 * time.Hour,
		"seen late":           23*time.Hour + 30*time.Minute,
	} {
		t.Run(name, func(t *testing.T) {
			h := oikadetest.New(t, seenTodayOrYesterday(), oikadetest.Options{Devices: awayDevices(true, false)})
			h.Advance(seen)
			h.Set(HallSensor.Occupied, false)
			h.Advance(wednesday.Sub(h.Now()) - time.Minute)
			h.ExpectValue(TowelRail, 19)
			h.Advance(time.Minute)
			h.ExpectValue(TowelRail, 7)
		})
	}
}

// The host keeps when a LingerUntil ends, as it does a Linger's.
func TestASignalsLingerUntilSurvivesARestart(t *testing.T) {
	h := oikadetest.New(t, seenTodayOrYesterday(), oikadetest.Options{Devices: awayDevices(true, false)})
	h.Advance(8 * time.Hour)
	h.Set(HallSensor.Occupied, false)
	h.Advance(30 * time.Hour)
	h.Restart()
	h.ExpectValue(TowelRail, 19)
	h.Advance(wednesday.Sub(h.Now()))
	h.ExpectValue(TowelRail, 7)
}

func TestASignalSeenTodayIsFalseAtMidnight(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	seenToday := home.Signal("seen-today", HallSensor.Occupied).LingerUntil("00:00")
	home.Decide(TowelRail).Case("away", 7, oikade.Not(seenToday)).Default(19)
	h := oikadetest.New(t, home, oikadetest.Options{Devices: awayDevices(true, nil)})
	h.Advance(15 * time.Hour)
	h.Set(HallSensor.Occupied, false)
	h.Advance(9*time.Hour - time.Minute)
	h.ExpectValue(TowelRail, 19)
	h.Advance(time.Minute)
	h.ExpectValue(TowelRail, 7)
}

// terraceHome keeps the terrace light on until 23:00 once someone's been out on the terrace in the
// evening.
func terraceHome(t *testing.T) (*oikadetest.Home, oikade.Bool, oikade.Bool) {
	t.Helper()
	sensor := oikade.Bool{Device: "terrace-sensor", Capability: oikade.Occupied}
	light := oikade.Bool{Device: "terrace-light", Capability: oikade.On}
	home := oikade.NewHome(oikade.Options{})
	home.Decide(light).
		Case("out", true, sensor, oikade.Daily("20:00", "23:00")).
		LingerUntil("23:00").
		Default(false)
	occupied, on := string(oikade.Occupied), string(oikade.On)
	h := oikadetest.New(t, home, oikadetest.Options{
		Start: time.Date(2030, time.January, 7, 21, 0, 0, 0, time.UTC),
		Devices: []oikadetest.Device{
			{Name: "terrace-sensor", Capabilities: []string{occupied}, Values: map[string]any{occupied: false}},
			{Name: "terrace-light", Capabilities: []string{on}, Values: map[string]any{on: false}},
		},
	})
	return h, sensor, light
}

func TestACaseLingersUntilATimeOfDay(t *testing.T) {
	h, sensor, light := terraceHome(t)
	h.Set(sensor, true)
	h.ExpectValue(light, true)
	h.Advance(5 * time.Minute)
	h.Set(sensor, false)
	h.Advance(time.Hour + 54*time.Minute)
	h.ExpectValue(light, true)
	h.Advance(time.Minute)
	h.ExpectValue(light, false)
}

// A case whose Daily ends at its LingerUntil ends there, not a day later.
func TestACaseWhoseWindowEndsAtItsLingerUntilEndsThere(t *testing.T) {
	h, sensor, light := terraceHome(t)
	h.Set(sensor, true)
	h.Advance(time.Hour + 59*time.Minute)
	h.ExpectValue(light, true)
	h.Advance(time.Minute)
	h.ExpectValue(light, false)
	h.Set(sensor, false)
	h.Advance(20 * time.Hour)
	h.ExpectValue(light, false)
}
