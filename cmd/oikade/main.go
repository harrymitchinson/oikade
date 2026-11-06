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
	"slices"
	"strings"
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
              [--metrics-addr ADDRESS]
  oikade deploy [--module DIR | --binary FILE] [--allow-safety-removal]
  oikade rollback [--allow-safety-removal]
  oikade status [DEVICE] [--json]
  oikade tail [--json]
  oikade set DEVICE CAPABILITY VALUE [--json]
  oikade pause RULE [--allow-safety] [--json]
  oikade resume RULE [--json]
  oikade devices [--all] [--json]
  oikade devices name DEVICE NAME [--json]
  oikade devices room DEVICE ROOM [--json]
  oikade devices reserve NAME [--room ROOM] [--json]
  oikade devices replace DEVICE WITH [--json]
  oikade devices remove DEVICE [--allow-safety] [--json]
  oikade devices restore FILE [--json]
  oikade types [--out FILE] [--package NAME]

Every command but host takes --data-dir DIR and --host NAME, which runs it on that SSH host.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	command, args := splitCommand(os.Args[1:])
	var err error
	switch command {
	case "host":
		err = runHost(ctx, args)
	case "deploy":
		err = runDeploy(ctx, args)
	case "rollback":
		err = runRollback(ctx, args)
	case "status":
		err = runStatus(ctx, args)
	case "tail":
		err = runTail(ctx, args)
	case "set":
		err = runSet(ctx, args)
	case "pause":
		err = runPause(ctx, args, true)
	case "resume":
		err = runPause(ctx, args, false)
	case "devices":
		err = runDevices(ctx, args)
	case "types":
		err = runTypes(ctx, args)
	case "proxy":
		err = runProxy(ctx, args)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// splitCommand finds the command, so that oikade --host pi status works as well as oikade status
// --host pi.
func splitCommand(args []string) (string, []string) {
	if len(args) >= 3 && args[0] == "--host" {
		return args[2], append(slices.Clone(args[:2]), args[3:]...)
	}
	if len(args) >= 2 && strings.HasPrefix(args[0], "--host=") {
		return args[1], append(slices.Clone(args[:1]), args[2:]...)
	}
	return args[0], args[1:]
}

func runHost(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "Directory with all the host's state.")
	home := fs.String("home", "", "Your home's binary, until one is deployed.")
	simulate := fs.String("simulate", "", "JSON file of simulated devices. Lines of JSON on stdin change them.")
	heartbeat := fs.Duration("heartbeat-timeout", host.DefaultHeartbeatTimeout,
		"How long your process may go without answering.")
	settle := fs.Duration("settle", host.DefaultSettle, "How long a new version must stay up.")
	metricsAddr := fs.String("metrics-addr", ":8080", "Where to serve metrics and health over HTTP. Empty serves neither.")
	_ = fs.Parse(args)

	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stdout, nil))
	h := &host.Host{
		DataDir: *dataDir, Home: *home, HeartbeatTimeout: *heartbeat, Settle: *settle, MetricsAddr: *metricsAddr, Log: log,
	}
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
	target := addTarget(fs)
	module := fs.String("module", ".", "Your home's module, to check and build.")
	binary := fs.String("binary", "", "A binary already built for the host, skipping the source checks.")
	allow := fs.Bool("allow-safety-removal", false, "Go ahead even though a Safety rule would go.")
	_ = fs.Parse(args)

	transport := target.transport()
	path := *binary
	if path == "" {
		info, err := api.Call(ctx, transport, api.Request{Type: api.TypeInfo})
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
	}
	remote, uploaded, err := transport.Upload(ctx, path)
	if err != nil {
		return err
	}
	defer uploaded()
	resp, err := api.Call(ctx, transport, api.Request{Type: api.TypeDeploy, Binary: remote, AllowSafetyRemoval: *allow})
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
	target := addTarget(fs)
	allow := fs.Bool("allow-safety-removal", false, "Go ahead even though a Safety rule would go.")
	_ = fs.Parse(args)
	resp, err := api.Call(ctx, target.transport(), api.Request{Type: api.TypeRollback, AllowSafetyRemoval: *allow})
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
