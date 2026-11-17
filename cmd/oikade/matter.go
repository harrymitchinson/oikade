package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/qr"
)

// defaultWindow is how long oikade matter open keeps the window open: the longest Matter allows.
const defaultWindow = 15 * time.Minute

func runMatter(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("matter", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	window := fs.Duration("for", defaultWindow, "How long open keeps the window open: 3m to 15m.")
	words := cli.Parse(fs, args)
	req := api.Request{Type: api.TypeMatter}
	switch {
	case len(words) == 0:
	case len(words) == 1 && words[0] == "open":
		req = api.Request{Type: api.TypeMatterOpen, WindowSeconds: int(window.Seconds())}
		_, _ = fmt.Fprintln(os.Stderr, "Opening the Matter bridge's window")
	default:
		return errors.New("usage: oikade matter [open [--for DURATION]] [--json]")
	}
	resp, err := api.Call(ctx, target.transport(), req)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	drawQR := term.IsTerminal(int(os.Stdout.Fd()))
	if req.Type == api.TypeMatterOpen {
		if resp.MatterWindow == nil {
			return errors.New("the host opened no window")
		}
		if *asJSON {
			return cli.PrintJSON(resp.MatterWindow)
		}
		printWindow(os.Stdout, resp.MatterWindow, drawQR)
		return nil
	}
	if resp.Matter == nil {
		return errors.New("the host sent nothing about Matter")
	}
	if *asJSON {
		return cli.PrintJSON(resp.Matter)
	}
	printMatter(os.Stdout, resp.Matter, drawQR)
	return nil
}

func printMatter(out io.Writer, m *api.Matter, drawQR bool) {
	state := "down, and the host's log says why"
	switch {
	case m.Serving:
		state = fmt.Sprintf("serving on UDP port %d", m.Port)
	case m.Problem != "":
		state = "down: " + m.Problem
	}
	_, _ = fmt.Fprintf(out, "Matter bridge: %s\n", state)
	// The bridge's own code pairs only while it's on no fabric: once it's on one, a window takes a
	// code of its own.
	if m.ManualCode != "" && m.Serving && m.Window && len(m.Fabrics) == 0 && drawQR {
		printQR(out, m.QRCode)
	}
	if m.ManualCode != "" {
		_, _ = fmt.Fprintf(out, "Manual code: %s\nQR code: %s\n", m.ManualCode, m.QRCode)
	}
	switch {
	case !m.Serving:
	case m.Window && len(m.Fabrics) == 0:
		_, _ = fmt.Fprintln(out, "Ready to pair: scan it with the Home app, or the camera of any controller.")
	case m.Window:
		_, _ = fmt.Fprintln(out, "Ready to pair, with the code that opened the window:",
			"oikade matter open's, or the one a controller shared.")
	case len(m.Fabrics) == 0:
		_, _ = fmt.Fprintln(out, "Not ready to pair: the window has closed. oikade matter open opens it again.")
	default:
		_, _ = fmt.Fprintln(out, "To add another controller, run oikade matter open,",
			"or share the bridge from one that's paired.")
	}
	for _, f := range m.Fabrics {
		_, _ = fmt.Fprintf(out, "Paired: fabric %d, vendor %#04x %s\n", f.Index, f.VendorID, f.Label)
	}
}

func printWindow(out io.Writer, w *api.MatterWindow, drawQR bool) {
	_, _ = fmt.Fprintf(out, "Opened the Matter bridge's window until %s\n", w.Expires.Local().Format("15:04"))
	if drawQR {
		printQR(out, w.QRCode)
	}
	_, _ = fmt.Fprintf(out, "Manual code: %s\nQR code: %s\n", w.ManualCode, w.QRCode)
	if w.Fresh {
		_, _ = fmt.Fprintln(out, "These codes are this window's, as the bridge is paired, and pair nothing once it closes.")
	}
	_, _ = fmt.Fprintln(out, "Scan it with the Home app, or the camera of any controller.")
}

// printQR draws the QR code of an onboarding payload, or nothing if it can't, as the line after has
// the payload as text.
func printQR(out io.Writer, payload string) {
	c, err := qr.Encode(payload)
	if err != nil {
		return
	}
	for _, line := range c.Terminal() {
		_, _ = fmt.Fprintln(out, line)
	}
}
