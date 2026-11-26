package engine

import "testing"

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
