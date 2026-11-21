package discardcheck_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"oikade.dev/internal/discardcheck"
)

func TestChains(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), discardcheck.Analyzer, "rules")
}
