// Command oikade is how you build a home and work with its host: init, test and dev on your laptop,
// and deploy, status and the rest over the host's API. The host itself is oikade-host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/flowcheck"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// version is the release this binary was built from, set with -ldflags "-X main.version=v0.1.0".
var version = cli.DevVersion

const usage = `Usage:
  oikade init [DIR] [--module PATH] [--source DIR]
  oikade check [--module DIR]
  oikade test [--module DIR] [PACKAGES...]
  oikade dev [--module DIR] [--types FILE]
  oikade replay [--module DIR] [--types FILE] [--since DURATION] [--json]
  oikade deploy [--module DIR | --binary FILE] [--allow-safety-removal]
  oikade rollback [--allow-safety-removal]
  oikade status [DEVICE] [--json]
  oikade tail [--json]
  oikade set DEVICE CAPABILITY VALUE [--json]
  oikade pause RULE [--allow-safety] [--json]
  oikade resume RULE [--json]
  oikade devices [--all] [--json]
  oikade devices name DEVICE NAME [--allow-safety] [--json]
  oikade devices room DEVICE ROOM [--json]
  oikade devices reserve NAME [--room ROOM] [--json]
  oikade devices replace DEVICE WITH [--allow-safety] [--json]
  oikade devices remove DEVICE [--allow-safety] [--json]
  oikade devices remove --plugin NAME [--allow-safety] [--json]
  oikade devices restore FILE [--force] [--json]
  oikade types [--out FILE] [--package NAME]
  oikade matter [--json]
  oikade backup
  oikade secret list [--json]
  oikade secret set NAME [--json]      The value comes from stdin, or is asked for.
  oikade secret remove NAME [--json]
  oikade client list [--json]
  oikade client revoke NAME [--json]
  oikade login ADDRESS --pin PIN [--code CODE] [--context NAME]
  oikade version

Every command that talks to a host takes --data-dir DIR, for the host on this machine, and --context NAME,
which reaches a host you logged in to over its network API. Without either, a command uses the context
oikade login last saved, or the host on this machine. OIKADE_TOKEN is the token of a context saved
without one. To trust a new client, run oikade-host client add NAME on the host's machine.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	command, args := os.Args[1], os.Args[2:]
	var err error
	switch command {
	case "init":
		err = runInit(ctx, args)
	case "check":
		err = runCheck(ctx, args)
	case "test":
		err = runTest(ctx, args)
	case "dev":
		err = runDev(ctx, args)
	case "replay":
		err = runReplay(ctx, args)
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
	case "matter":
		err = runMatter(ctx, args)
	case "backup":
		err = runBackup(ctx, args)
	case "secret":
		err = runSecret(ctx, args)
	case "client":
		err = runClient(ctx, args)
	case "login":
		err = runLogin(ctx, args)
	case "version":
		fmt.Println(cli.Version(version))
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown command %s\n\n%s\n", command, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func runDeploy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	target := addTarget(fs)
	module := fs.String("module", ".", "Your home's module, to check and build.")
	binary := fs.String("binary", "", "A binary already built for the host, skipping the source checks.")
	allow := fs.Bool("allow-safety-removal", false, "Go ahead even though a Safety rule would go.")
	_ = fs.Parse(args)

	d := deployer{
		stdout:   os.Stdout,
		progress: &progress{w: os.Stderr, live: redraws(term.IsTerminal(int(os.Stderr.Fd())), os.Getenv("TERM"))},
		check:    checkFlows,
		build:    build,
	}
	return d.run(ctx, target.transport(), *module, *binary, *allow)
}

// deployer is a deploy, with what it writes to and the source checks and build it runs.
type deployer struct {
	stdout   io.Writer
	progress *progress
	check    func(module string) error
	build    func(ctx context.Context, module, out string, info api.Response) error
}

// run deploys the module, or binary if it's given, and says how it's going on the way.
func (d deployer) run(ctx context.Context, transport api.Transport, module, binary string, allow bool) error {
	path := binary
	if path == "" {
		info, err := api.Call(ctx, transport, api.Request{Type: api.TypeInfo})
		if err != nil {
			return err
		}
		d.progress.phase("Checking flows")
		if err := d.check(module); err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "oikade-deploy")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		path = filepath.Join(dir, "home")
		d.progress.phase("Building for %s/%s", info.GOOS, info.GOARCH)
		stop := heartbeat(d.progress, "building", 5*time.Second)
		err = d.build(ctx, module, path, info)
		stop()
		if err != nil {
			return err
		}
	}
	file, err := os.Stat(path)
	if err != nil {
		return err
	}
	d.progress.startUpload(file.Size())
	resp, err := api.Deploy(ctx, transport, path, allow, d.progress.upload)
	d.progress.end()
	if err != nil {
		return err
	}
	printUnimported(os.Stderr, resp.Unimported)
	if resp.Unchanged {
		_, _ = fmt.Fprintln(d.stdout, "Already running version", resp.Version)
		return nil
	}
	return report(d.stdout, resp, "Deployed version")
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
	printUnimported(os.Stderr, resp.Unimported)
	return report(os.Stdout, resp, "Rolled back to version")
}

func report(stdout io.Writer, resp api.Response, done string) error {
	if resp.Error == "" {
		_, _ = fmt.Fprintln(stdout, done, resp.Version)
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
	// stdout is for the result.
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Env = append(os.Environ(), "GOOS="+info.GOOS, "GOARCH="+info.GOARCH, "CGO_ENABLED=0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building your module: %w", err)
	}
	return nil
}
