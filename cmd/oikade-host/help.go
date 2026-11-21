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

	"oikade.dev/internal/cli"
)

// std is where every command prints, and exit how it ends: os.Stdout, os.Stderr and os.Exit, except
// in tests.
var (
	std  = struct{ out, err *cli.Printer }{cli.NewPrinter(os.Stdout), cli.NewPrinter(os.Stderr)}
	exit = os.Exit
)

// The commands, by the words after oikade-host that run them.
const (
	hostCommand    = ""
	addCommand     = "add"
	installCommand = "install"
	versionCommand = "version"
	// helpWord is help, which help alone or help help means the host's.
	helpWord = "help"
)

// instead names the command to use for a word from before.
var instead = map[string]string{
	"client": "oikade-host add client/NAME adds a client, and oikade list clients lists them",
}

// others are oikade-host's commands besides running the host.
var others = []cli.Command{
	{
		Name:    "oikade-host add",
		Summary: "Add a client of the host's network API: a code for oikade login, or a token for CI.",
		Usage:   []string{"oikade-host add client/NAME [--token] [--data-dir DIR] [--json]"},
		About: "It goes only over the host's socket, so a stolen token can't make more clients. Without --token, it " +
			"adds nothing yet: the client is added once oikade login uses its code, and until then oikade list " +
			"clients shows it waiting. oikade remove client/NAME removes one, or cancels its code.",
		Example: []string{"sudo oikade-host add client/laptop"},
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
		"broker": "URL", "broker-password-secret": "NAME", "outside-plugins": "LIST",
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
		// help, help add and help install are those commands' --help.
		words, rest = strings.Join(args[1:], " "), []string{"--help"}
		if words == helpWord {
			words = hostCommand
		}
	case len(args) > 0 && !strings.HasPrefix(args[0], "-"):
		words, rest = args[0], args[1:]
	}
	c, ok := find(words)
	if !ok {
		return std.err.Fail(unknown(words), debug)
	}
	var err error
	switch words {
	case hostCommand:
		err = runHost(ctx, rest)
	case addCommand:
		err = runAdd(ctx, rest)
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
	first, _, _ := strings.Cut(strings.TrimSpace(words), " ")
	if use, ok := instead[first]; ok {
		return cli.UsageErrorf("oikade-host has no %s command", words).WithHint("%s", use)
	}
	hint := "oikade-host help lists them, and everything else, from deploy to status, is oikade"
	if slip := cli.Closest(words, []string{addCommand, installCommand, versionCommand, helpWord}); slip != "" {
		hint = fmt.Sprintf("did you mean %s? oikade-host help lists them", slip)
	}
	return cli.UsageErrorf("unknown command %s", words).WithHint("%s", hint)
}
