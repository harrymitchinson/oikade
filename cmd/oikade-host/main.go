// Command oikade-host is the host: the one process per home that runs your binary and plugins, and
// serves Matter and the API. Everything you run from your laptop is oikade.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/store"
)

// version is the release this binary was built from, set with -ldflags "-X main.version=v0.1.0".
var version = cli.DevVersion

const usage = `Usage:
  oikade-host [--data-dir DIR] [--home BINARY] [--simulate FILE] [--heartbeat-timeout DURATION] [--settle DURATION]
              [--event-log-age DURATION] [--event-log-mib SIZE] [--metrics-addr ADDRESS] [--api-addr ADDRESS]
              [--broker URL] [--broker-password-secret NAME] [--outside-plugins LIST] [--secrets-dir DIR]
              [--matter-port PORT] [--matter-name NAME] [--matter-interfaces LIST] [--plugin-set-timeout DURATION]
              [--stop-timeout DURATION]
  oikade-host client add NAME [--token] [--data-dir DIR] [--json]
                              A code for oikade login, or a token for CI, from the host running here.
  oikade-host version

With no command, oikade-host runs the host. Everything else, from deploy to status, is oikade.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	command, args := "", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch {
	case command == "" && len(args) == 1 && (args[0] == "-h" || args[0] == "--help"):
		command = "help"
	case command == "" && len(args) == 1 && args[0] == "--version":
		command = "version"
	}
	var err error
	switch command {
	case "":
		err = runHost(ctx, args)
	case "client":
		err = runClient(ctx, args)
	case "version":
		fmt.Println(cli.Version(version))
	case "help":
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

func runHost(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("oikade-host", flag.ExitOnError)
	dataDir := fs.String("data-dir", cli.DefaultDataDir, "Directory with all the host's state.")
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
	apiAddr := fs.String("api-addr", "",
		"Where to serve the API over TLS, for clients with a token, such as :8443. Empty serves it only on the socket.")
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
	stopTimeout := fs.Duration("stop-timeout", host.DefaultStopTimeout,
		"How long stopping may take before the rest is cut off. Keep it under systemd's TimeoutStopSec.")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		// A command after a flag would otherwise start a second host on the data directory.
		return fmt.Errorf("%q isn't a flag: a command such as client add comes first, before any flags", fs.Arg(0))
	}
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
		DataDir: *dataDir, Home: *home, HeartbeatTimeout: *heartbeat, Settle: *settle, Log: log,
		MetricsAddr: *metricsAddr, APIAddr: *apiAddr,
		Broker: *broker, PluginSetTimeout: *pluginSetTimeout, StopTimeout: *stopTimeout, SecretsDir: *secretsDir,
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
		h.Matter = &host.Matter{Port: uint16(*matterPort), Name: *matterName, Version: cli.Version(version)}
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

const clientUsage = "usage: oikade-host client add NAME [--token] [--data-dir DIR] [--json]"

// runClient trusts a new client of the network API. It goes only over the socket, so a stolen token
// can't make more of them. oikade lists and revokes them.
func runClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	dataDir := fs.String("data-dir", cli.DefaultDataDir, "The host's data directory.")
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	token := fs.Bool("token", false,
		"Print a token for the client to send, such as CI, rather than a code to log in with.")
	words := cli.Parse(fs, args)
	if len(words) != 2 || words[0] != "add" {
		return errors.New(clientUsage)
	}
	name := words[1]
	resp, err := api.Call(ctx, api.Local{DataDir: *dataDir},
		api.Request{Type: api.TypeClientAdd, Client: name, Token: *token})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if !*token {
		if *asJSON {
			return cli.PrintJSON(struct {
				Name    string    `json:"name"`
				Code    string    `json:"code"`
				Expires time.Time `json:"expires"`
				Pin     string    `json:"pin"`
				Address string    `json:"address"`
			}{name, resp.Code, resp.Expires, resp.Pin, resp.Address})
		}
		fmt.Printf("On %s, by %s, run:\n  oikade login %s --pin %s --code %s\n",
			name, resp.Expires.Local().Format(time.TimeOnly), resp.Address, resp.Pin, resp.Code)
		fmt.Println("The code works once. If the host's name doesn't reach it from there, use its address.")
		return nil
	}
	if resp.Pin == "" {
		_, _ = fmt.Fprintln(os.Stderr,
			"The host has no network API identity, so this token has nothing to reach yet: its log says why")
	}
	if *asJSON {
		return cli.PrintJSON(struct {
			Name  string `json:"name"`
			Token string `json:"token"`
			Pin   string `json:"pin"`
		}{name, resp.Token, resp.Pin})
	}
	fmt.Printf("Token for %s. It isn't shown again, and it can do anything you can:\n  %s\n", name, resp.Token)
	fmt.Printf("The host's pin, which a client checks it by:\n  %s\n", resp.Pin)
	return nil
}
