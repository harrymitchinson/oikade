package discardcheck

import (
	"reflect"
	"testing"

	"github.com/harrymitchinson/oikade"
)

// The analyzer's tests use a stand-in package oikade, so this ties it to the real one: a move would
// otherwise leave it checking nothing.
func TestItNamesTheRealFramework(t *testing.T) {
	if got := reflect.TypeFor[oikade.Moment]().PkgPath(); got != oikadePath {
		t.Fatalf("the analyzer looks in %s, and the package is %s", oikadePath, got)
	}
	// A decision is the exception, by name.
	if got := reflect.TypeFor[oikade.Decision[bool]]().Name(); got != "Decision[bool]" {
		t.Fatalf("a decision is %s", got)
	}
	var _ = oikade.Moment.Restart
	var _ = oikade.Set.For
	// why names these, by what takes each.
	var _ = (*oikade.FlowRun).Set
	var _ = oikade.NewHome
	var _ = oikade.NewBundle
	var _ = oikade.Run
	var _ = []oikade.Part{oikade.Moment{}, oikade.Flow{}, oikade.Plugin{}, oikade.Bundle{}}
}
