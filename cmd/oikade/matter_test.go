package main

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

const testQR = "MT:Y.K9042C00KA0648G00"

func matterOutput(m *api.Matter, drawQR bool) string {
	var b strings.Builder
	printMatter(&b, m, drawQR)
	return b.String()
}

// The QR code is drawn only to a terminal, and only while the bridge's own code pairs: a window is
// open and nothing is paired.
func TestMatterDrawsTheQRCodeWhileItPairs(t *testing.T) {
	ready := &api.Matter{Serving: true, Port: 5540, ManualCode: "34970112332", QRCode: testQR, Window: true}
	out := matterOutput(ready, true)
	// Black on bright white, so that a dark terminal doesn't draw it inverted.
	if !strings.Contains(out, "\x1b[30;107m") || !strings.Contains(out, "QR code: "+testQR) ||
		!strings.Contains(out, "Ready to pair: scan it") {
		t.Errorf("ready, to a terminal:\n%s", out)
	}
	if out := matterOutput(ready, false); strings.Contains(out, "█") || !strings.Contains(out, "QR code: "+testQR) {
		t.Errorf("ready, to a pipe:\n%s", out)
	}

	closed := *ready
	closed.Window = false
	out = matterOutput(&closed, true)
	if strings.Contains(out, "█") || !strings.Contains(out, "oikade matter open opens it again") {
		t.Errorf("window closed:\n%s", out)
	}
	paired := closed
	paired.Fabrics = []api.Fabric{{Index: 1, VendorID: 0x1349}}
	if out := matterOutput(&paired, true); strings.Contains(out, "█") || !strings.Contains(out, "run oikade matter open") {
		t.Errorf("paired:\n%s", out)
	}
	paired.Window = true
	out = matterOutput(&paired, true)
	if strings.Contains(out, "█") || !strings.Contains(out, "the code that opened the window, not these") {
		t.Errorf("paired, with a window open:\n%s", out)
	}
}

func TestMatterOpenSaysWhoseCodeItIs(t *testing.T) {
	expires := time.Date(2026, 11, 17, 14, 47, 0, 0, time.Local)
	w := &api.MatterWindow{ManualCode: "34970112332", QRCode: testQR, Expires: expires}
	var b strings.Builder
	printWindow(&b, w, true)
	out := b.String()
	if !strings.HasPrefix(out, "Opened the Matter bridge's window until 14:47\n") || !strings.Contains(out, "█") ||
		!strings.Contains(out, "Manual code: 34970112332\nQR code: "+testQR) || strings.Contains(out, "pair nothing") {
		t.Errorf("the bridge's own code:\n%s", out)
	}
	w.Fresh = true
	b.Reset()
	printWindow(&b, w, false)
	if out := b.String(); strings.Contains(out, "█") || !strings.Contains(out, "pair nothing once it closes") {
		t.Errorf("a fresh code, to a pipe:\n%s", out)
	}
}
