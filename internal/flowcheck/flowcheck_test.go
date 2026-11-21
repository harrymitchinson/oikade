package flowcheck_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"oikade.dev/internal/flowcheck"
)

func TestFlows(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), flowcheck.Analyzer, "flows")
}
