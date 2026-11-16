package flowcheck

import (
	"reflect"
	"testing"

	"github.com/harrymitchinson/oikade"
)

// The analyzer's tests use a stand-in package oikade, so this ties it to the real one: a rename would
// otherwise leave it checking nothing.
func TestItNamesTheRealFlowRun(t *testing.T) {
	typ := reflect.TypeFor[oikade.FlowRun]()
	if typ.Name() != runType || typ.PkgPath() != oikadePath {
		t.Fatalf("the analyzer looks for %s.%s, and the type is %s.%s", oikadePath, runType, typ.PkgPath(), typ.Name())
	}
	if _, ok := reflect.TypeFor[*oikade.FlowRun]().MethodByName("Do"); !ok {
		t.Fatal("the analyzer exempts FlowRun.Do, which has gone")
	}
}
