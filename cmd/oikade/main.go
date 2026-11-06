// Command oikade is one tool for building a home. So far it has one command, host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/host"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "host" {
		fmt.Fprintln(os.Stderr, "Usage: oikade host --data-dir DIR --home BINARY "+
			"[--simulate FILE] [--heartbeat-timeout DURATION]")
		os.Exit(2)
	}
	if err := runHost(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func runHost(args []string) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Directory with all the host's state.")
	home := fs.String("home", "", "Your home's binary.")
	simulate := fs.String("simulate", "", "JSON file of simulated devices. Lines of JSON on stdin change them.")
	heartbeat := fs.Duration("heartbeat-timeout", host.DefaultHeartbeatTimeout,
		"How long your process may go without answering.")
	_ = fs.Parse(args)
	if *dataDir == "" {
		return errors.New("--data-dir is required")
	}
	if *home == "" {
		return errors.New("--home is required")
	}

	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stdout, nil))
	h := &host.Host{DataDir: *dataDir, Home: *home, HeartbeatTimeout: *heartbeat, Log: log}
	if *simulate != "" {
		devices, err := host.ReadSimulated(*simulate)
		if err != nil {
			return err
		}
		h.Devices, h.Simulate = devices, os.Stdin
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return h.Run(ctx)
}
