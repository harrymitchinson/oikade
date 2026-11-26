package engine

import (
	"testing"

	"oikade.dev/internal/driver"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
)

// A release clears another Rule's latch; it writes nothing itself.
func TestReleaseActionDoesNotSet(t *testing.T) {
	setter := &fakeSetter{}
	result, _ := runRoom(t, setter, nil,
		Release("workshop-overheat"),
		set("hallway-light", "on", "true"))
	if result != ResultSucceeded {
		t.Fatalf("result = %s, want %s", result, ResultSucceeded)
	}
	if got := setter.got(); len(got) != 1 || got[0].device != "hallway-light" {
		t.Fatalf("calls = %v, want only the set", got)
	}
}

// A flow releases a case by its key, or with its device's name, as a person writes it.
func TestAReleaseFindsACaseByItsDevicesName(t *testing.T) {
	store := hub.NewStore(logging.Discard())
	relays := driver.InstanceKey{Driver: driver.Plugin, Name: "relays"}
	store.SetDevices(relays, map[ident.ID]hub.Meta{{Name: "relays/v1"}: {DisplayName: "valve", Instance: relays}})
	e := &Engine{Log: logging.Discard(), Store: store}
	e.Init()
	for rule, want := range map[string]string{
		"valve on leak": "relays/v1 on leak", "relays/v1 on leak": "relays/v1 on leak",
		"morning": "morning", "lounge on dusk": "lounge on dusk",
	} {
		if got := e.ruleNamed(rule); got != want {
			t.Errorf("%q releases %q, want %q", rule, got, want)
		}
	}
}
