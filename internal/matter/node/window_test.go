package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
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
	var telling sync.Mutex
	var told []bool
	n, fake, _ := start(t, Config{DataDir: t.TempDir(), Clock: clk, WindowChanged: func(open bool) {
		telling.Lock()
		defer telling.Unlock()
		told = append(told, open)
	}})
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

	// WindowChanged heard each window open and close, in turn: the initial one, the owner's on no
	// fabric, the one opened twice over, and the last. Opening one again while it's open isn't news.
	testkit.Eventually(t, "the last window's closing to be told", func() bool {
		telling.Lock()
		defer telling.Unlock()
		return len(told) > 0 && len(told)%2 == 0
	})
	telling.Lock()
	for i, open := range told {
		if open != (i%2 == 0) {
			t.Errorf("WindowChanged heard %v, want opened and closed in turn", told)
			break
		}
	}
	if len(told) < 8 {
		t.Errorf("WindowChanged heard %v, want four windows", told)
	}
	telling.Unlock()

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

// jsonLines is a log in slog's JSON, as the host writes it, read back a line at a time.
type jsonLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *jsonLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// line is the first line with msg, decoded, or nil.
func (l *jsonLines) line(t *testing.T, msg string) map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for raw := range bytes.Lines(l.buf.Bytes()) {
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if line["msg"] == msg {
			return line
		}
	}
	return nil
}

// The window's lines read as the host writes them: its timeout as a duration, not nanoseconds, and
// the advertising lines under the part the mDNS responder logs as (OIK-338).
func TestTheWindowsLinesSayTheirPartAndTimeout(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC))
	log := &jsonLines{}
	n, fake, _ := start(t, Config{DataDir: t.TempDir(), Clock: clk, Log: slog.New(slog.NewJSONHandler(log, nil))})
	testkit.Eventually(t, "the initial window", func() bool { return fake.commissionable() != nil })
	testkit.Eventually(t, "the advertised line", func() bool { return log.line(t, "Advertised commissionable service") != nil })
	clk.Step(DefaultWindowTimeout)
	testkit.Eventually(t, "the window to time out", func() bool { return !n.WindowOpen() })
	testkit.Eventually(t, "the stopped line", func() bool { return log.line(t, "Stopped advertising commissionable service") != nil })

	for msg, want := range map[string]map[string]any{
		"Opened commissioning window":                                                           {"part": "commissioning", "timeout": DefaultWindowTimeout.String()},
		"Advertised commissionable service":                                                     {"part": "mdns"},
		"Stopped advertising commissionable service":                                            {"part": "mdns"},
		"Closed the commissioning window while on no fabric: oikade matter open opens it again": {"part": "commissioning"},
	} {
		line := log.line(t, msg)
		if line == nil {
			t.Errorf("no line %q", msg)
			continue
		}
		for k, v := range want {
			if line[k] != v {
				t.Errorf("%q: %s is %v (%T), want %v", msg, k, line[k], line[k], v)
			}
		}
	}
}
