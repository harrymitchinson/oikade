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
	"runtime"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
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
	envFile := fs.String("env-file", "", "Your values, from this file rather than the .env beside your go.mod.")
	keepVars := fs.Bool("keep-vars", false,
		"Send no values, so the host keeps its own, as from CI or a clone with no .env.")
	allow := fs.Bool("allow-safety", false, "Go ahead even though a Safety rule would go, and end its claims.")
	renamed(fs, "allow-safety-removal", "allow-safety", true)
	_ = fs.Parse(args)
	if *keepVars && *envFile != "" {
		return cli.UsageErrorf("--keep-vars keeps the host's values, and --env-file replaces them: pass one or the other")
	}
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
		manifest: homeCheck,
		lookup:   os.LookupEnv,
	}
	return d.run(ctx, target.transport(), deployOptions{
		module: *module, binary: *binary, envFile: *envFile, keepVars: *keepVars, allow: *allow,
	})
}

// deployOptions are a deploy's flags.
type deployOptions struct {
	// module is your home's, or binary one already built for the host.
	module, binary string
	// envFile is the file of your values, rather than the .env beside module's go.mod.
	envFile string
	// keepVars sends no values, so the host keeps its own.
	keepVars bool
	// allow ends a Safety rule's claims, rather than refusing a version that would.
	allow bool
}

// deployer is a deploy, with what it writes to, the source checks, build and check of your home it
// runs, and the environment it reads your values from.
type deployer struct {
	stdout   io.Writer
	progress *progress
	check    func(module string) error
	build    func(ctx context.Context, module, out string, info api.Response) error
	// manifest asks your home what it is, given vars: the binary at path, or the module built for
	// this machine if path is empty.
	manifest func(ctx context.Context, module, path string, vars map[string]string) (wire.Manifest, error)
	lookup   func(name string) (string, bool)
}

// run deploys the module, or the binary if it's given, with your values, and says how it's going on
// the way. A value your code uses that nothing gives stops it before the upload. With keepVars, it
// sends none, and the host checks the version against its own.
func (d deployer) run(ctx context.Context, transport api.Transport, o deployOptions) error {
	module, envFile := o.module, o.envFile
	path := o.binary
	var vals values
	// Your values are read as soon as what your code uses is known, so a missing one stops the deploy
	// before anything slow.
	read := func(used []string) (err error) {
		vals, err = readValues(module, envFile, used, d.lookup)
		return err
	}
	// learn asks your home's check which values its code uses, then reads them. The check has the
	// file's, as the host's has the deploy's.
	learn := func(path string) error {
		given, _, err := collectValues(module, envFile, nil, d.lookup)
		if err != nil {
			return err
		}
		m, err := d.manifest(ctx, module, path, given.set)
		switch {
		case cantRunHere(err):
			// The host checks it, and refuses it if it uses a value it isn't sent, and only the
			// file's are.
			return read(nil)
		case err != nil:
			return err
		}
		return read(m.Vars)
	}
	if o.keepVars {
		// Only the host knows which values it has, so its check is the one that counts.
		learn = nil
	}
	if path == "" {
		info, err := api.Call(ctx, transport, api.Request{Type: api.TypeInfo})
		if err != nil {
			return err
		}
		d.progress.phase("Checking your code")
		if err := d.check(module); err != nil {
			return err
		}
		// What your code uses comes from its check, which only a binary for this machine can run.
		native := info.GOOS == runtime.GOOS && info.GOARCH == runtime.GOARCH
		if !native && learn != nil {
			stop := heartbeat(d.progress, "checking", 5*time.Second)
			err := learn("")
			stop()
			if err != nil {
				return err
			}
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
		if native && learn != nil {
			d.progress.phase("Checking your home")
			if err := learn(path); err != nil {
				return err
			}
		}
	} else if learn != nil {
		d.progress.phase("Checking your home")
		if err := learn(path); err != nil {
			return err
		}
	}
	file, err := os.Stat(path)
	if err != nil {
		return err
	}
	d.progress.startUpload(file.Size())
	resp, err := api.Deploy(ctx, transport, path, vals.set, o.allow, d.progress.upload)
	d.progress.end()
	if err != nil {
		return err
	}
	printUnimported(os.Stderr, resp.Unimported)
	printTakenOffMatter(os.Stderr, resp.TakenOffMatter)
	printDeviceChanges(os.Stderr, resp)
	stdout := printerOf(d.stdout)
	switch {
	case resp.Error != "":
	case resp.Unchanged && len(resp.VarsChanged) > 0:
		stdout.Done("Deployed version %s again, with %s changed", cli.ShortVersion(resp.Version), andList(resp.VarsChanged))
		return nil
	case resp.Unchanged:
		stdout.Println("Already running version " + cli.ShortVersion(resp.Version))
		return nil
	}
	if o.keepVars {
		return report(stdout, resp, "Deployed version", ", keeping the host's values",
			fmt.Sprintf("add %s to your .env, and deploy without --keep-vars, which sends your values", them(resp.MissingVars)))
	}
	// The host refuses a value that's missing only when the check couldn't run here, and then the
	// environment's go only for the file's names.
	return report(stdout, resp, "Deployed version", vals.sent(),
		fmt.Sprintf("add %s to %s, or pass --keep-vars to keep the host's values", them(resp.MissingVars), vals.file))
}

// homeCheck runs your home's check with vars, for the values its code uses: the binary at path, or
// the module built for this machine. The host checks the protocol, so a deploy doesn't, and a binary
// that can't run here, such as one for another machine, is a notHere.
func homeCheck(ctx context.Context, module, path string, vars map[string]string) (wire.Manifest, error) {
	if path == "" {
		built, done, err := buildHome(ctx, module)
		if err != nil {
			return wire.Manifest{}, err
		}
		defer done()
		path = built
	}
	// A bare name, as in --binary home, is a file here, not a command on the PATH.
	abs, err := filepath.Abs(path)
	if err != nil {
		return wire.Manifest{}, err
	}
	return checkWith(ctx, abs, vars)
}

// them is "it" or "them", for names.
func them(names []string) string {
	if len(names) == 1 {
		return "it"
	}
	return "them"
}

func runRollback(ctx context.Context, args []string) error {
	fs := newFlags("rollback")
	target := addTarget(fs)
	envFile := fs.String("env-file", "",
		"Values to replace the host's with, from this file. Without it, the host keeps its own.")
	allow := fs.Bool("allow-safety", false, "Go ahead even though a Safety rule would go, and end its claims.")
	renamed(fs, "allow-safety-removal", "allow-safety", true)
	_ = fs.Parse(args)
	req := api.Request{Type: api.TypeRollback, AllowSafety: *allow}
	var vals values
	if *envFile != "" {
		var err error
		if vals, err = readValues(".", *envFile, nil, os.LookupEnv); err != nil {
			return err
		}
		req.Vars = vals.set
	}
	resp, err := api.Call(ctx, target.transport(), req)
	if err != nil {
		return err
	}
	printUnimported(os.Stderr, resp.Unimported)
	printTakenOffMatter(os.Stderr, resp.TakenOffMatter)
	printDeviceChanges(os.Stderr, resp)
	hint := "oikade rollback --env-file FILE rolls back with values of your own"
	if *envFile != "" {
		hint = fmt.Sprintf("add %s to %s", them(resp.MissingVars), vals.file)
	}
	return report(std.out, resp, "Rolled back to version", vals.sent(), hint)
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

// report says how a deploy or a rollback went: done, the version and with when it went ahead, and
// the host's error when it didn't, with missingHint for a version that uses values it wasn't given.
func report(stdout *cli.Printer, resp api.Response, done, with, missingHint string) error {
	if resp.Error == "" {
		stdout.Done("%s %s%s", done, cli.ShortVersion(resp.Version), with)
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
	if len(resp.MissingVars) > 0 {
		return cli.Errorf("%s", resp.Error).WithHint("%s", missingHint)
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
