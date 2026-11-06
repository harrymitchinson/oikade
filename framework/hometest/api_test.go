package hometest

import (
	"testing"

	"github.com/harrymitchinson/oikade/internal/apicheck"
)

// Homes live in other modules, and can't name anything internal.
func TestExportedAPIMentionsNothingInternal(t *testing.T) {
	found, err := apicheck.Check(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Error(f)
	}
}
