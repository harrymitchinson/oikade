package clusters

import (
	"testing"
	"time"

	"oikade.dev/internal/clock"
	mcrypto "oikade.dev/internal/matter/crypto"
	"oikade.dev/internal/matter/im"
	"oikade.dev/internal/matter/tlv"
	"oikade.dev/internal/testkit"
)

var timeoutsStart = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

// waitsFor fails the test unless the soonest wait on clk is at want.
func waitsFor(t *testing.T, clk *clock.Fake, what string, want time.Time) {
	t.Helper()
	if got, ok := clk.NextWait(); !ok || !got.Equal(want) {
		t.Fatalf("%s: the soonest wait is at %v (%t), want %v", what, got, ok, want)
	}
}

// TestFailSafeIsCappedAt900s: arming the fail-safe again extends it, but never past 900 s after it
// was first armed, and it expires there (General Commissioning, ArmFailSafe).
func TestFailSafeIsCappedAt900s(t *testing.T) {
	clk := clock.NewFake(timeoutsStart)
	n := newNodeOn(t, t.TempDir(), clk)
	c := n.pase(t)
	armed := func() (a bool) {
		n.bridge.Node.View(func() { a = n.comm.failSafe.armed })
		return a
	}

	if code := armFailSafe(t, c, 600); code != commissioningOK {
		t.Fatalf("ArmFailSafe: %d", code)
	}
	waitsFor(t, clk, "armed for 600 s", timeoutsStart.Add(600*time.Second))
	clk.Step(500 * time.Second)
	if code := armFailSafe(t, c, 600); code != commissioningOK {
		t.Fatalf("ArmFailSafe again: %d", code)
	}
	waitsFor(t, clk, "armed for 600 s more, 500 s in", timeoutsStart.Add(900*time.Second))

	clk.SetTime(timeoutsStart.Add(900*time.Second - time.Nanosecond))
	if !armed() {
		t.Fatal("the fail-safe expired before 900 s")
	}
	clk.Step(time.Nanosecond)
	testkit.Eventually(t, "the fail-safe to expire at 900 s", func() bool { return !armed() })
}

// TestCommissioningWindowTimesOut: a window an administrator opens closes at the timeout it was
// opened with (Administrator Commissioning, OpenCommissioningWindow).
func TestCommissioningWindowTimesOut(t *testing.T) {
	clk := clock.NewFake(timeoutsStart)
	n := newNodeOn(t, t.TempDir(), clk)
	c := n.pase(t)
	v, _ := mcrypto.NewVerifier(12345678, random(16), 1000)
	res := invoke(t, c, IDAdministratorCommissioning, 0x00, func(w *tlv.Writer) {
		w.PutUint(tlv.Context(0), 180)
		w.PutBytes(tlv.Context(1), v.Bytes())
		w.PutUint(tlv.Context(2), 3840)
		w.PutUint(tlv.Context(3), 1000)
		w.PutBytes(tlv.Context(4), random(16))
	}, true)
	if res.Status != im.StatusSuccess {
		t.Fatalf("open: %+v", res)
	}
	waitsFor(t, clk, "a window opened for 180 s", timeoutsStart.Add(180*time.Second))

	clk.SetTime(timeoutsStart.Add(180*time.Second - time.Nanosecond))
	if !n.comm.WindowOpen() {
		t.Fatal("the window closed before 180 s")
	}
	clk.Step(time.Nanosecond)
	testkit.Eventually(t, "the window to close at 180 s", func() bool { return !n.comm.WindowOpen() })
	if v, _ := im.DecodeValue(readAttr(t, c, IDAdministratorCommissioning, 0).Data); v != uint64(windowClosed) {
		t.Errorf("WindowStatus %v once it timed out", v)
	}
}
