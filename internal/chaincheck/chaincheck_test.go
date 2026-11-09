package chaincheck_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/harrymitchinson/oikade/internal/chaincheck"
)

func TestChains(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), chaincheck.Analyzer, "rules")
}
