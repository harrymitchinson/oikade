package relays

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

var declared = []contractv1.DeclaredDevice{
	{ID: "a", Name: "hall", DisplayName: "Hall"},
	{ID: "b", Name: "porch", DisplayName: "Porch"},
	{ID: "c.misconfigured", Name: "fan", DisplayName: "Fan"},
}

func TestNewSwitchesReportsEachDevice(t *testing.T) {
	s := newSwitches(declared)
	if len(s.relays) != 2 || s.relays["a"] == nil || s.relays["b"] == nil {
		t.Fatalf("got relays %v, want a and b", s.relays)
	}
	if len(s.reports) != 3 {
		t.Fatalf("got %d reports, want 3", len(s.reports))
	}
	want := []contractv1.DeviceReport{
		{
			ID: "a", Found: true, Manufacturer: "Oikade", Model: "Simulated switch", SerialNumber: "SIM-A",
			FirmwareVersion: "1.0.0", Capabilities: []string{"on"},
		},
		{
			ID: "b", Found: true, Manufacturer: "Oikade", Model: "Simulated switch", SerialNumber: "SIM-B",
			FirmwareVersion: "1.0.0", Capabilities: []string{"on"},
		},
	}
	if !reflect.DeepEqual(s.reports[:2], want) {
		t.Fatalf("got %+v, want %+v", s.reports[:2], want)
	}
	if c := s.reports[2]; c.ID != "c.misconfigured" || c.Found || !strings.Contains(c.Error, "misconfigured") {
		t.Fatalf("got %+v, want c not found with the reason", c)
	}
}

func TestApply(t *testing.T) {
	s := newSwitches(declared)
	got, err := s.apply(contractv1.SetCommand{ID: "b", Capability: "on", Value: true})
	if err != nil || !reflect.DeepEqual(got, contractv1.State{"on": true}) {
		t.Fatalf("got %v, %v, want on true", got, err)
	}
	if s.relays["b"].values["on"] != true {
		t.Fatal("relay b didn't switch on")
	}

	for _, cmd := range []contractv1.SetCommand{
		{ID: "a", Capability: "brightness", Value: 40.0},
		{ID: "a", Capability: "on", Value: 1.0},
		{ID: "c", Capability: "on", Value: true},
		{ID: "a", Capability: "mode", Value: "dry"},
		{ID: "missing", Capability: "on", Value: true},
	} {
		if _, err := s.apply(cmd); err == nil {
			t.Fatalf("applied %+v", cmd)
		}
	}
}

// An air conditioner has an on and a mode, which starts at auto and takes a name.
func TestAnAirConditionerHasAMode(t *testing.T) {
	const id = "bedroom.air-conditioner"
	s := newSwitches([]contractv1.DeclaredDevice{{ID: id, Name: "bedroom-ac", DisplayName: "AC"}})
	if got := s.reports[0].Capabilities; !reflect.DeepEqual(got, []string{"mode", "on"}) {
		t.Fatalf("capabilities = %v", got)
	}
	got, err := s.apply(contractv1.SetCommand{ID: id, Capability: "mode", Value: "dry"})
	if err != nil || !reflect.DeepEqual(got, contractv1.State{"on": false, "mode": "dry"}) {
		t.Fatalf("got %v, %v, want mode dry", got, err)
	}
	if _, err := s.apply(contractv1.SetCommand{ID: id, Capability: "mode", Value: true}); err == nil {
		t.Fatal("a bool set the mode")
	}
}

func TestNewSwitchesDiscoversEveryRelayAndASpare(t *testing.T) {
	den := contractv1.DeclaredDevice{
		ID: "d.unstable-id", DisplayName: "Den", Room: "den",
	}
	s := newSwitches(append(slices.Clone(declared), den))
	ids := make([]string, 0, len(s.discovered))
	for _, d := range s.discovered {
		ids = append(ids, d.ID)
	}
	if len(ids) != 4 || ids[0] != "a" || ids[1] != "b" || !strings.HasPrefix(ids[2], "d.unstable-id-") ||
		ids[3] != spare.ID {
		t.Fatalf("discovered %v, want a, b, d.unstable-id-<random> and %s", ids, spare.ID)
	}
	if d := s.discovered[2]; d.Name != "Den" || d.Room != "den" {
		t.Errorf("got %+v, want the declared name and room", d)
	}
	if again := newSwitches(declared); !reflect.DeepEqual(again.discovered, newSwitches(declared).discovered) {
		t.Error("discovered IDs change between starts")
	}
}

func TestARefusedLoginIsThePluginsProblem(t *testing.T) {
	s := newSwitches([]contractv1.DeclaredDevice{
		{ID: "a", Name: "hall"}, {ID: "d.refused-login", Name: "cloud"},
	})
	if s.problem == "" || len(s.relays) != 1 {
		t.Fatalf("got problem %q and relays %v, want a problem and relay a", s.problem, s.relays)
	}
	if d := s.reports[1]; d.ID != "d.refused-login" || d.Found || d.Error == "" {
		t.Fatalf("got %+v, want d not found with the reason", d)
	}
	if newSwitches(declared).problem != "" {
		t.Fatal("a plugin with no refused login has a problem")
	}
}

func TestFaultIsAfterTheIDsLastDot(t *testing.T) {
	for id, want := range map[string]string{
		"hall": "", "cloud.refused-login": "refused-login", "porch.relay.needs-key": "needs-key",
	} {
		if got := fault(id); got != want {
			t.Errorf("fault(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestARelayWithNoKeyIsDiscoveredUnderItsName(t *testing.T) {
	s := newSwitches([]contractv1.DeclaredDevice{{ID: "l.needs-key", Name: "locked", DisplayName: "Locked"}})
	if len(s.relays) != 0 || len(s.reports) != 1 || !s.reports[0].Found || s.reports[0].Error == "" {
		t.Fatalf("got relays %v and reports %+v, want it found, unusable, with the reason", s.relays, s.reports)
	}
	if d := s.discovered[0]; d.ID != "l.needs-key" || d.Name != "Locked" {
		t.Fatalf("got %+v, want it discovered under its name", d)
	}
}
