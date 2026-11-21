package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/qr"
)

// defaultWindow is how long oikade pair matter keeps the window open: the longest Matter allows, and
// minWindow the shortest.
const (
	defaultWindow = 15 * time.Minute
	minWindow     = 3 * time.Minute
)

// runPair opens the Matter bridge's window, so a controller can pair with it. The bridge is the one
// thing a controller pairs with, as it brings your devices with it.
func runPair(ctx context.Context, args []string) error {
	fs := newFlags("pair")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	window := fs.Duration("for", defaultWindow, "How long the window stays open: 3m to 15m.")
	words := cli.Parse(fs, args)
	if len(words) != 1 {
		return cli.UsageErrorf("oikade pair takes matter, the Matter bridge, whose window it opens")
	}
	refs, err := refsOf(words)
	if err != nil {
		return err
	}
	if refs[0].kind != ident.Matter {
		return cli.UsageErrorf("oikade pair takes matter, not %s", refs[0].ref).
			WithHint("a controller pairs with the Matter bridge, which brings your devices: oikade pair matter")
	}
	if *window < minWindow || *window > defaultWindow {
		// 20m, as it was typed, rather than Go's 20m0s.
		typed := window.String()
		if minutes, ok := strings.CutSuffix(typed, "m0s"); ok {
			typed = minutes + "m"
		}
		return cli.UsageErrorf("--for is 3m to 15m, not %s", typed)
	}
	req := api.Request{Type: api.TypeMatterOpen, WindowSeconds: int(window.Seconds())}
	_, _ = fmt.Fprintln(os.Stderr, "Opening the Matter bridge's window")
	resp, err := api.Call(ctx, target.transport(), req)
	switch {
	case err != nil:
	case resp.Error != "":
		err = errors.New(resp.Error)
	case resp.MatterWindow == nil:
		err = errors.New("the host opened no window")
	}
	if err != nil {
		return failedJSON(*asJSON, string(ident.Matter), err)
	}
	if *asJSON {
		return printJSON(std.out, changedJSON{OK: true, Ref: string(ident.Matter), Window: resp.MatterWindow})
	}
	printWindow(std.out, resp.MatterWindow, std.out.Terminal)
	return nil
}

func printMatter(out *cli.Printer, m *api.Matter, drawQR bool) {
	state := "down, and the host's log says why"
	switch {
	case m.Serving:
		state = fmt.Sprintf("serving on UDP port %d", m.Port)
	case m.Problem != "":
		state = "down: " + m.Problem
	}
	out.Printf("Matter bridge: %s\n", state)
	// The bridge's own code pairs only while it's on no fabric: once it's on one, a window takes a
	// code of its own.
	if m.ManualCode != "" && m.Serving && m.Window && len(m.Fabrics) == 0 && drawQR {
		printQR(out, m.QRCode)
	}
	if m.ManualCode != "" {
		out.Printf("Manual code: %s\nQR code: %s\n", m.ManualCode, m.QRCode)
	}
	switch {
	case !m.Serving:
	case m.Window && len(m.Fabrics) == 0:
		out.Println("Ready to pair: scan it with the Home app, or the camera of any controller.")
	case m.Window:
		out.Println("Ready to pair, with the code that opened the window, not these:",
			"oikade pair matter replaces it with a new one.")
	case len(m.Fabrics) == 0:
		out.Println("Not ready to pair: the window has closed. oikade pair matter opens it again.")
	default:
		out.Println("To add another controller, run oikade pair matter,",
			"or share the bridge from one that's paired.")
	}
	if len(m.Fabrics) > 0 {
		out.Println()
		out.Println(out.Bold("Paired controllers"))
	}
	for _, f := range m.Fabrics {
		out.Println("  " + fabricLine(f))
	}
}

func printWindow(out *cli.Printer, w *api.MatterWindow, drawQR bool) {
	out.Done("Opened the Matter bridge's window until %s", w.Expires.Local().Format("15:04"))
	if drawQR {
		printQR(out, w.QRCode)
	}
	out.Printf("Manual code: %s\nQR code: %s\n", w.ManualCode, w.QRCode)
	if w.Fresh {
		out.Println("These codes are this window's, as the bridge is paired, and pair nothing once it closes.")
	}
	out.Println("Scan it with the Home app, or the camera of any controller.")
}

// fabricLine is a paired controller: its fabric, its label if it gave one, and its vendor's ID,
// such as "fabric 1: Home, vendor 0x1349", or "fabric 2: vendor 0x1384".
func fabricLine(f api.Fabric) string {
	vendor := fmt.Sprintf("vendor %#04x", f.VendorID)
	if f.Label != "" {
		return fmt.Sprintf("fabric %d: %s, %s", f.Index, f.Label, vendor)
	}
	return fmt.Sprintf("fabric %d: %s", f.Index, vendor)
}

// printQR draws the QR code of an onboarding payload, or nothing if it can't, as the line after has
// the payload as text.
func printQR(out *cli.Printer, payload string) {
	c, err := qr.Encode(payload)
	if err != nil {
		return
	}
	for _, line := range c.Terminal() {
		out.Println(cli.Image(line))
	}
}
