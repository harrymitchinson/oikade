// Command oikade is one tool for building a home: it runs the host, and deploys to it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/flowcheck"
	"github.com/harrymitchinson/oikade/internal/host"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/store"
)

// version is the release this binary was built from, set with -ldflags "-X main.version=v0.1.0".
var version = devVersion

// devVersion is a build no release or go install put a version on.
const devVersion = "dev"

// defaultDataDir is where a host keeps its state, unless told otherwise.
const defaultDataDir = "/var/lib/oikade"

const usage = `Usage:
  oikade install [--binary FILE] [--user NAME] [--print] [-- HOST FLAGS...]
  oikade upgrade [--binary FILE]
  oikade backup
  oikade secret list [--json]
  oikade secret set NAME [--json]      The value comes from stdin, or is asked for.
  oikade secret remove NAME [--json]
  oikade version
  oikade host [--data-dir DIR] [--home BINARY] [--simulate FILE] [--heartbeat-timeout DURATION] [--settle DURATION]
              [--event-log-age DURATION] [--event-log-mib SIZE] [--metrics-addr ADDRESS]
              [--broker URL] [--broker-password-secret NAME] [--outside-plugins LIST] [--secrets-dir DIR]
              [--matter-port PORT] [--matter-name NAME] [--matter-interfaces LIST]
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
  oikade devices restore FILE [--force] [--json]
  oikade types [--out FILE] [--package NAME]
  oikade matter [--json]

Every command that talks to a host takes --data-dir DIR and --host NAME, which runs it on that SSH host.`

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
	case "install":
		err = runInstall(ctx, args)
	case "upgrade":
		err = runUpgrade(ctx, args)
	case "backup":
		err = runBackup(ctx, args)
	case "secret":
		err = runSecret(ctx, args)
	case "version":
		fmt.Println(currentVersion())
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
	logAge := fs.Duration("event-log-age", host.DefaultEventLogAge,
		"How long oikade replay's event log keeps events.")
	logMiB := fs.Uint64("event-log-mib", host.DefaultEventLogBytes>>20,
		"The most oikade replay's event log holds, in MiB.")
	metricsAddr := fs.String("metrics-addr", ":8080", "Where to serve metrics and health over HTTP. Empty serves neither.")
	matterPort := fs.Uint("matter-port", host.DefaultMatterPort, "The Matter bridge's UDP port. 0 turns Matter off.")
	matterName := fs.String("matter-name", "Oikade", "The Matter bridge's name, at most 32 bytes.")
	matterInterfaces := fs.String("matter-interfaces", "",
		"Comma-separated interfaces to advertise Matter on. Empty means every multicast interface.")
	broker := fs.String("broker", "",
		"An MQTT broker of yours, for --outside-plugins and imported plugins that use one, such as Zigbee2MQTT.")
	outside := fs.String("outside-plugins", "",
		"Comma-separated plugins that run as processes of their own and speak the MQTT contract on --broker, "+
			"under oikade/v1/home/<name>.")
	brokerPassword := fs.String("broker-password-secret", "",
		"The secret with the password for --broker, whose URL names the user. A password in a flag lands in ps.")
	secretsDir := fs.String("secrets-dir", "",
		"Directory of secrets, one file each. Defaults to secrets in --data-dir. A read-only one is fine.")
	pluginSetTimeout := fs.Duration("plugin-set-timeout", host.DefaultPluginSetTimeout,
		"How long a plugin's Set may run before the plugin counts as hung.")
	_ = fs.Parse(args)
	if *matterPort > math.MaxUint16 {
		return fmt.Errorf("--matter-port %d isn't a UDP port", *matterPort)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if *brokerPassword != "" {
		dir := *secretsDir
		if dir == "" {
			dir = filepath.Join(*dataDir, host.DefaultSecretsDir)
		}
		withPassword, err := host.WithBrokerPassword(*broker, dir, *brokerPassword)
		switch {
		case err == nil:
			*broker = withPassword
		case errors.Is(err, os.ErrNotExist):
			// oikade secret set needs a running host, so a first start comes before the secret does.
			log.Error("Could not read the broker's password; restart the host once it's set", "err", err,
				"secret", *brokerPassword)
		default:
			return err
		}
	}

	h := &host.Host{
		DataDir: *dataDir, Home: *home, HeartbeatTimeout: *heartbeat, Settle: *settle, MetricsAddr: *metricsAddr, Log: log,
		Broker: *broker, PluginSetTimeout: *pluginSetTimeout, SecretsDir: *secretsDir,
		EventLog: store.Bound{MaxAge: *logAge, MaxBytes: *logMiB << 20},
	}
	if *outside != "" {
		if *broker == "" {
			return errors.New("--outside-plugins needs --broker")
		}
		for name := range strings.SplitSeq(*outside, ",") {
			name = strings.TrimSpace(name)
			if !contractv1.ValidID(name) {
				return fmt.Errorf("--outside-plugins: %q isn't a plugin name", name)
			}
			h.Outside = append(h.Outside, name)
		}
	}
	if *matterPort != 0 {
		h.Matter = &host.Matter{Port: uint16(*matterPort), Name: *matterName, Version: currentVersion()}
		if *matterInterfaces != "" {
			for name := range strings.SplitSeq(*matterInterfaces, ",") {
				h.Matter.Interfaces = append(h.Matter.Interfaces, strings.TrimSpace(name))
			}
		}
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
