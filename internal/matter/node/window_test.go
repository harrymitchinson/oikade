package node

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// TestOwnersWindow: once the initial window has closed, OpenWindow opens it again with the node's
// own code. Once the node is paired, it opens a window with a code of its own, that a second
// controller pairs with, and that the node's own code doesn't open. Each closes on the clock.
func TestOwnersWindow(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 17, 14, 0, 0, 0, time.UTC))
	n, fake, _ := start(t, Config{DataDir: t.TempDir(), Clock: clk})
	testkit.Eventually(t, "the initial window", func() bool { return fake.commissionable() != nil })
	clk.Step(DefaultWindowTimeout)
	testkit.Eventually(t, "the initial window to time out", func() bool { return fake.commissionable() == nil && !n.WindowOpen() })

	for _, d := range []time.Duration{MinWindowTimeout - time.Second, DefaultWindowTimeout + time.Second} {
		if _, err := n.OpenWindow(t.Context(), d); err == nil {
			t.Errorf("opened a window for %v", d)
		}
	}

	// On no fabric, it's the initial window again.
	w, err := n.OpenWindow(t.Context(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w.Fresh || w.Payload != n.Payload() || !w.Closes.Equal(clk.Now().Add(5*time.Minute)) {
		t.Fatalf("window on no fabric %+v", w)
	}
	testkit.Eventually(t, "the basic window's advertisement", func() bool { return slices.Contains(fake.commissionable(), "CM=1") })
	cm := nodetest.New(t)
	admin := cm.Commission(addr(n), testSetup.Passcode)
	testkit.Eventually(t, "commissioning to close the window", func() bool { return fake.commissionable() == nil && !n.WindowOpen() })

	// Paired, it's a window with a passcode of its own. Opening it again makes another.
	first, err := n.OpenWindow(t.Context(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	w, err = n.OpenWindow(t.Context(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Fresh || w.Payload.Passcode == testSetup.Passcode || w.Payload.Passcode == first.Payload.Passcode {
		t.Fatalf("window on a fabric %+v, after %+v", w, first)
	}
	testkit.Eventually(t, "the enhanced window's advertisement", func() bool {
		txt := fake.commissionable()
		return slices.Contains(txt, "CM=2") && slices.Contains(txt, "D="+strconv.Itoa(int(w.Payload.Discriminator)))
	})
	if err := pase(t, client(t), n, testSetup.Passcode); err == nil {
		t.Error("the node's own passcode opened PASE in a window with its own")
	}
	if err := pase(t, client(t), n, first.Payload.Passcode); err == nil {
		t.Error("the replaced window's passcode opened PASE")
	}
	nodetest.New(t).Commission(addr(n), w.Payload.Passcode)
	testkit.Eventually(t, "the second fabric, and the window closed", func() bool {
		return n.Fabrics().Count() == 2 && !n.WindowOpen()
	})

	// A window times out.
	if _, err := n.OpenWindow(t.Context(), MinWindowTimeout); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, "the window to open", func() bool { return fake.commissionable() != nil })
	clk.Step(MinWindowTimeout)
	testkit.Eventually(t, "the window to time out", func() bool { return fake.commissionable() == nil && !n.WindowOpen() })

	// Not while a controller has the fail-safe armed.
	if f := cm.Invoke(admin, clusters.IDGeneralCommissioning, 0x00, func(w *tlv.Writer) {
		w.PutUint(tlv.Context(0), 60)
		w.PutUint(tlv.Context(1), 1)
	}); f[0] != uint64(0) {
		t.Fatalf("ArmFailSafe: %v", f)
	}
	if _, err := n.OpenWindow(t.Context(), MinWindowTimeout); !errors.Is(err, clusters.ErrWindowBusy) {
		t.Errorf("opened a window with the fail-safe armed: %v", err)
	}
}
