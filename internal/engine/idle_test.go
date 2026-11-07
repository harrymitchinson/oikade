package engine

import (
	"runtime"
	"testing"
	"time"
)

// settleLimit bounds a wait for the engine: far longer than any test's work takes, even under -race.
const settleLimit = 10 * time.Second

// poll returns once cond holds. It yields between looks rather than sleeping, so a test waits only
// as long as the engine takes.
func poll(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(settleLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// waitIdle returns once the engine has caught up with the fake clock. Idle counts everything the
// engine has in flight, so this needs no pause to be sure.
func waitIdle(t testing.TB, e *Engine) {
	t.Helper()
	poll(t, "the engine to settle", e.Idle)
}
