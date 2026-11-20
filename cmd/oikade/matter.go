package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/qr"
)

// defaultWindow is how long oikade matter open keeps the window open: the longest Matter allows.
const defaultWindow = 15 * time.Minute

func runMatter(ctx context.Context, args []string) error {
	fs := newFlags("matter")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	window := fs.Duration("for", defaultWindow, "How long open keeps the window open: 3m to 15m.")
	words := cli.Parse(fs, args)
	forSet := false
	fs.Visit(func(f *flag.Flag) { forSet = forSet || f.Name == "for" })
	var req api.Request
	switch {
	case len(words) == 0 && forSet:
		return errors.New("--for is how long oikade matter open keeps the window open, and needs open")
	case len(words) == 0:
		// What the bridge is doing is a status like any other's.
		return status(ctx, target.transport(), []named{{ref: string(ident.Matter), kind: ident.Matter}},
			statusOptions{json: *asJSON})
	case len(words) == 1 && words[0] == "open":
		// Under a second would round to 0, which the host takes as the longest.
		if *window < time.Second {
			return fmt.Errorf("--for is 3m to 15m, not %v", *window)
		}
		req = api.Request{Type: api.TypeMatterOpen, WindowSeconds: int(window.Seconds())}
		_, _ = fmt.Fprintln(os.Stderr, "Opening the Matter bridge's window")
	default:
		return cli.UsageErrorf("oikade matter takes nothing, or open")
	}
	resp, err := api.Call(ctx, target.transport(), req)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if resp.MatterWindow == nil {
		return errors.New("the host opened no window")
	}
	if *asJSON {
		return cli.PrintJSON(resp.MatterWindow)
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
			"oikade matter open replaces it with a new one.")
	case len(m.Fabrics) == 0:
		out.Println("Not ready to pair: the window has closed. oikade matter open opens it again.")
	default:
		out.Println("To add another controller, run oikade matter open,",
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
