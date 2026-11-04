package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

var declared = []contractv1.DeclaredDevice{
	{ID: "a", Name: "hall", DisplayName: "Hall"},
	{ID: "b", Name: "porch", DisplayName: "Porch", Parameters: map[string]string{"capability": "light.on"}},
	{ID: "c", Name: "fan", DisplayName: "Fan", Parameters: map[string]string{"capability": "fan.on"}},
}

func TestNewSwitchesFollowsTheRelayConvention(t *testing.T) {
	s := newSwitches(declared)
	if len(s.relays) != 2 || s.relays["a"] == nil || s.relays["b"] == nil {
		t.Fatalf("got relays %v, want a and b", s.relays)
	}
	if len(s.reports) != 3 {
		t.Fatalf("got %d reports, want 3", len(s.reports))
	}
	want := []contractv1.DeviceReport{
		{ID: "a", Found: true, Manufacturer: "Oikade", Model: "Simulated switch", Capabilities: []string{"switch.on"}},
		{ID: "b", Found: true, Manufacturer: "Oikade", Model: "Simulated switch", Capabilities: []string{"light.on"}},
	}
	if !reflect.DeepEqual(s.reports[:2], want) {
		t.Fatalf("got %+v, want %+v", s.reports[:2], want)
	}
	if c := s.reports[2]; c.ID != "c" || c.Found || !strings.Contains(c.Error, "fan.on") {
		t.Fatalf("got %+v, want c not found with the reason", c)
	}
}

func TestApply(t *testing.T) {
	s := newSwitches(declared)
	got, err := s.apply(contractv1.SetCommand{ID: "b", Capability: "light.on", Value: true})
	if err != nil || !reflect.DeepEqual(got, contractv1.State{"light.on": true}) {
		t.Fatalf("got %v, %v, want light.on true", got, err)
	}
	if !s.relays["b"].on {
		t.Fatal("relay b didn't switch on")
	}

	for _, cmd := range []contractv1.SetCommand{
		{ID: "a", Capability: "light.on", Value: true},
		{ID: "a", Capability: "switch.on", Value: 1.0},
		{ID: "c", Capability: "switch.on", Value: true},
	} {
		if _, err := s.apply(cmd); err == nil {
			t.Fatalf("applied %+v", cmd)
		}
	}
}

func TestNewSwitchesDiscoversEveryRelayAndASpare(t *testing.T) {
	den := contractv1.DeclaredDevice{
		ID: "d", DisplayName: "Den", Room: "den", Parameters: map[string]string{"simulate": "unstable-id"},
	}
	s := newSwitches(append(slices.Clone(declared), den))
	ids := make([]string, 0, len(s.discovered))
	for _, d := range s.discovered {
		ids = append(ids, d.ID)
	}
	if len(ids) != 4 || ids[0] != "a" || ids[1] != "b" || !strings.HasPrefix(ids[2], "d-") || ids[3] != spare.ID {
		t.Fatalf("discovered %v, want a, b, d-<random> and %s", ids, spare.ID)
	}
	if d := s.discovered[2]; d.Name != "Den" || d.Room != "den" {
		t.Errorf("got %+v, want the declared name and room", d)
	}
	if again := newSwitches(declared); !reflect.DeepEqual(again.discovered, newSwitches(declared).discovered) {
		t.Error("discovered IDs change between starts")
	}
}
