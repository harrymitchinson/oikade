package simhome_test

import (
	"testing"
	"time"

	"oikade.dev"
	"oikade.dev/internal/simhome"
	"oikade.dev/internal/testkit"
)

const (
	occupied = string(oikade.Occupied)
	setpoint = string(oikade.Setpoint)
)

var (
	hallSensor     = oikade.Bool{Device: "hall-sensor", Capability: oikade.Occupied}
	bathroomSensor = oikade.Bool{Device: "bathroom-sensor", Capability: oikade.Occupied}
	towelRail      = oikade.Number{Device: "towel-rail", Capability: oikade.Setpoint}
	hallLamp       = oikade.Bool{Device: "hall-lamp", Capability: oikade.On}
)

type sensor struct {
	oikade.Hardware
	Occupied oikade.Bool
}

// awaySim runs a home whose towel rail drops to 7 while the signal anyone-home, over both sensors,
// is false, with the hall sensor seeing someone and the bathroom's not. signal adds Linger or
// MaxAge, and decide more rules.
func awaySim(t *testing.T, signal func(oikade.Signal) oikade.Signal, decide func(*oikade.Home)) (*simhome.Sim, *testkit.Log) {
	t.Helper()
	home := oikade.NewHome(oikade.Options{})
	presence := home.Group("presence", sensor{oikade.Hardware{Key: "hall-sensor"}, hallSensor},
		sensor{oikade.Hardware{Key: "bathroom-sensor"}, bathroomSensor})
	anyoneHome := signal(home.Signal("anyone-home", presence.Occupied.Any()))
	home.Decide(towelRail).Case("away", 7, oikade.Not(anyoneHome)).Default(19)
	decide(home)
	simulated, err := simhome.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	log := testkit.Logs(t)
	sim, err := simhome.New(simhome.Options{Home: simulated, Start: monday, Log: log.Logger, Devices: []simhome.Device{
		{Name: "hall-sensor", Capabilities: []string{occupied}, Values: map[string]any{occupied: true}},
		{Name: "bathroom-sensor", Capabilities: []string{occupied}, Values: map[string]any{occupied: false}},
		{Name: "towel-rail", Capabilities: []string{setpoint}, Values: map[string]any{setpoint: 19.0}},
		{Name: "hall-lamp", Capabilities: []string{on}, Values: map[string]any{on: false}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	return sim, log
}

func nothingMore(*oikade.Home) {}

func expect(t *testing.T, sim *simhome.Sim, device, name string, want any) {
	t.Helper()
	if got, _ := sim.Value(device, name); got != want {
		t.Fatalf("%s %s is %v, want %v, at %v", device, name, got, want, sim.Now())
	}
}

func advance(t *testing.T, sim *simhome.Sim, d time.Duration) {
	t.Helper()
	if err := sim.Advance(d); err != nil {
		t.Fatal(err)
	}
}

// A sensor that dies while it sees someone stops counting, so the signal's Linger runs out and the
// house reads as empty, rather than as occupied for good.
func TestAnOfflineSensorStopsCountingInASignal(t *testing.T) {
	sim, log := awaySim(t, func(s oikade.Signal) oikade.Signal { return s.Linger(36 * time.Hour) }, nothingMore)
	expect(t, sim, "towel-rail", setpoint, 19.0)

	if err := sim.Availability("hall-sensor", false); err != nil {
		t.Fatal(err)
	}
	advance(t, sim, 36*time.Hour-time.Minute)
	expect(t, sim, "towel-rail", setpoint, 19.0)
	advance(t, sim, time.Minute)
	expect(t, sim, "towel-rail", setpoint, 7.0)
	if !log.Has("Left dead Devices out of Signal", "signal=signal/anyone-home", "hall-sensor") {
		t.Error("nothing logged the sensor being left out")
	}

	// Back online, it counts again at the next look.
	if err := sim.Availability("hall-sensor", true); err != nil {
		t.Fatal(err)
	}
	advance(t, sim, time.Minute)
	expect(t, sim, "towel-rail", setpoint, 19.0)
	if !log.Has("Counted every Device in Signal again", "signal=signal/anyone-home") {
		t.Error("nothing logged the sensor counting again")
	}
}

// With a MaxAge, a sensor that has been quiet that long stops counting then, with no report from
// any device, and counts again once it reports, even the same value.
func TestASignalsMaxAgeLeavesOutAQuietSensor(t *testing.T) {
	sim, _ := awaySim(t, func(s oikade.Signal) oikade.Signal { return s.MaxAge(24 * time.Hour) }, nothingMore)
	advance(t, sim, 24*time.Hour-time.Second)
	expect(t, sim, "towel-rail", setpoint, 19.0)
	advance(t, sim, time.Second)
	expect(t, sim, "towel-rail", setpoint, 7.0)

	if err := sim.Set("hall-sensor", occupied, true); err != nil {
		t.Fatal(err)
	}
	advance(t, sim, time.Minute)
	expect(t, sim, "towel-rail", setpoint, 19.0)
}

// With no MaxAge, a sensor that reports only when it changes counts however long it's quiet.
func TestASignalWithoutMaxAgeKeepsCountingAQuietSensor(t *testing.T) {
	sim, _ := awaySim(t, func(s oikade.Signal) oikade.Signal { return s }, nothingMore)
	advance(t, sim, 30*24*time.Hour)
	expect(t, sim, "towel-rail", setpoint, 19.0)
}

// Only a signal leaves out a dead sensor: a case that reads it directly still counts its last value.
func TestACaseStillReadsAnOfflineSensor(t *testing.T) {
	sim, _ := awaySim(t, func(s oikade.Signal) oikade.Signal { return s }, func(h *oikade.Home) {
		h.Decide(hallLamp).Case("occupied", true, hallSensor).Default(false)
	})
	expect(t, sim, "hall-lamp", on, true)
	if err := sim.Availability("hall-sensor", false); err != nil {
		t.Fatal(err)
	}
	expect(t, sim, "towel-rail", setpoint, 7.0)
	expect(t, sim, "hall-lamp", on, true)
}

// A signal that reads an offline sensor alone can't work it out, which counts as false, and the log
// says so at Info, as for any value nobody has reported, rather than as a mistake.
func TestASignalOfOneOfflineSensorIsFalse(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	hallBusy := home.Signal("hall-busy", hallSensor)
	home.Decide(hallLamp).Case("busy", true, hallBusy).Default(false)
	simulated, err := simhome.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	log := testkit.Logs(t)
	sim, err := simhome.New(simhome.Options{Home: simulated, Start: monday, Log: log.Logger, Devices: []simhome.Device{
		{Name: "hall-sensor", Capabilities: []string{occupied}, Values: map[string]any{occupied: true}},
		{Name: "hall-lamp", Capabilities: []string{on}, Values: map[string]any{on: false}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	expect(t, sim, "hall-lamp", on, true)

	if err := sim.Availability("hall-sensor", false); err != nil {
		t.Fatal(err)
	}
	expect(t, sim, "hall-lamp", on, false)
	if log.Has("level=ERROR") {
		t.Error("a dead sensor was logged as a mistake")
	}
	if !log.Has("Could not evaluate Signal", "hall-sensor occupied hasn't reported") {
		t.Error("nothing logged why the signal can't be worked out")
	}
}
