package oikadetest_test

import (
	"testing"
	"time"

	"oikade.dev"
	"oikade.dev/oikadetest"
)

// Presence sensors, a radiator and a shutter, as oikade types writes them.
var (
	HallSensor     = presenceSensor("hall-sensor")
	BathroomSensor = presenceSensor("bathroom-sensor")
	TowelRail      = oikade.Number{Device: "towel-rail", Capability: oikade.Setpoint}
	LoungeShutter  = oikade.Number{Device: "lounge-shutter", Capability: oikade.Position}
)

type presenceDevice struct {
	oikade.Device
	Occupied oikade.Bool
}

func presenceSensor(name string) presenceDevice {
	return presenceDevice{oikade.Device{Name: name}, oikade.Bool{Device: name, Capability: oikade.Occupied}}
}

// awayHome keeps the towel rail at frost protection while nobody has been seen for a day and a half,
// and closes the shutter once everyone has gone.
func awayHome() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	presence := home.Group("presence", HallSensor, BathroomSensor)
	anyoneHome := home.Signal("anyone-home", presence.Occupied.Any()).Linger(36 * time.Hour)
	home.Decide(TowelRail).Case("away", 7, oikade.Not(anyoneHome)).Default(19)
	home.Flow("everyone-left", setting(LoungeShutter.To(100))).When(anyoneHome.Becomes(false))
	return home
}

// awayDevices are the sensors, which report what's given, or nothing for nil.
func awayDevices(hall, bathroom any) []oikadetest.Device {
	occupied, setpoint, position := string(oikade.Occupied), string(oikade.Setpoint), string(oikade.Position)
	reported := func(v any) map[string]any {
		if v == nil {
			return map[string]any{}
		}
		return map[string]any{occupied: v}
	}
	return []oikadetest.Device{
		{Name: "hall-sensor", Capabilities: []string{occupied}, Values: reported(hall)},
		{Name: "bathroom-sensor", Capabilities: []string{occupied}, Values: reported(bathroom)},
		{Name: "towel-rail", Capabilities: []string{setpoint}, Values: map[string]any{setpoint: 19.0}},
		{Name: "lounge-shutter", Capabilities: []string{position}, Values: map[string]any{position: 0.0}},
	}
}

func TestASignalLingersAfterTheLastSighting(t *testing.T) {
	h := oikadetest.New(t, awayHome(), oikadetest.Options{Devices: awayDevices(false, false)})
	h.ExpectValue(TowelRail, 7)

	h.Set(HallSensor.Occupied, true)
	h.ExpectValue(TowelRail, 19)
	h.Set(BathroomSensor.Occupied, true)
	h.Set(HallSensor.Occupied, false)
	h.Advance(time.Hour)
	// The bathroom still sees someone, so nothing lingers yet.
	h.Set(BathroomSensor.Occupied, false)
	h.Advance(36*time.Hour - time.Minute)
	h.ExpectValue(TowelRail, 19)
	h.ExpectValue(LoungeShutter, 0)

	h.Advance(time.Minute)
	h.ExpectValue(TowelRail, 7)
	h.ExpectValue(LoungeShutter, 100)
	h.ExpectFinished("everyone-left")
	h.Advance(48 * time.Hour)
	if n := shutterWrites(h); n != 1 {
		t.Fatalf("the shutter was written %d times, want once: %v", n, h.Writes())
	}
}

func TestASightingDuringALingerKeepsTheSignalTrue(t *testing.T) {
	h := oikadetest.New(t, awayHome(), oikadetest.Options{Devices: awayDevices(true, false)})
	h.Set(HallSensor.Occupied, false)
	h.Advance(30 * time.Hour)
	h.Set(BathroomSensor.Occupied, true)
	h.Set(BathroomSensor.Occupied, false)
	h.Advance(30 * time.Hour)
	h.ExpectValue(TowelRail, 19)
	h.Advance(6 * time.Hour)
	h.ExpectValue(TowelRail, 7)
	if n := shutterWrites(h); n != 1 {
		t.Fatalf("the shutter was written %d times, want once: %v", n, h.Writes())
	}
}

func shutterWrites(h *oikadetest.Home) int {
	n := 0
	for _, w := range h.Writes() {
		if w.Device == LoungeShutter.Device {
			n++
		}
	}
	return n
}

// Sensors that have never reported can't say anyone's home, so the signal is false and the case
// that reads it applies, rather than waiting.
func TestASignalWhoseSensorsHaveNeverReportedIsFalse(t *testing.T) {
	h := oikadetest.New(t, awayHome(), oikadetest.Options{Devices: awayDevices(nil, nil)})
	h.ExpectValue(TowelRail, 7)
	// A signal that starts false hasn't changed, so the flow didn't run.
	h.ExpectValue(LoungeShutter, 0)

	h.Set(BathroomSensor.Occupied, true)
	h.ExpectValue(TowelRail, 19)
}

// A signal's first value is what its conditions say, which is no change, so a new signal doesn't
// run the flows it triggers.
func TestANewSignalStartsAtItsConditions(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	anyoneHome := home.Signal("anyone-home", HallSensor.Occupied).Delay(10 * time.Minute)
	home.Decide(TowelRail).Case("home", 21, anyoneHome).Default(19)
	home.Flow("arrived", setting(LoungeShutter.To(0))).When(anyoneHome.Becomes(true))
	h := oikadetest.New(t, home, oikadetest.Options{Devices: awayDevices(true, nil)})
	h.ExpectValue(TowelRail, 21)
	if n := shutterWrites(h); n != 0 {
		t.Fatalf("the shutter was written %d times, want none: %v", n, h.Writes())
	}

	// After that, the Delay counts.
	h.Set(HallSensor.Occupied, false)
	h.ExpectValue(TowelRail, 19)
	h.Set(HallSensor.Occupied, true)
	h.Advance(9 * time.Minute)
	h.ExpectValue(TowelRail, 19)
	h.Advance(time.Minute)
	h.ExpectValue(TowelRail, 21)
	h.ExpectFinished("arrived")
}

// The host keeps when a signal's Linger ends, so a restart doesn't start it again.
func TestASignalsLingerSurvivesARestart(t *testing.T) {
	h := oikadetest.New(t, awayHome(), oikadetest.Options{Devices: awayDevices(true, false)})
	h.Set(HallSensor.Occupied, false)
	h.Advance(10 * time.Hour)
	h.Restart()
	h.ExpectValue(TowelRail, 19)
	h.Advance(26*time.Hour - time.Minute)
	h.ExpectValue(TowelRail, 19)
	h.Advance(time.Minute)
	h.ExpectValue(TowelRail, 7)
	h.ExpectValue(LoungeShutter, 100)
}

// A signal reads the signals made before it, worked out in the same look.
func TestASignalReadsTheSignalsBeforeIt(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	upstairs := home.Signal("upstairs", BathroomSensor.Occupied)
	anyoneHome := home.Signal("anyone-home", oikade.Any(upstairs, HallSensor.Occupied))
	home.Decide(TowelRail).Case("away", 7, oikade.Not(anyoneHome)).Default(19)
	h := oikadetest.New(t, home, oikadetest.Options{Devices: awayDevices(false, false)})
	h.ExpectValue(TowelRail, 7)
	h.Set(BathroomSensor.Occupied, true)
	h.ExpectValue(TowelRail, 19)
	if got := len(h.Writes()); got != 2 {
		t.Fatalf("%d writes, want 2: away, then home in the same look: %v", got, h.Writes())
	}
}

// A signal made after another can't be read by it, even from inside a ConditionFunc, where oikade
// check can't see: the read is a mistake, so the reader's conditions count as false, and so does one
// that reads itself.
func TestASignalCantReadItselfOrALaterOne(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	var later, itself oikade.Signal
	early := home.Signal("early", oikade.ConditionFunc("later", func(s oikade.Snapshot) bool { return s.Is(later) }))
	later = home.Signal("later", HallSensor.Occupied)
	itself = home.Signal("itself", oikade.ConditionFunc("itself", func(s oikade.Snapshot) bool {
		return s.Is(itself) || s.Get(HallSensor.Occupied)
	}))
	home.Decide(TowelRail).Case("early", 21, early).Case("itself", 20, itself).Case("later", 7, later).Default(19)
	h := oikadetest.New(t, home, oikadetest.Options{Devices: awayDevices(true, nil)})
	h.ExpectValue(TowelRail, 7)
}
