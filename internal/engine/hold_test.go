package engine

import (
	"slices"
	"strings"
	"testing"
)

// A release of a rule that isn't loaded fails the run, and says how a case's key is written, as a
// case named by its device's name was once.
func TestAReleaseOfNoRuleFails(t *testing.T) {
	result, logs := runRoom(t, &fakeSetter{}, nil, Release("valve on leak"))
	if result != ResultFailed {
		t.Fatalf("result = %s, want %s", result, ResultFailed)
	}
	if !slices.ContainsFunc(logs, func(l string) bool {
		return strings.Contains(l, `no rule \"valve on leak\" is loaded`) && strings.Contains(l, "<device's key>")
	}) {
		t.Errorf("logged:\n%s", strings.Join(logs, "\n"))
	}
}
