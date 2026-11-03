// Command oikade-conformance checks that a plugin meets the oikade/v1 contract
// (docs/plugin-contract.md). It starts the plugin against a fake hub, an in-process MQTT broker
// and Oikade's own decoder, and drives it through every topic of the contract.
//
//	oikade-conformance --devices FILE [--secrets DIR] [--timeout D] [-v] -- COMMAND [ARG...]
//
// COMMAND starts the plugin, without a shell, in the current directory. It must be the plugin
// itself, not go run or a wrapper script, because the suite signals and kills it. Paths are absolute or
// relative to the current directory. It exits 0 when every check passes, 1 when one fails and
// 2 on a usage error.
//
// Go plugins run it as a tool, so its version follows the oikade module in their go.mod:
//
//	go get -tool github.com/harrymitchinson/oikade/cmd/oikade-conformance
//	go build -o bin/my-plugin . && go tool oikade-conformance --devices conformance-devices.json -- bin/my-plugin
//
// Plugins in other languages install it in CI with go install …/cmd/oikade-conformance@vX.Y.Z.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/harrymitchinson/oikade/internal/conformance"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("oikade-conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: oikade-conformance --devices FILE [--secrets DIR] [--timeout D] [-v] "+
			"-- COMMAND [ARG...]")
		fs.PrintDefaults()
	}
	devices := fs.String("devices", "", "the devices.json to hand the plugin (required). It must declare "+
		"at least one device the plugin can find without real hardware or credentials")
	secrets := fs.String("secrets", "", "a directory to pass to the plugin as OIKADE_SECRETS_DIR")
	timeout := fs.Duration("timeout", conformance.DefaultTimeout, "how long the plugin has to react at each step")
	verbose := fs.Bool("v", false, "log what the decoder sees")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	command := fs.Args()
	if *devices == "" || len(command) == 0 {
		fs.Usage()
		return 2
	}
	cfg := conformance.Config{Command: command, Timeout: *timeout}
	var err error
	if cfg.Devices, err = filepath.Abs(*devices); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if *secrets != "" {
		if cfg.Secrets, err = filepath.Abs(*secrets); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
	}
	cfg.Log = logr.Discard()
	if *verbose {
		cfg.Log = funcr.New(func(prefix, args string) { _, _ = fmt.Fprintln(stderr, prefix, args) }, funcr.Options{})
	}
	if !conformance.Run(cfg, stdout) {
		return 1
	}
	return 0
}
