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
	h.ExpectClaim(porchLight, "case/porch-light/on/motion", "Normal")
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
	h.ExpectClaim(poolPump, "case/pool-pump/on/swimming-hours", "Normal")

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
	h.ExpectClaim(poolPump, "case/pool-pump/on/frost", "Safety")
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
	h.ExpectClaim(poolPump, "case/pool-pump/on/shoulder", "Normal")
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
	h.ExpectClaim(garageHeater, "case/garage-heater/on/overheat", "Safety")
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
	h.ExpectClaim(hallLight, "flow/front-door-arrival", "Normal")
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
	h.ExpectClaim(mainsValve, "flow/night-off", "Normal")
	h.Set(mainsValve, true)
	h.Advance(59 * time.Minute)
	h.ExpectValue(mainsValve, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(mainsValve, false)

	h.Set(sinkHumidity, 95.0)
	h.ExpectClaim(mainsValve, "flow/leak", "Safety")
	h.Advance(time.Minute)
	h.Set(mainsValve, true)
	h.ExpectValue(mainsValve, false)
	h.Restart()
	// Night-off's claim ends at 07:00, and the leak's lasts until it's released.
	h.Advance(12 * time.Hour)
	h.ExpectValue(mainsValve, false)
	h.ExpectClaim(mainsValve, "flow/leak", "Safety")
}

// The design's radiators, declared as oikade types writes them.
var (
	BedroomRadiator = radiator("bedroom-radiator")
	StudyRadiator   = radiator("study-radiator")
	KitchenRadiator = radiator("kitchen-radiator")
	BedroomWindow   = window("bedroom-window")
	StudyWindow     = window("study-window")
	KitchenWindow   = window("kitchen-window")
	StudySensor     = struct {
		oikade.Device
		Occupied oikade.Bool
	}{oikade.Device{Name: "study-sensor"}, oikade.Bool{Device: "study-sensor", Capability: oikade.Occupied}}
	OutdoorSensor = struct {
		oikade.Device
		Temperature oikade.Number
	}{oikade.Device{Name: "outdoor-sensor"}, oikade.Number{Device: "outdoor-sensor", Capability: oikade.Temperature}}
)

type radiatorDevice struct {
	oikade.Device
	Setpoint oikade.Number
}

func radiator(name string) radiatorDevice {
	return radiatorDevice{oikade.Device{Name: name}, oikade.Number{Device: name, Capability: oikade.Setpoint}}
}

type windowDevice struct {
	oikade.Device
	Open oikade.Bool
}

func window(name string) windowDevice {
	return windowDevice{oikade.Device{Name: name}, oikade.Bool{Device: name, Capability: oikade.Open}}
}

// compensate is the setpoint for a temperature outside: warmer inside the colder it is.
func compensate(outside float64) float64 { return 21 - outside/4 }

// radiatorsHome is the design's: every radiator follows the weather, and goes down while a window
// is open, and the study's is warmer while someone works there.
func radiatorsHome() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	radiators := home.Group("radiators", BedroomRadiator, StudyRadiator, KitchenRadiator)
	windows := home.Group("windows", BedroomWindow, StudyWindow, KitchenWindow)
	home.Decide(radiators.Setpoint).
		Case("windows-open", 7, windows.Open.Any()).Delay(2*time.Minute).
		OverrideFor(2*time.Hour).
		Flows().
		Follow("weather", OutdoorSensor.Temperature, compensate).
		Default(18)
	home.Decide(StudyRadiator.Setpoint).
		Case("working", 21, StudySensor.Occupied, oikade.Daily("09:00", "17:30")).
		Then(radiators.Setpoint)
	return home
}

// radiatorsTest starts on a Monday at 06:00, with outside the temperature outside, or none reported
// for 0.
func radiatorsTest(t *testing.T, outside float64) *hometest.Home {
	t.Helper()
	open, setpoint, temperature := string(oikade.Open), string(oikade.Setpoint), string(oikade.Temperature)
	occupied := string(oikade.Occupied)
	values := map[string]any{}
	if outside != 0 {
		values[temperature] = outside
	}
	devices := make([]hometest.Device, 0, 8)
	devices = append(devices, []hometest.Device{
		{Name: "study-sensor", Capabilities: []string{occupied}, Values: map[string]any{occupied: false}},
		{Name: "outdoor-sensor", Capabilities: []string{temperature}, Values: values},
	}...)
	for _, room := range []string{"bedroom", "study", "kitchen"} {
		devices = append(devices,
			hometest.Device{Name: room + "-radiator", Capabilities: []string{setpoint}, Values: map[string]any{setpoint: 15.0}},
			hometest.Device{Name: room + "-window", Capabilities: []string{open}, Values: map[string]any{open: false}},
		)
	}
	return hometest.New(t, radiatorsHome(), hometest.Options{
		Start: time.Date(2030, time.January, 7, 6, 0, 0, 0, time.UTC), Devices: devices,
	})
}

// radiatorWrites are the setpoints written to a radiator.
func radiatorWrites(h *hometest.Home, device string) []any {
	var out []any
	for _, w := range h.Writes() {
		if w.Device == device {
			out = append(out, w.Value)
		}
	}
	return out
}

// Every radiator follows the weather in half degrees, writing only when that changes, and a restart
// doesn't write it again.
func TestTheRadiatorsFollowTheWeatherAcrossARestart(t *testing.T) {
	h := radiatorsTest(t, 8)
	for _, r := range []radiatorDevice{BedroomRadiator, StudyRadiator, KitchenRadiator} {
		h.ExpectValue(r.Setpoint, 19)
		h.ExpectClaim(r.Setpoint, "case/radiators/setpoint/weather", "Normal")
	}
	h.Set(OutdoorSensor.Temperature, 8.5) // 18.875, which rounds to 19
	h.Restart()
	h.Set(OutdoorSensor.Temperature, 8.6)
	h.Advance(time.Hour)
	h.Set(OutdoorSensor.Temperature, 10.0)
	h.ExpectValue(KitchenRadiator.Setpoint, 18.5)
	// The Default comes first on startup, before any line has claimed, as with a case.
	if got := fmt.Sprint(radiatorWrites(h, "kitchen-radiator")); got != "[18 19 18.5]" {
		t.Errorf("wrote %s, want [18 19 18.5]", got)
	}
}

// One open window turns every radiator down, after two minutes, and once it's shut the weather's
// setpoint is back.
func TestAnOpenWindowTurnsEveryRadiatorDown(t *testing.T) {
	h := radiatorsTest(t, 8)
	h.Set(KitchenWindow.Open, true)
	h.Advance(time.Minute)
	h.ExpectValue(BedroomRadiator.Setpoint, 19)
	h.Advance(2 * time.Minute)
	for _, r := range []radiatorDevice{BedroomRadiator, StudyRadiator, KitchenRadiator} {
		h.ExpectValue(r.Setpoint, 7)
		h.ExpectClaim(r.Setpoint, "case/radiators/setpoint/windows-open", "Safety")
	}
	h.Set(KitchenWindow.Open, false)
	h.ExpectValue(BedroomRadiator.Setpoint, 19)
	h.ExpectValue(StudyRadiator.Setpoint, 19)
}

// The study's own case ranks above the group's, so someone working there keeps it warm, window or
// not, while the others follow the group.
func TestTheStudyIsWarmWhileSomeoneWorksThere(t *testing.T) {
	h := radiatorsTest(t, 8)
	h.Advance(4 * time.Hour) // 10:00
	h.Set(StudySensor.Occupied, true)
	h.ExpectValue(StudyRadiator.Setpoint, 21)
	h.ExpectClaim(StudyRadiator.Setpoint, "case/study-radiator/setpoint/working", "Safety")
	h.ExpectValue(BedroomRadiator.Setpoint, 19)

	h.Set(StudyWindow.Open, true)
	h.Advance(3 * time.Minute)
	h.ExpectValue(StudyRadiator.Setpoint, 21)
	h.ExpectValue(BedroomRadiator.Setpoint, 7)
	h.Set(StudyWindow.Open, false)

	h.Advance(8 * time.Hour) // 18:03, after work
	h.ExpectValue(StudyRadiator.Setpoint, 19)
}

// A tap on one radiator wins on that radiator for two hours, and the others keep following.
func TestATapOnOneRadiatorWinsOnlyThere(t *testing.T) {
	h := radiatorsTest(t, 8)
	h.Advance(time.Minute) // past the settle of the engine's write
	h.Set(BedroomRadiator.Setpoint, 23.0)
	h.ExpectClaim(BedroomRadiator.Setpoint, "device", "Override")
	h.Set(OutdoorSensor.Temperature, 12.0)
	h.ExpectValue(BedroomRadiator.Setpoint, 23)
	h.ExpectValue(KitchenRadiator.Setpoint, 18)
	h.Advance(2*time.Hour + time.Minute)
	h.ExpectValue(BedroomRadiator.Setpoint, 18)
}

// Until the sensor outside reports, the radiators are at their Default.
func TestTheRadiatorsWaitForTheWeather(t *testing.T) {
	h := radiatorsTest(t, 0)
	h.ExpectValue(BedroomRadiator.Setpoint, 18)
	h.Set(OutdoorSensor.Temperature, 4.0)
	h.ExpectValue(BedroomRadiator.Setpoint, 20)
	h.ExpectValue(StudyRadiator.Setpoint, 20)
}
