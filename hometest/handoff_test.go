package hometest_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade"
	"github.com/harrymitchinson/oikade/hometest"
)

var fountain = oikade.Bool{Device: "fountain", Capability: oikade.On}

// writesOf are the times and values of the writes to fountain, as "15:04 true".
func writesOf(h *hometest.Home) []string {
	var out []string
	for _, w := range h.Writes() {
		if w.Device == fountain.Device {
			out = append(out, fmt.Sprint(w.Time.Format("15:04"), " ", w.Value))
		}
	}
	return out
}

func fountainHome(t *testing.T, home *oikade.Home, start time.Time) *hometest.Home {
	t.Helper()
	return hometest.New(t, home, hometest.Options{Start: start, Devices: []hometest.Device{{
		Name: "fountain", Capabilities: []string{string(oikade.On)}, Values: map[string]any{string(oikade.On): false},
	}}})
}

// One case ending at the minute the next starts hands over with no write, so MinimumOff never
// rests it in between. The ending case's name sorts first on purpose: that order is the one that
// could write in between.
func TestACaseHandsOverToTheNextWithNoWrite(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(fountain).
		Limits(oikade.Limits{MinimumOn: 30 * time.Minute, MinimumOff: 30 * time.Minute}).
		Case("lunch", true, oikade.Daily("13:00", "15:00")).
		Case("tea", true, oikade.Daily("15:00", "17:00")).
		Default(false)
	h := fountainHome(t, home, time.Date(2030, time.January, 15, 12, 0, 0, 0, time.UTC))
	h.Advance(6 * time.Hour)
	if got, want := writesOf(h), []string{"13:00 true", "17:00 false"}; !slices.Equal(got, want) {
		t.Errorf("The fountain was written %v, want %v", got, want)
	}
}

// A budget's block ending at the minute a case starts hands over with no write either.
func TestABudgetHandsOverToACaseWithNoWrite(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(fountain).
		Limits(oikade.Limits{MinimumOn: 30 * time.Minute, MinimumOff: 30 * time.Minute}).
		Case("swim", true, oikade.Daily("17:00", "20:00")).
		Budget("extra", true, oikade.NewBudget(func(oikade.Snapshot) int { return 60 }).
			Prefer(oikade.Daily("16:00", "17:00"), 3).Forbid(oikade.Daily("17:00", "20:00"))).
		Default(false)
	h := fountainHome(t, home, time.Date(2030, time.January, 15, 0, 0, 0, 0, time.UTC))
	h.Advance(21 * time.Hour)
	if got, want := writesOf(h), []string{"16:00 true", "20:00 false"}; !slices.Equal(got, want) {
		t.Errorf("The fountain was written %v, want %v", got, want)
	}
}
