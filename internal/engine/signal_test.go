package engine

import (
	"strings"
	"testing"

	"oikade.dev/internal/capability"
)

// A signal's change fires only the triggers that name it, to the value they ask for, and never a
// device's.
func TestASignalsChangeMatchesOnlyItsTriggers(t *testing.T) {
	c, err := NewTrigger(goKey("flow"), Trigger{Changes: []Change{
		{Signal: "anyone-home", To: false}, {Signal: "upstairs"}, {Device: "hall-sensor", Capability: capability.Occupied},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		signal string
		value  bool
		want   bool
	}{
		"to the value asked for":       {"anyone-home", false, true},
		"to another":                   {"anyone-home", true, false},
		"to any value":                 {"upstairs", true, true},
		"a signal no trigger names":    {"downstairs", true, false},
		"a device's name, as a signal": {"hall-sensor", true, false},
	} {
		if got := c.matchesSignal(tc.signal, tc.value); got != tc.want {
			t.Errorf("%s: matchesSignal = %v, want %v", name, got, tc.want)
		}
	}
	if c.matches("anyone-home", "", nil, "", nil) {
		t.Error("a device's change matched a signal's trigger")
	}
}

func TestASignalsChangeIsChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		change Change
		want   string
	}{
		"with a device":     {Change{Signal: "anyone-home", Device: "hall-sensor"}, "names no device, room or capability"},
		"with a capability": {Change{Signal: "anyone-home", Capability: capability.Occupied}, "names no device"},
		"to a number":       {Change{Signal: "anyone-home", To: 1.0}, "a signal is true or false"},
	} {
		_, err := NewTrigger(goKey("flow"), Trigger{Changes: []Change{tc.change}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}
