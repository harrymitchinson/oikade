// Command oikade is how you build a home and work with its host: init, test and dev on your laptop,
// and deploy, status and the rest over the host's API. The host itself is oikade-host.
package main

import (
	"context"
	"errors"
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
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/sourcecheck"
)

// version is the release this binary was built from, set with -ldflags "-X main.version=v0.1.0".
var version = cli.DevVersion

func main() {
	args, debug := cli.Debug(os.Args[1:], "test")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := dispatch(ctx, args, debug)
	stop()
	os.Exit(code)
}

func runDeploy(ctx context.Context, args []string) error {
	fs := newFlags("deploy")
	target := addTarget(fs)
	module := fs.String("module", ".", "Your home's module, to check and build.")
	binary := fs.String("binary", "", "A binary already built for the host, skipping the source checks.")
	allow := fs.Bool("allow-safety", false, "Go ahead even though a Safety rule would go, and end its claims.")
	renamed(fs, "allow-safety-removal", "allow-safety", true)
	_ = fs.Parse(args)
	if *binary == "" {
		if err := requireHome(*module); err != nil {
			return err
		}
	}

	d := deployer{
		stdout:   std.out,
		progress: &progress{w: os.Stderr, live: redraws(term.IsTerminal(int(os.Stderr.Fd())), os.Getenv("TERM"))},
		check:    checkSource,
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
		d.progress.phase("Checking your code")
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
	printTakenOffMatter(os.Stderr, resp.TakenOffMatter)
	printDeviceChanges(os.Stderr, resp)
	defer printUnsetSecrets(os.Stderr, resp.UnsetSecrets)
	if resp.Unchanged {
		printerOf(d.stdout).Println("Already running version " + cli.ShortVersion(resp.Version))
		return nil
	}
	return report(printerOf(d.stdout), resp, "Deployed version")
}

func runRollback(ctx context.Context, args []string) error {
	fs := newFlags("rollback")
	target := addTarget(fs)
	allow := fs.Bool("allow-safety", false, "Go ahead even though a Safety rule would go, and end its claims.")
	renamed(fs, "allow-safety-removal", "allow-safety", true)
	_ = fs.Parse(args)
	resp, err := api.Call(ctx, target.transport(), api.Request{Type: api.TypeRollback, AllowSafety: *allow})
	if err != nil {
		return err
	}
	printUnimported(os.Stderr, resp.Unimported)
	printTakenOffMatter(os.Stderr, resp.TakenOffMatter)
	printDeviceChanges(os.Stderr, resp)
	defer printUnsetSecrets(os.Stderr, resp.UnsetSecrets)
	return report(std.out, resp, "Rolled back to version")
}

// printDeviceChanges says what a version's device declarations changed, which declared names wait
// for their hardware, and what the host warns of them.
func printDeviceChanges(out io.Writer, resp api.Response) {
	for _, c := range resp.DeviceChanges {
		_, _ = fmt.Fprintln(out, c)
	}
	for _, name := range resp.Unseen {
		_, _ = fmt.Fprintf(out, "%s's hardware hasn't reported yet, so the name waits for it\n", name)
	}
	if resp.Warning != "" {
		_, _ = fmt.Fprintln(out, "Warning:", resp.Warning)
	}
}

func report(stdout *cli.Printer, resp api.Response, done string) error {
	if resp.Error == "" {
		stdout.Done("%s %s", done, cli.ShortVersion(resp.Version))
		return nil
	}
	for _, f := range resp.Findings {
		fmt.Fprintln(os.Stderr, "-", f)
	}
	if len(resp.Findings) > 0 {
		fmt.Fprintln(os.Stderr, "Pass --allow-safety to end those rules' claims and go ahead.")
	}
	if resp.Restored {
		fmt.Fprintln(os.Stderr, "Still running version", resp.Version)
	}
	return errors.New(resp.Error)
}

// checkSource fails on what only the source shows: code that would break a flow's replay, and a
// value of package oikade that is built and thrown away.
func checkSource(module string) error {
	found, err := sourcecheck.Check(module, nil)
	if err != nil {
		return err
	}
	for _, f := range found {
		fmt.Fprintln(os.Stderr, f)
	}
	if len(found) > 0 {
		return fmt.Errorf("%d problems in your code", len(found))
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
		if !ranAndFailed(err) {
			return fmt.Errorf("couldn't run go build: %w", err)
		}
		return cli.Wrap(err, "your home doesn't build, as go build says above")
	}
	return nil
}
