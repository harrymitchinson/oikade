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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/install"
	"github.com/harrymitchinson/oikade/internal/store"
)

// version is the release this binary was built from, set with -ldflags "-X main.version=v0.1.0".
var version = cli.DevVersion

func main() {
	args, debug := cli.Debug(os.Args[1:])
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := dispatch(ctx, args, debug)
	stop()
	os.Exit(code)
}

func runHost(ctx context.Context, args []string) error {
	fs := newFlags(hostCommand)
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
	matterInterfaces := fs.String("matter-interfaces", "",
		"Comma-separated interfaces to advertise Matter on. Empty means every multicast interface.")
	broker := fs.String("broker", "",
		"The MQTT broker --outside-plugins speak the contract on. "+
			"A plugin your home imports, such as Zigbee2MQTT, takes its broker in its Config.")
	outside := fs.String("outside-plugins", "",
		"Comma-separated plugins that run as processes of their own and speak the MQTT contract on --broker, "+
			"under oikade/v1/<name>.")
	brokerPassword := fs.String("broker-password-secret", "",
		"The file in --data-dir's secrets directory with the password for --broker, whose URL names the user. "+
			"A password in a flag lands in ps.")
	pluginSetTimeout := fs.Duration("plugin-set-timeout", host.DefaultPluginSetTimeout,
		"How long a plugin's Set may run before the plugin counts as hung.")
	stopTimeout := fs.Duration("stop-timeout", host.DefaultStopTimeout,
		"How long stopping may take before the rest is cut off. Keep it under systemd's TimeoutStopSec.")
	for name, why := range removed {
		fs.Func(name, "Gone: "+why+".", func(string) error { return fmt.Errorf("--%s is gone: %s", name, why) })
	}
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		// A command after a flag would otherwise start a second host on the data directory.
		return cli.UsageErrorf("%q isn't a flag: a command such as add comes first, before any flags", fs.Arg(0))
	}
	if *matterPort > math.MaxUint16 {
		return cli.UsageErrorf("--matter-port %d isn't a UDP port", *matterPort)
	}

	if *outside == "" && (*broker != "" || *brokerPassword != "") {
		return cli.UsageErrorf("--broker and --broker-password-secret are only for --outside-plugins: " +
			"a plugin your home imports takes its broker in its Config, such as zigbee2mqtt.Config's Broker and Password")
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if *brokerPassword != "" {
		dir := filepath.Join(*dataDir, host.SecretsDir)
		withPassword, err := host.WithBrokerPassword(*broker, dir, *brokerPassword)
		file, unread := errors.AsType[*os.PathError](err)
		switch {
		case unread:
			return cli.Wrap(err, "could not read the broker's password from %s", file.Path).
				WithHint("put the password in that file, readable only by the host's user, and start the host again")
		case err != nil:
			return err
		}
		*broker = withPassword
	}

	h := &host.Host{
		DataDir: *dataDir, Home: *home, HeartbeatTimeout: *heartbeat, Settle: *settle, Log: log,
		Version:     cli.Version(version),
		MetricsAddr: *metricsAddr, APIAddr: *apiAddr,
		Broker: *broker, PluginSetTimeout: *pluginSetTimeout, StopTimeout: *stopTimeout,
		EventLog:   store.Bound{MaxAge: *logAge, MaxBytes: *logMiB << 20},
		RuntimeDir: runtimeDir(os.Getenv("RUNTIME_DIRECTORY")),
	}
	if *outside != "" {
		if *broker == "" {
			return cli.UsageErrorf("--outside-plugins needs --broker")
		}
		for name := range strings.SplitSeq(*outside, ",") {
			name = strings.TrimSpace(name)
			if err := ident.CheckName(ident.Plugin, name); err != nil {
				return cli.UsageErrorf("--outside-plugins: %s", err)
			}
			h.Outside = append(h.Outside, name)
		}
	}
	if *matterPort != 0 {
		h.Matter = &host.Matter{Port: uint16(*matterPort), Version: cli.Version(version)}
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
	return dataDirError(h.Run(ctx), *dataDir)
}

// within says whether path is dir or in it.
func within(path, dir string) bool {
	path, errPath := filepath.Abs(path)
	dir, errDir := filepath.Abs(dir)
	if errPath != nil || errDir != nil {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// dataDirError is err in the user's words if it's one the host's user may not write its data
// directory with, as when it isn't root.
func dataDirError(err error, dataDir string) error {
	if pe, ok := errors.AsType[*os.PathError](err); ok && errors.Is(pe, os.ErrPermission) && within(pe.Path, dataDir) {
		return cli.Wrap(err, "the host may not write its data directory, %s", dataDir).
			WithHint("run it as root, or pass --data-dir DIR for one its user may write")
	}
	return err
}

// removed are flags that described the home rather than the machine, and the field that replaced
// each. A unit file that still passes one fails loudly rather than being quietly ignored.
var removed = map[string]string{
	"matter-name": "the bridge's name is MatterName in your home's oikade.Options now: set it there and deploy",
	"secrets-dir": "your values, passwords included, go in .env beside your home's go.mod, which oikade deploy sends",
}

// runAdd trusts a new client of the network API. It goes only over the socket, so a stolen token
// can't make more of them. oikade lists and removes them.
func runAdd(ctx context.Context, args []string) error {
	fs := newFlags(addCommand)
	dataDir := fs.String("data-dir", cli.DefaultDataDir, "The host's data directory.")
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	token := fs.Bool("token", false,
		"Print a token for the client to send, such as CI, rather than a code to log in with.")
	words := cli.Parse(fs, args)
	if len(words) != 1 {
		return cli.UsageErrorf("oikade-host add takes one client, as client/NAME")
	}
	// A client is all it adds, so a bare word is one's name.
	name := words[0]
	if strings.Contains(name, "/") {
		kind, parts, err := ident.ParseRef(name)
		if err != nil || kind != ident.Client || len(parts) != 1 {
			return cli.UsageErrorf("oikade-host add takes a client, as client/NAME, not %s", name)
		}
		name = parts[0]
	}
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
			return cli.PrintJSON(newLogin(name, resp))
		}
		printLogin(name, resp)
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

// login is a new client's code, as add --json and install --json print it.
type login struct {
	Name    string    `json:"name"`
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
	Pin     string    `json:"pin"`
	Address string    `json:"address"`
}

func newLogin(name string, resp api.Response) login {
	return login{name, resp.Code, resp.Expires, resp.Pin, resp.Address}
}

func printLogin(name string, resp api.Response) {
	fmt.Printf("On %s, by %s, run:\n  oikade login %s --pin %s --code %s\n",
		name, resp.Expires.Local().Format(time.TimeOnly), resp.Address, resp.Pin, resp.Code)
	fmt.Printf("The code works once. %s is added once it's used, and oikade remove %s cancels it. If the host's "+
		"name doesn't reach it from there, use its address.\n", ident.NewRef(ident.Client, name),
		ident.NewRef(ident.Client, name))
}

// runInstall installs this binary as the host under systemd, or upgrades the one there (OIK-277).
func runInstall(ctx context.Context, args []string) error {
	fs := newFlags(installCommand)
	apiAddr := fs.String("api-addr", install.DefaultAPIAddr,
		"Where the unit serves the API over TLS. Empty serves it only on the socket. "+
			"An upgrade writes the unit again only when it's given.")
	client := fs.String("client", install.DefaultClient,
		"Who the login line is for. An upgrade prints one only when it's given.")
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	if words := cli.Parse(fs, args); len(words) > 0 {
		return cli.UsageErrorf("oikade-host install takes no %q", words[0])
	}
	opts := install.Options{APIAddr: *apiAddr, Client: *client}
	fs.Visit(func(f *flag.Flag) {
		opts.APIAddrSet = opts.APIAddrSet || f.Name == "api-addr"
		opts.ClientSet = opts.ClientSet || f.Name == "client"
	})
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return fmt.Errorf("couldn't find this binary: %w", err)
	}
	in := &install.Installer{
		Root: "/", Self: self, Version: cli.Version(version), Privileged: os.Geteuid() == 0,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err,
					strings.TrimSpace(string(out)))
			}
			return out, nil
		},
		Call: func(ctx context.Context, req api.Request) (api.Response, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return api.Call(ctx, api.Local{DataDir: cli.DefaultDataDir}, req)
		},
		Progress: os.Stderr, Wait: install.DefaultWait, Poll: 250 * time.Millisecond,
	}
	res, err := in.Install(ctx, opts)
	if err != nil {
		return err
	}
	if *asJSON {
		out := struct {
			Version      string `json:"version"`
			Upgraded     bool   `json:"upgraded"`
			From         string `json:"from,omitempty"`
			Unchanged    bool   `json:"unchanged,omitempty"`
			Previous     string `json:"previous,omitempty"`
			Backup       string `json:"backup,omitempty"`
			Login        *login `json:"login,omitempty"`
			FirewallRule string `json:"firewallRule,omitempty"`
		}{res.Version, res.Upgraded, res.From, res.Unchanged, res.Previous, res.Backup, nil, res.FirewallRule}
		if res.Login != nil {
			l := newLogin(opts.Client, *res.Login)
			out.Login = &l
		}
		return cli.PrintJSON(out)
	}
	switch {
	case res.Previous != "":
		fmt.Printf("Upgraded to %s. The previous version is %s\n", res.Version, res.Previous)
	case res.Upgraded && !res.Unchanged:
		fmt.Printf("Restarted %s with its new unit\n", install.UnitName)
	case !res.Upgraded:
		fmt.Printf("Installed oikade-host %s, running as %s\n", res.Version, install.UnitName)
	}
	if res.Login != nil {
		printLogin(opts.Client, *res.Login)
	}
	if res.Firewall != "" {
		_, _ = fmt.Fprintf(os.Stderr, "%s is active: %s\n", res.Firewall, res.FirewallRule)
	}
	return nil
}

// runtimeDir is the first of the runtime directories systemd made for the unit, listed in
// $RUNTIME_DIRECTORY, which is where the host keeps your values for its processes. It's in memory, so
// they're never on disk there. Outside systemd there's none.
func runtimeDir(listed string) string {
	first, _, _ := strings.Cut(listed, ":")
	return first
}
