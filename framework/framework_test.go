package framework

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/engine"
)

func TestCompileRejectsAnUnsoundHome(t *testing.T) {
	when := func(State) bool { return true }
	for name, tc := range map[string]struct {
		home Home
		want string
	}{
		"no name": {Home{Holds: []Hold{{Device: "d", Capability: "switch.on", Value: true, When: when}}}, "needs a name"},
		"twice": {Home{
			Holds:   []Hold{{Name: "a", Device: "d", Capability: "switch.on", Value: true, When: when}},
			Moments: []Moment{{Name: "a", At: []string{"07:00"}, Do: []Action{Wait(time.Second)}}},
		}, "declared twice"},
		"unknown capability": {
			Home{Holds: []Hold{{Name: "a", Device: "d", Capability: "nope", Value: true, When: when}}}, "unknown capability",
		},
		"no trigger": {Home{Moments: []Moment{{Name: "a", Do: []Action{Wait(time.Second)}}}}, "trigger"},
		"nil action": {Home{Moments: []Moment{{Name: "a", At: []string{"07:00"}, Do: []Action{nil}}}}, "no action"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.home.compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

func TestConditionSeesTheHome(t *testing.T) {
	now := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	cond := Condition(func(s State) bool {
		return s.Bool("door", "sensor.contact-open") && s.Number("room", "sensor.temperature") > 20 && s.Now().Equal(now)
	}).toEngine()
	ok, err := cond(engine.Vars{"now": now, "devices": map[string]map[string]any{
		"door": {"sensor.contact-open": true},
		"room": {"sensor.temperature": 21.5},
	}})
	if err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}

// A hold has to keep its state until the devices it reads have reported, so that isn't false.
func TestConditionReadingAnUnreportedValueIsAnError(t *testing.T) {
	cond := Condition(func(s State) bool { return !s.Bool("door", "sensor.contact-open") }).toEngine()
	if _, err := cond(engine.Vars{"devices": map[string]map[string]any{}}); err == nil {
		t.Fatal("got no error")
	}
}
