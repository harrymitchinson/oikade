package hometest_test

import (
	"fmt"
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

// designHome is the design's porch light, pool pump and garage heater.
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
		Reading:   poolWater.AverageOver(24 * time.Hour),
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
		Budget("summer-extra", true, extra, summer).
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

// pumpRun is how many minutes the pool pump ran from since to until.
func pumpRun(h *hometest.Home, since, until time.Time) int {
	ran := 0
	var on time.Time
	for _, w := range h.Writes() {
		if w.Device != "pool-pump" || w.Time.Before(since) {
			continue
		}
		if w.Value == true {
			on = w.Time
		} else if !on.IsZero() {
			ran += int(w.Time.Sub(on).Minutes())
			on = time.Time{}
		}
	}
	if !on.IsZero() {
		ran += int(until.Sub(on).Minutes())
	}
	return ran
}

// The summer budget is planned from the water's average over the last day, which a restart keeps:
// a day at 30 with its last two hours at 20 averages above 29, for four hours' run, where the water
// as it is, or an average started again at the restart, would give none.
func TestThePoolPumpsBudgetReadsTheAverageAcrossARestart(t *testing.T) {
	start := time.Date(2030, time.June, 30, 0, 0, 0, 0, time.UTC)
	h := designTest(t, start, 30)
	h.Advance(22 * time.Hour)
	h.Set(poolWater, 20.0)
	h.Advance(time.Hour)
	h.Restart()
	// A sensor reports now and then, so its average is fresh at midnight.
	h.Advance(50 * time.Minute)
	h.Set(poolWater, 20.0)
	h.Advance(10 * time.Minute)
	midnight := start.Add(24 * time.Hour)
	h.Advance(10 * time.Hour)
	if ran := pumpRun(h, midnight, midnight.Add(10*time.Hour)); ran != 240 {
		t.Errorf("ran %d minutes before swimming hours, want 240: %v", ran, h.Writes())
	}
}

// Outside summer the budget doesn't run, however warm the water.
func TestThePoolPumpsBudgetIsOnlyForSummer(t *testing.T) {
	start := time.Date(2030, time.November, 10, 0, 0, 0, 0, time.UTC)
	h := designTest(t, start, 30)
	h.Advance(10 * time.Hour)
	if ran := pumpRun(h, start, start.Add(10*time.Hour)); ran != 0 {
		t.Errorf("ran %d minutes in November: %v", ran, h.Writes())
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

// The flows OIK-298's design is written around. A radiator stands in for the design's room of them,
// and the mains valve's On is whether it's open.
var (
	hallLight    = oikade.Bool{Device: "hall-light", Capability: oikade.On}
	frontDoor    = oikade.Bool{Device: "front-door", Capability: oikade.Open}
	hallRadiator = oikade.Number{Device: "hall-radiator", Capability: oikade.Setpoint}

	mainsValve   = oikade.Bool{Device: "mains-valve", Capability: oikade.On}
	sinkHumidity = oikade.Number{Device: "sink-sensor", Capability: oikade.Humidity}
)

func flowsHome() *oikade.Home {
	home := oikade.NewHome(oikade.Options{Location: oikade.NewLocation(51.48, 0)})

	home.Flow("front-door-arrival", func(r *oikade.FlowRun) {
		if r.Is(oikade.Daily(oikade.Sunset(0), oikade.Sunrise(0))) {
			r.Set(hallLight.To(true).For(3 * time.Minute))
		}
		if !r.WaitFor(oikade.Not(frontDoor), 10*time.Minute) {
			r.Set(hallRadiator.To(7).For(time.Hour))
		}
	}).When(frontDoor.Becomes(true))
	home.Decide(hallLight).Flows().Default(false)
	home.Decide(hallRadiator).Flows().Default(20)

	leak := home.Flow("leak", func(r *oikade.FlowRun) {
		r.Set(mainsValve.To(false).UntilReleased())
	}).When(sinkHumidity).If(sinkHumidity.Above(90))
	home.Flow("night-off", setting(mainsValve.To(false).For(8*time.Hour))).At(oikade.Clock("23:00"))
	home.Decide(mainsValve).
		Flows(leak). // this flow beats a switch
		OverrideFor(time.Hour).
		Flows(). // every other flow
		Default(true)
	return home
}

func flowsTest(t *testing.T, start time.Time) *hometest.Home {
	t.Helper()
	open, setpoint, humidity := string(oikade.Open), string(oikade.Setpoint), string(oikade.Humidity)
	return hometest.New(t, flowsHome(), hometest.Options{Start: start, Devices: []hometest.Device{
		{Name: "hall-light", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "front-door", Capabilities: []string{open}, Values: map[string]any{open: false}},
		{Name: "hall-radiator", Capabilities: []string{setpoint}, Values: map[string]any{setpoint: 20.0}},
		{Name: "mains-valve", Capabilities: []string{on}, Values: map[string]any{on: true}},
		{Name: "sink-sensor", Capabilities: []string{humidity}, Values: map[string]any{humidity: 50.0}},
	}})
}

// After dark, the front door lights the hall for three minutes, and left open, it turns the
// radiator down for an hour, across a restart.
func TestTheFrontDoorLightsTheHallAndTurnsTheRadiatorDown(t *testing.T) {
	h := flowsTest(t, time.Date(2030, time.January, 7, 18, 0, 0, 0, time.UTC))
	h.Set(frontDoor, true)
	h.ExpectValue(hallLight, true)
	h.ExpectClaim(hallLight, "Rule/front-door-arrival", "Normal")
	h.Advance(4 * time.Minute)
	h.ExpectValue(hallLight, false)
	h.Restart()
	h.Advance(5 * time.Minute)
	h.ExpectValue(hallRadiator, 20)
	h.Advance(2 * time.Minute)
	h.ExpectValue(hallRadiator, 7)
	h.Advance(time.Hour)
	h.ExpectValue(hallRadiator, 20)
	h.ExpectFinished("front-door-arrival")
}

// In daylight the hall stays dark, and a door closed in time leaves the radiator alone.
func TestTheFrontDoorClosedInDaylightDoesNothing(t *testing.T) {
	h := flowsTest(t, time.Date(2030, time.January, 7, 12, 0, 0, 0, time.UTC))
	h.Set(frontDoor, true)
	h.Advance(time.Minute)
	h.Set(frontDoor, false)
	h.Advance(time.Hour)
	h.ExpectValue(hallLight, false)
	h.ExpectValue(hallRadiator, 20)
	h.ExpectFinished("front-door-arrival")
}

// The leak flow ranks above the valve's override line, so a tap on its switch is written back, and
// every other flow ranks below it, so a tap beats it for an hour.
func TestTheLeakFlowBeatsASwitchAndTheOthersDont(t *testing.T) {
	h := flowsTest(t, time.Date(2030, time.January, 7, 22, 0, 0, 0, time.UTC))
	h.Advance(61 * time.Minute)
	h.ExpectValue(mainsValve, false)
	h.ExpectClaim(mainsValve, "Rule/night-off", "Normal")
	h.Set(mainsValve, true)
	h.Advance(59 * time.Minute)
	h.ExpectValue(mainsValve, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(mainsValve, false)

	h.Set(sinkHumidity, 95.0)
	h.ExpectClaim(mainsValve, "Rule/leak", "Safety")
	h.Advance(time.Minute)
	h.Set(mainsValve, true)
	h.ExpectValue(mainsValve, false)
	h.Restart()
	// Night-off's claim ends at 07:00, and the leak's lasts until it's released.
	h.Advance(12 * time.Hour)
	h.ExpectValue(mainsValve, false)
	h.ExpectClaim(mainsValve, "Rule/leak", "Safety")
}

// The design's weather compensation. A radiator stands in for its group of them, which comes with
// groups.
var (
	studyRadiator = oikade.Number{Device: "study-radiator", Capability: oikade.Setpoint}
	studyWindow   = oikade.Bool{Device: "study-window", Capability: oikade.Open}
	outdoorTemp   = oikade.Number{Device: "outdoor-sensor", Capability: oikade.Temperature}
)

// compensate is the setpoint for a temperature outside: warmer inside the colder it is.
func compensate(outside float64) float64 { return 21 - outside/4 }

func weatherTest(t *testing.T, outside float64) *hometest.Home {
	t.Helper()
	home := oikade.NewHome(oikade.Options{})
	home.Decide(studyRadiator).
		Case("windows-open", 7, studyWindow).Delay(2*time.Minute).
		OverrideFor(2*time.Hour).
		Flows().
		Follow("weather", outdoorTemp, compensate).
		Default(18)
	open, setpoint, temperature := string(oikade.Open), string(oikade.Setpoint), string(oikade.Temperature)
	values := map[string]any{}
	if outside != 0 {
		values[temperature] = outside
	}
	return hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{Name: "study-radiator", Capabilities: []string{setpoint}, Values: map[string]any{setpoint: 15.0}},
		{Name: "study-window", Capabilities: []string{open}, Values: map[string]any{open: false}},
		{Name: "outdoor-sensor", Capabilities: []string{temperature}, Values: values},
	}})
}

// radiatorWrites are the setpoints written to the study's radiator.
func radiatorWrites(h *hometest.Home) []any {
	var out []any
	for _, w := range h.Writes() {
		if w.Device == "study-radiator" {
			out = append(out, w.Value)
		}
	}
	return out
}

// The radiator follows the weather in half degrees, writing only when that changes, and a restart
// doesn't write it again.
func TestTheRadiatorFollowsTheWeatherAcrossARestart(t *testing.T) {
	h := weatherTest(t, 8)
	h.ExpectValue(studyRadiator, 19)
	h.ExpectClaim(studyRadiator, "Rule/study-radiator setpoint weather", "Normal")
	h.Set(outdoorTemp, 8.5) // 18.875, which rounds to 19
	h.Restart()
	h.Set(outdoorTemp, 8.6)
	h.Advance(time.Hour)
	h.Set(outdoorTemp, 10.0)
	h.ExpectValue(studyRadiator, 18.5)
	// The Default comes first on startup, before any line has claimed, as with a case.
	if got := fmt.Sprint(radiatorWrites(h)); got != "[18 19 18.5]" {
		t.Errorf("wrote %s, want [18 19 18.5]", got)
	}

	// An open window beats the weather, and once it's shut the weather's setpoint is back.
	h.Set(studyWindow, true)
	h.Advance(3 * time.Minute)
	h.ExpectValue(studyRadiator, 7)
	h.Set(studyWindow, false)
	h.ExpectValue(studyRadiator, 18.5)
}

// Until the sensor outside reports, the radiator is at its Default.
func TestTheRadiatorWaitsForTheWeather(t *testing.T) {
	h := weatherTest(t, 0)
	h.ExpectValue(studyRadiator, 18)
	h.Set(outdoorTemp, 12.0)
	h.ExpectValue(studyRadiator, 18)
	h.Set(outdoorTemp, 4.0)
	h.ExpectValue(studyRadiator, 20)
}
