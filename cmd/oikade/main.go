// Command oikade is one tool for building a home: it runs the host, and deploys to it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/flowcheck"
	"github.com/harrymitchinson/oikade/internal/host"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// defaultDataDir is where a host keeps its state, unless told otherwise.
const defaultDataDir = "/var/lib/oikade"

const usage = `Usage:
  oikade host [--data-dir DIR] [--home BINARY] [--simulate FILE] [--heartbeat-timeout DURATION] [--settle DURATION]
  oikade deploy [--data-dir DIR] [--module DIR | --binary FILE] [--allow-safety-removal]
  oikade rollback [--data-dir DIR] [--allow-safety-removal]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "host":
		err = runHost(ctx, os.Args[2:])
	case "deploy":
		err = runDeploy(ctx, os.Args[2:])
	case "rollback":
		err = runRollback(ctx, os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func runHost(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "Directory with all the host's state.")
	home := fs.String("home", "", "Your home's binary, until one is deployed.")
	simulate := fs.String("simulate", "", "JSON file of simulated devices. Lines of JSON on stdin change them.")
	heartbeat := fs.Duration("heartbeat-timeout", host.DefaultHeartbeatTimeout,
		"How long your process may go without answering.")
	settle := fs.Duration("settle", host.DefaultSettle, "How long a new version must stay up.")
	_ = fs.Parse(args)

	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stdout, nil))
	h := &host.Host{DataDir: *dataDir, Home: *home, HeartbeatTimeout: *heartbeat, Settle: *settle, Log: log}
	if *simulate != "" {
		devices, err := host.ReadSimulated(*simulate)
		if err != nil {
			return err
		}
		h.Devices, h.Simulate = devices, os.Stdin
	}
	return h.Run(ctx)
}

func runDeploy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "The host's data directory.")
	module := fs.String("module", ".", "Your home's module, to check and build.")
	binary := fs.String("binary", "", "A binary already built for the host, skipping the source checks.")
	allow := fs.Bool("allow-safety-removal", false, "Go ahead even though a Safety rule would go.")
	_ = fs.Parse(args)

	socket := filepath.Join(*dataDir, api.SocketName)
	path := *binary
	if path == "" {
		info, err := api.Call(ctx, socket, api.Request{Type: api.TypeInfo})
		if err != nil {
			return err
		}
		if err := checkFlows(*module); err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "oikade-deploy")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		path = filepath.Join(dir, "home")
		if err := build(ctx, *module, path, info); err != nil {
			return err
		}
	} else if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	resp, err := api.Call(ctx, socket, api.Request{Type: api.TypeDeploy, Binary: path, AllowSafetyRemoval: *allow})
	if err != nil {
		return err
	}
	if resp.Unchanged {
		fmt.Println("Already running version", resp.Version)
		return nil
	}
	return report(resp, "Deployed version")
}

func runRollback(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "The host's data directory.")
	allow := fs.Bool("allow-safety-removal", false, "Go ahead even though a Safety rule would go.")
	_ = fs.Parse(args)
	resp, err := api.Call(ctx, filepath.Join(*dataDir, api.SocketName),
		api.Request{Type: api.TypeRollback, AllowSafetyRemoval: *allow})
	if err != nil {
		return err
	}
	return report(resp, "Rolled back to version")
}

func report(resp api.Response, done string) error {
	if resp.Error == "" {
		fmt.Println(done, resp.Version)
		return nil
	}
	for _, f := range resp.Findings {
		fmt.Fprintln(os.Stderr, "-", f)
	}
	if len(resp.Findings) > 0 {
		fmt.Fprintln(os.Stderr, "Pass --allow-safety-removal to end those rules' claims and go ahead.")
	}
	if resp.Restored {
		fmt.Fprintln(os.Stderr, "Still running version", resp.Version)
	}
	return errors.New(resp.Error)
}

// checkFlows fails on code that would break a flow's replay, which only the source shows.
func checkFlows(module string) error {
	found, err := flowcheck.Check(module, nil)
	if err != nil {
		return err
	}
	for _, f := range found {
		fmt.Fprintln(os.Stderr, f)
	}
	if len(found) > 0 {
		return fmt.Errorf("%d problems in your flows", len(found))
	}
	return nil
}

// build builds a module for the host's platform.
func build(ctx context.Context, module, out string, info api.Response) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	cmd.Dir = module
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "GOOS="+info.GOOS, "GOARCH="+info.GOARCH, "CGO_ENABLED=0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building your module: %w", err)
	}
	return nil
}
