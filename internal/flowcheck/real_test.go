package flowcheck

import (
	"reflect"
	"testing"

	"oikade.dev"
)

// The analyzer's tests use a stand-in package oikade, so this ties it to the real one: a rename would
// otherwise leave it checking nothing.
func TestItNamesTheRealFlowRun(t *testing.T) {
	typ := reflect.TypeFor[oikade.FlowRun]()
	if typ.Name() != runType || typ.PkgPath() != oikadePath {
		t.Fatalf("the analyzer looks for %s.%s, and the type is %s.%s", oikadePath, runType, typ.PkgPath(), typ.Name())
	}
	// The analyzer exempts FlowRun.Do's call. A generic method isn't in reflect's method set, so
	// this only builds while it's there.
	var _ = (*oikade.FlowRun).Do[int]
}
