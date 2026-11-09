package chaincheck

import (
	"reflect"
	"testing"

	"github.com/harrymitchinson/oikade/framework"
)

// The analyzer's tests use a stand-in framework, so this ties it to the real one: a move would
// otherwise leave it checking nothing.
func TestItNamesTheRealFramework(t *testing.T) {
	if got := reflect.TypeFor[framework.Hold]().PkgPath(); got != frameworkPath {
		t.Fatalf("the analyzer looks in %s, and the framework is %s", frameworkPath, got)
	}
	var _ = framework.Hold.OffDelay
	var _ = framework.Set.For
}
