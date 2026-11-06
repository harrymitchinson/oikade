package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

func runMatter(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("matter", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	parse(fs, args)
	resp, err := api.Call(ctx, target.transport(), api.Request{Type: api.TypeMatter})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if resp.Matter == nil {
		return errors.New("the host sent nothing about Matter")
	}
	if *asJSON {
		return printJSON(resp.Matter)
	}
	printMatter(os.Stdout, resp.Matter)
	return nil
}

func printMatter(out io.Writer, m *api.Matter) {
	state := "down, and the host's log says why"
	if m.Serving {
		state = fmt.Sprintf("serving on UDP port %d", m.Port)
	}
	_, _ = fmt.Fprintf(out, "Matter bridge: %s\nManual code: %s\nQR code: %s\n", state, m.ManualCode, m.QRCode)
	switch {
	case m.Window:
		_, _ = fmt.Fprintln(out, "Ready to pair.")
	case len(m.Fabrics) == 0:
		_, _ = fmt.Fprintln(out, "Not ready to pair: the window has closed. Restart the host to open it again.")
	default:
		_, _ = fmt.Fprintln(out, "To add another controller, share the bridge from one that's already paired.")
	}
	for _, f := range m.Fabrics {
		_, _ = fmt.Fprintf(out, "Paired: fabric %d, vendor %#04x %s\n", f.Index, f.VendorID, f.Label)
	}
}
