package discardcheck_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/harrymitchinson/oikade/internal/discardcheck"
)

func TestChains(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), discardcheck.Analyzer, "rules")
}
