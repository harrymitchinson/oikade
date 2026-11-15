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
	var _ func(*framework.FlowRun, framework.Set) error = (*framework.FlowRun).Set
	var _ func(framework.Options, ...framework.Part) framework.Home = framework.NewHome
	var _ func(...framework.Part) framework.Bundle = framework.NewBundle
	var _ func(framework.Home) = framework.Run
	var _ = []framework.Part{framework.Hold{}, framework.Moment{}, framework.Flow{}, framework.Control{},
		framework.Plugin{}, framework.Bundle{}}
}
