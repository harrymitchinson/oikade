package hometest_test

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade"
	"github.com/harrymitchinson/oikade/hometest"
)

// The decisions OIK-298's design is written around, as a home writes them.

var (
	porchLight  = oikade.Bool{Device: "porch-light", Capability: oikade.On}
	porchSensor = oikade.Bool{Device: "porch-sensor", Capability: oikade.Occupied}

	poolPump  = oikade.Bool{Device: "pool-pump", Capability: oikade.On}
	poolWater = oikade.Number{Device: "pool-water", Capability: oikade.Temperature}

	garageHeater = oikade.Bool{Device: "garage-heater", Capability: oikade.On}
	garageTemp   = oikade.Number{Device: "garage-sensor", Capability: oikade.Temperature}
	garageDoor   = oikade.Bool{Device: "garage-door", Capability: oikade.Open}
)

func designDevices(water float64) []hometest.Device {
	temperature, open, occupied := string(oikade.Temperature), string(oikade.Open), string(oikade.Occupied)
	return []hometest.Device{
		{Name: "porch-light", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "porch-sensor", Capabilities: []string{occupied}, Values: map[string]any{occupied: false}},
		{Name: "pool-pump", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "pool-water", Capabilities: []string{temperature}, Values: map[string]any{temperature: water}},
		{Name: "garage-heater", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "garage-sensor", Capabilities: []string{temperature}, Values: map[string]any{temperature: 20.0}},
		{Name: "garage-door", Capabilities: []string{open}, Values: map[string]any{open: false}},
	}
}

// designHome is the design's porch light, pool pump and garage heater. The pump's budget has no
// condition and reads the water as it is, rather than its average: those come in a later step.
func designHome() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})

	home.Decide(porchLight).
		Case("motion", true, porchSensor).Linger(5 * time.Minute).
		Default(false)

	summer := oikade.Yearly("1 Jun", "15 Sep")
	shoulder := oikade.Any(oikade.Yearly("1 Apr", "31 May"), oikade.Yearly("16 Sep", "31 Oct"))
	swimHours := oikade.Daily("10:00", "18:00")
	cheapHours := oikade.Daily("00:30", "05:30")
	extra := oikade.NewBandedBudget(oikade.Bands{
		Reading:   poolWater,
		Ranges:    []oikade.Band{{Below: 22, Minutes: 0}, {Below: 26, Minutes: 60}, {Below: 29, Minutes: 120}},
		Otherwise: 240,
	}).Prefer(cheapHours, 3).Forbid(swimHours).
		// The design's budget plans in half hours, which its MinimumOff isn't a multiple of.
		SlotMinutes(15)
	home.Decide(poolPump).
		Limits(oikade.Limits{MinimumOn: 30 * time.Minute, MinimumOff: 15 * time.Minute}).
		Case("frost", true, poolWater.Below(3)).Linger(30*time.Minute).
		OverrideFor(2*time.Hour).
		Case("swimming-hours", true, summer, swimHours).
		Budget("summer-extra", true, extra).
		Case("shoulder", true, shoulder, cheapHours).
		Default(false)

	home.Decide(garageHeater).
		NoOverride().
		Case("overheat", false, garageTemp.Above(40)).LatchUntil(garageDoor).
		Default(true)
	return home
}

func designTest(t *testing.T, start time.Time, water float64) *hometest.Home {
	t.Helper()
	return hometest.New(t, designHome(), hometest.Options{Start: start, Devices: designDevices(water)})
}

func TestThePorchLightLingersAfterMotion(t *testing.T) {
	h := designTest(t, time.Time{}, 20)
	h.ExpectValue(porchLight, false)
	h.Set(porchSensor, true)
	h.ExpectValue(porchLight, true)
	h.ExpectClaim(porchLight, "Rule/porch-light on motion", "Normal")
	h.Set(porchSensor, false)
	h.Restart()
	h.Advance(4 * time.Minute)
	h.ExpectValue(porchLight, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(porchLight, false)
}

// In summer the pump runs in swimming hours, and a tap on its switch wins for two hours.
func TestThePoolPumpRunsInSwimmingHoursAndASwitchWinsForAWhile(t *testing.T) {
	// Cold water, so the summer budget has no minutes to run.
	h := designTest(t, time.Date(2030, time.July, 1, 6, 0, 0, 0, time.UTC), 20)
	h.ExpectValue(poolPump, false)
	h.Advance(4*time.Hour + time.Minute)
	h.ExpectValue(poolPump, true)
	h.ExpectClaim(poolPump, "Rule/pool-pump on swimming-hours", "Normal")

	h.Advance(time.Hour)
	h.Set(poolPump, false)
	h.Restart()
	h.Advance(time.Hour)
	h.ExpectValue(poolPump, false)
	h.Advance(time.Hour + time.Minute)
	h.ExpectValue(poolPump, true)

	h.Advance(5 * time.Hour)
	h.ExpectValue(poolPump, false)
}

// Near freezing, the pump runs whatever its switch says, and for half an hour after.
func TestThePoolPumpsFrostRunBeatsItsSwitch(t *testing.T) {
	h := designTest(t, time.Date(2030, time.January, 7, 12, 0, 0, 0, time.UTC), 5)
	h.ExpectValue(poolPump, false)
	h.Set(poolWater, 2.0)
	h.ExpectValue(poolPump, true)
	h.ExpectClaim(poolPump, "Rule/pool-pump on frost", "Safety")
	// A tap on its switch a minute later turns it off, and its MinimumOff, which beats every line,
	// rests it for a quarter of an hour before frost turns it back on.
	h.Advance(time.Minute)
	h.Set(poolPump, false)
	h.Advance(14 * time.Minute)
	h.ExpectValue(poolPump, false)
	h.Advance(2 * time.Minute)
	h.ExpectValue(poolPump, true)

	h.Set(poolWater, 4.0)
	h.Restart()
	h.Advance(29 * time.Minute)
	h.ExpectValue(poolPump, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(poolPump, false)
}

// In spring the pump runs in the cheap hours.
func TestThePoolPumpRunsInTheCheapHoursInSpring(t *testing.T) {
	h := designTest(t, time.Date(2030, time.April, 10, 0, 0, 0, 0, time.UTC), 15)
	h.Advance(29 * time.Minute)
	h.ExpectValue(poolPump, false)
	h.Advance(2 * time.Minute)
	h.ExpectValue(poolPump, true)
	h.ExpectClaim(poolPump, "Rule/pool-pump on shoulder", "Normal")
	h.Advance(5 * time.Hour)
	h.ExpectValue(poolPump, false)
}

// In summer, warm water gives the pump extra minutes, outside swimming hours.
func TestThePoolPumpRunsItsSummerBudget(t *testing.T) {
	h := designTest(t, time.Date(2030, time.July, 1, 0, 0, 0, 0, time.UTC), 25)
	h.Advance(10 * time.Hour)
	ran := 0
	var since time.Time
	for _, w := range h.Writes() {
		if w.Device != "pool-pump" {
			continue
		}
		if w.Value == true {
			since = w.Time
		} else if !since.IsZero() {
			ran += int(w.Time.Sub(since).Minutes())
			since = time.Time{}
		}
	}
	if ran != 60 {
		t.Errorf("ran %d minutes before swimming hours, want 60: %v", ran, h.Writes())
	}
}

// An overheated garage turns its heater off until someone opens the door: no reset rule, and a tap
// in an app is written back, across a restart.
func TestTheGarageHeaterLatchesOffUntilTheDoorOpens(t *testing.T) {
	h := designTest(t, time.Time{}, 20)
	h.ExpectValue(garageHeater, true)
	h.Set(garageTemp, 45.0)
	h.ExpectValue(garageHeater, false)
	h.ExpectClaim(garageHeater, "Rule/garage-heater on overheat", "Safety")
	h.Set(garageTemp, 20.0)
	h.Restart()
	h.Advance(time.Hour)
	h.ExpectValue(garageHeater, false)
	h.Set(garageHeater, true)
	h.ExpectValue(garageHeater, false)

	h.Set(garageDoor, true)
	h.ExpectValue(garageHeater, true)
	h.Set(garageDoor, false)
	h.ExpectValue(garageHeater, true)
}
