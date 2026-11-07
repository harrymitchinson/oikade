package engine

import (
	"testing"
	"time"
)

// settleLimit bounds a wait for the engine: far longer than any test's work takes, even under -race.
const settleLimit = 10 * time.Second

// poll returns once cond holds. It looks every 50µs, so a test waits about as long as the engine
// takes, without taking a core from it.
func poll(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(settleLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// waitIdle returns once the engine has caught up with the fake clock. Idle counts everything the
// engine has in flight, so this needs no pause to be sure.
func waitIdle(t testing.TB, e *Engine) {
	t.Helper()
	poll(t, "the engine to settle", e.Idle)
}
