package flowcheck

import (
	"reflect"
	"testing"

	"github.com/harrymitchinson/oikade/framework"
)

// The analyzer's tests use a stand-in framework, so this ties it to the real one: a rename would
// otherwise leave it checking nothing.
func TestItNamesTheRealFlowRun(t *testing.T) {
	typ := reflect.TypeFor[framework.FlowRun]()
	if typ.Name() != runType || typ.PkgPath() != frameworkPath {
		t.Fatalf("the analyzer looks for %s.%s, and the type is %s.%s", frameworkPath, runType, typ.PkgPath(), typ.Name())
	}
	if _, ok := reflect.TypeFor[*framework.FlowRun]().MethodByName("Do"); !ok {
		t.Fatal("the analyzer exempts FlowRun.Do, which has gone")
	}
}
