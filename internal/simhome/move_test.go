package simhome_test

import (
	"slices"
	"testing"
	"time"

	"oikade.dev"
	"oikade.dev/internal/simhome"
	"oikade.dev/internal/testkit"
)

func TestACaseWhoseRoomLosesItsLastMemberWarnsOnce(t *testing.T) {
	occupied := string(oikade.Occupied)
	home := oikade.NewHome(oikade.Options{})
	hall := home.Room(oikade.Room("hall"))
	home.Decide(switchOf("lamp")).
		Case("hall-occupied", true, oikade.ConditionFunc("hall occupied", func(s oikade.Snapshot) bool {
			return s.Any(hall.Occupied)
		})).Linger(time.Minute).
		Default(false)
	simulated, err := simhome.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	logs := testkit.Logs(t)
	sim, err := simhome.New(simhome.Options{
		Home: simulated, Start: monday, Log: logs.Logger,
		Devices: []simhome.Device{
			{Name: "sensor", Room: "hall", Capabilities: []string{occupied}, Values: map[string]any{occupied: true}},
			{Name: "lamp", Capabilities: []string{on}, Values: map[string]any{on: false}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	if err := sim.Set("sensor", occupied, false); err != nil {
		t.Fatal(err)
	}
	// In its off-delay, the hall's only sensor moves out, and the hold looks again more than once.
	if err := sim.Move("sensor", "attic"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := sim.Advance(time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	const warned = `level=WARN msg="Rule read a Room with no members"`
	if got := logs.Count(warned); got != 1 {
		t.Fatalf("warned %d times, want once", got)
	}
	if !logs.Has(warned, "room=hall") {
		t.Fatal("the warning doesn't name the room")
	}

	// Back in the hall, then out again, is another time it happens.
	if err := sim.Move("sensor", "hall"); err != nil {
		t.Fatal(err)
	}
	if err := sim.Move("sensor", "attic"); err != nil {
		t.Fatal(err)
	}
	if got := logs.Count(warned); got != 2 {
		t.Fatalf("warned %d times after it emptied again, want twice", got)
	}
}

// A room's decision holds the devices there now, each ranked by the room's setting, apart from one
// with a decision of its own.
func TestARoomsDecisionHoldsTheDevicesThereNow(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(home.Room(oikade.Room("hall")).On).Case("evening", true, oikade.ConditionFunc("always", func(oikade.Snapshot) bool { return true })).
		Default(false)
	// The lamp's Flows line would rank the room's case as a flow's claim, but it's the lamp's own.
	home.Decide(switchOf("lamp")).Flows().Default(false)
	simulated, err := simhome.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	sim, err := simhome.New(simhome.Options{
		Home: simulated, Start: monday,
		Devices: []simhome.Device{
			{Name: "ceiling", Room: "hall", Capabilities: []string{on}, Values: map[string]any{on: false}},
			{Name: "lamp", Room: "hall", Capabilities: []string{on}, Values: map[string]any{on: true}},
			{Name: "spare", Room: "attic", Capabilities: []string{on}, Values: map[string]any{on: false}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	expect := func(device string, want bool, source string) {
		t.Helper()
		if v, _ := sim.Value(device, on); v != want {
			t.Errorf("%s is %v, want %v", device, v, want)
		}
		claims := sim.Claims(device, on)
		sources := make([]string, 0, len(claims))
		for _, c := range claims {
			sources = append(sources, c.Source)
		}
		if source != "" && !slices.Contains(sources, source) || source == "" && len(sources) > 0 {
			t.Errorf("%s's claims are %v, want %q", device, sources, source)
		}
	}
	expect("ceiling", true, "case/hall/on/evening")
	expect("lamp", false, "")
	expect("spare", false, "")

	// One that moves in takes the room's setting and its case.
	if err := sim.Move("spare", "hall"); err != nil {
		t.Fatal(err)
	}
	expect("spare", true, "case/hall/on/evening")
	// One that moves out is the room's no more.
	if err := sim.Move("ceiling", "attic"); err != nil {
		t.Fatal(err)
	}
	expect("ceiling", true, "")
}
