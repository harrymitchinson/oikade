package discardcheck

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
	// why names these, by what takes each.
	var _ = (*framework.FlowRun).Set
	var _ = framework.NewHome
	var _ = framework.NewBundle
	var _ = framework.Run
	var _ = []framework.Part{framework.Hold{}, framework.Moment{}, framework.Flow{}, framework.Control{},
		framework.Plugin{}, framework.Bundle{}}
}
