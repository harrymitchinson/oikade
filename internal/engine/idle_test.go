package engine

import (
	"testing"

	"oikade.dev/internal/testkit"
)

// waitIdle returns once the engine has caught up with the fake clock. Idle counts everything the
// engine has in flight, so this needs no pause to be sure.
func waitIdle(t testing.TB, e *Engine) {
	t.Helper()
	testkit.Eventually(t, "the engine to settle", e.Idle)
}
