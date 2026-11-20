package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/harrymitchinson/oikade/internal/cli"
)

// std is where every command prints, and exit how it ends: os.Stdout, os.Stderr and os.Exit, except
// in tests.
var (
	std  = struct{ out, err *cli.Printer }{cli.NewPrinter(os.Stdout), cli.NewPrinter(os.Stderr)}
	exit = os.Exit
)

// The commands, by the words after oikade-host that run them.
const (
	hostCommand      = ""
	clientAddCommand = "client add"
	installCommand   = "install"
	versionCommand   = "version"
	// clientWord is client add's first word, which alone means client add.
	clientWord = "client"
	// helpWord is help, which help alone or help help means the host's.
	helpWord = "help"
)

// others are oikade-host's commands besides running the host.
var others = []cli.Command{
	{
		Name:    "oikade-host client add",
		Summary: "Print a code for oikade login, or a token for CI, from the host running here.",
		Usage:   []string{"oikade-host client add NAME [--token] [--data-dir DIR] [--json]"},
		About: "It goes only over the host's socket, so a stolen token can't make more clients. " +
			"oikade list clients lists them, and oikade client revoke revokes one.",
		Example: []string{"sudo oikade-host client add laptop"},
	},
	{
		Name:    "oikade-host install",
		Summary: "Install this binary as the host under systemd, or upgrade the one there.",
		Usage:   []string{"oikade-host install [--api-addr ADDRESS] [--client NAME] [--json]"},
		About: "An upgrade backs up the store first, and puts the old binary back if the new one doesn't " +
			"answer within a minute.",
		Example: []string{"sudo oikade-host install"},
	},
	{
		Name:    "oikade-host version",
		Summary: "Print this oikade-host's version.",
		Usage:   []string{"oikade-host version"},
	},
}

// hostHelp is running the host, with no command, whose help is oikade-host help's.
var hostHelp = cli.Command{
	Name:    "oikade-host",
	Summary: "Run the host: the one process per home that runs your home and its plugins, and serves Matter and the API.",
	Usage:   []string{"oikade-host [FLAGS]"},
	About: "Everything you run from your laptop, from deploy to status, is oikade. Every command takes " +
		"--debug, or OIKADE_DEBUG=1, to show an error in full.",
	Values: map[string]string{
		"data-dir": "DIR", "home": "BINARY", "simulate": "FILE", "metrics-addr": "ADDRESS", "api-addr": "ADDRESS",
		"broker": "URL", "broker-password-secret": "NAME", "outside-plugins": "LIST", "secrets-dir": "DIR",
		"matter-port": "PORT", "matter-interfaces": "LIST", "event-log-mib": "MIB",
	},
	Hide:     slices.Collect(maps.Keys(removed)),
	Commands: others,
	Example:  []string{"oikade-host --data-dir /srv/oikade --api-addr :8443"},
}

// find is the command the words after oikade-host name.
func find(words string) (cli.Command, bool) {
	if words == hostCommand {
		return hostHelp, true
	}
	for _, c := range others {
		if c.Name == "oikade-host "+words {
			return c, true
		}
	}
	return cli.Command{}, false
}

// newFlags is the flag set of the command the words after oikade-host name, whose -h prints its
// help and whose bad flags print its usage.
func newFlags(words string) *flag.FlagSet {
	c, ok := find(words)
	if !ok {
		panic(fmt.Sprintf("no help for oikade-host %s", words))
	}
	fs := flag.NewFlagSet(c.Name, flag.ExitOnError)
	cli.HelpFlags(fs, c, std.out, std.err, exit)
	return fs
}

// dispatch runs oikade-host with args and returns the code to exit with.
func dispatch(ctx context.Context, args []string, debug bool) int {
	words, rest := hostCommand, args
	switch {
	case len(args) == 1 && args[0] == "--version":
		words, rest = versionCommand, nil
	case len(args) > 0 && args[0] == helpWord:
		// help, help client add and help install are those commands' --help.
		words, rest = strings.Join(args[1:], " "), []string{"--help"}
		if words == helpWord {
			words = hostCommand
		}
		if words == clientWord {
			words = clientAddCommand
		}
	case len(args) > 0 && !strings.HasPrefix(args[0], "-"):
		words, rest = args[0], args[1:]
		if words == clientWord {
			words = clientAddCommand
		}
	}
	c, ok := find(words)
	if !ok {
		return std.err.Fail(unknown(words), debug)
	}
	var err error
	switch words {
	case hostCommand:
		err = runHost(ctx, rest)
	case clientAddCommand:
		err = runClient(ctx, rest)
	case installCommand:
		err = runInstall(ctx, rest)
	case versionCommand:
		err = runVersion(rest)
	}
	switch e, isError := errors.AsType[*cli.Error](err); {
	case err == nil:
		return 0
	case isError && e.Usage:
		return std.err.FailUsage(err, debug, c)
	default:
		return std.err.Fail(err, debug)
	}
}

func runVersion(args []string) error {
	if extra := cli.Parse(newFlags(versionCommand), args); len(extra) > 0 {
		return cli.UsageErrorf("oikade-host version takes no %q", extra[0])
	}
	std.out.Println(cli.Version(version))
	return nil
}

// unknown says there's no command words, and names the one it's likely a slip for.
func unknown(words string) error {
	hint := "oikade-host help lists them, and everything else, from deploy to status, is oikade"
	if slip := cli.Closest(words, []string{clientWord, installCommand, versionCommand, helpWord}); slip != "" {
		hint = fmt.Sprintf("did you mean %s? oikade-host help lists them", slip)
	}
	return cli.UsageErrorf("unknown command %s", words).WithHint("%s", hint)
}
