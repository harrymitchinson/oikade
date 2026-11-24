package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/hub"
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

// A signal's view leaves out the offline devices and those older than its MaxAge, names only those
// that had values to leave out, and says when the next of the rest gets too old.
func TestASignalsViewLeavesOutDeadDevices(t *testing.T) {
	now := time.Date(2030, time.January, 7, 8, 0, 0, 0, time.UTC)
	occupied := map[string]any{capability.Occupied: true}
	h := &home{
		devices: map[string]map[string]any{
			"hall": occupied, "landing": occupied, "attic": occupied, "cellar": {}, "study": occupied,
		},
		liveness: map[string]hub.Liveness{
			"hall":    {LastSeen: now.Add(-time.Hour)},
			"landing": {LastSeen: now.Add(-3 * time.Hour)},
			"attic":   {Offline: true, LastSeen: now},
			"cellar":  {Offline: true},
			"study":   {LastSeen: now.Add(-2 * time.Hour)},
		},
	}
	view, left, aged := h.forSignal(3*time.Hour, now)
	if !slices.Equal(left, []string{"attic", "landing"}) {
		t.Errorf("left = %v, want attic and landing", left)
	}
	if !aged.Equal(now.Add(time.Hour)) {
		t.Errorf("aged = %v, want the study's, an hour from now", aged)
	}
	for _, name := range []string{"attic", "cellar", "landing"} {
		if _, ok := view.devices[name]; ok {
			t.Errorf("the view still has %s", name)
		}
	}
	if len(h.devices) != 5 {
		t.Errorf("the home itself lost devices: %v", h.devices)
	}

	// Without one, only the offline are, the attic and the cellar, which had nothing to leave out.
	if view, left, aged := h.forSignal(0, now); !slices.Equal(left, []string{"attic"}) || !aged.IsZero() ||
		len(view.devices) != 3 {
		t.Errorf("with no MaxAge: left = %v, aged = %v, %d devices, want the attic and cellar left out", left, aged,
			len(view.devices))
	}
}
