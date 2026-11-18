package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// std is where every command prints, and exit how it ends: os.Stdout, os.Stderr and os.Exit, except
// in tests.
var (
	std  = struct{ out, err *cli.Printer }{cli.NewPrinter(os.Stdout), cli.NewPrinter(os.Stderr)}
	exit = os.Exit
)

// group is commands that go together in oikade help.
type group struct {
	title    string
	commands []cli.Command
}

// targetValues name the values of the flags every command that talks to a host takes.
var targetValues = map[string]string{"data-dir": "DIR", "context": "NAME"}

// groups are oikade's commands, by what you're doing.
var groups = []group{
	{"Write your home", []cli.Command{
		{
			Name:    "oikade init",
			Summary: "Start a home: a Go module that builds against Oikade.",
			Usage:   []string{"oikade init [DIR] [--module PATH] [--source DIR]"},
			About:   "DIR is the directory to start it in, the current one by default, which mustn't hold a home already.",
			Example: []string{"oikade init my-home"},
		},
		{
			Name:    "oikade check",
			Summary: "Find what's wrong with your home, as a deploy would.",
			Usage:   []string{"oikade check [--module DIR]"},
			About: "It runs the compiler, then the source checks, then the checks your home's binary makes of " +
				"itself, which are the ones a deploy makes.",
			Example: []string{"oikade check"},
		},
		{
			Name:    "oikade test",
			Summary: "Run your home's tests.",
			Usage:   []string{"oikade test [--module DIR] [PACKAGES...]"},
			About:   "PACKAGES are go test's, ./... by default, and anything after them is go test's too.",
			Example: []string{"oikade test ./... -run TestNight"},
		},
		{
			Name:    "oikade dev",
			Summary: "Run your home against simulated devices.",
			Usage:   []string{"oikade dev [--module DIR] [--types FILE]"},
			Example: []string{"oikade dev"},
		},
		{
			Name:    "oikade replay",
			Summary: "Compare what your code would have done with what the host did.",
			Usage:   []string{"oikade replay [--module DIR] [--types FILE] [--since DURATION] [--json]"},
			About: "It fetches the host's event log, runs your local code against it on a fake clock, and " +
				"compares. It asks the host only for its status and its log.",
			Example: []string{"oikade replay --since 24h"},
		},
		{
			Name:    "oikade types",
			Summary: "Write devices.go, with a Go handle for each of the host's devices.",
			Usage:   []string{"oikade types [--out FILE] [--package NAME] [--module DIR]"},
			About: "Devices are named as your home declares them once it's deployed, so a name you've just " +
				"declared has its handle at once. Commit the file: a home builds without a host.",
			Example: []string{"oikade types --out - | less"},
		},
	}},
	{"Ship it", []cli.Command{
		{
			Name:    "oikade deploy",
			Summary: "Check and build your home, and run it on the host.",
			Usage:   []string{"oikade deploy [--module DIR | --binary FILE] [--allow-safety-removal]"},
			About: "The host keeps the version it was running until the new one has stayed up, and puts it back " +
				"if it doesn't. A deploy that would end a Safety rule's claims needs --allow-safety-removal.",
			Example: []string{"oikade deploy --module ~/my-home"},
		},
		{
			Name:    "oikade rollback",
			Summary: "Go back to the version the host ran before this one.",
			Usage:   []string{"oikade rollback [--allow-safety-removal]"},
			Example: []string{"oikade rollback"},
		},
	}},
	{"Watch and drive", []cli.Command{
		{
			Name:    "oikade status",
			Summary: "Show each device's values, and the rules that set them.",
			Usage:   []string{"oikade status [DEVICE...] [--wide] [--json]"},
			Example: []string{"oikade status desk-plug"},
		},
		{
			Name:    "oikade tail",
			Summary: "Follow your home's events as they happen.",
			Usage:   []string{"oikade tail [--json]"},
			Example: []string{"oikade tail --json"},
		},
		{
			Name:    "oikade set",
			Summary: "Set a capability, as a person would, through arbitration.",
			Usage: []string{
				"oikade set device/DEVICE/CAPABILITY VALUE [--json]",
				"oikade set DEVICE CAPABILITY VALUE [--json]",
			},
			About:   "It says what the capability is afterwards, and the rule that holds it if that isn't VALUE.",
			Example: []string{"oikade set device/hall-heater/setpoint 19.5"},
		},
		{
			Name:    "oikade pause",
			Summary: "Pause a rule, so it lets go of what it holds until you resume it.",
			Usage:   []string{"oikade pause REF [--allow-safety] [--json]"},
			About: "REF is a case's, case/DEVICE/CAPABILITY/CASE, or a flow's, flow/FLOW, as oikade status " +
				"names them. A Safety rule needs --allow-safety.",
			Example: []string{"oikade pause case/hall-heater/on/occupied"},
		},
		{
			Name:    "oikade resume",
			Summary: "Resume a rule you paused.",
			Usage:   []string{"oikade resume REF [--json]"},
			About:   "REF is a case's, case/DEVICE/CAPABILITY/CASE, or a flow's, flow/FLOW.",
			Example: []string{"oikade resume flow/morning"},
			// pause's alone, which shares resume's flag set.
			Hide: []string{"allow-safety"},
		},
	}},
	{"Devices and pairing", []cli.Command{
		{
			Name:    "oikade devices",
			Summary: "List the host's devices and how each is doing, or remove one.",
			Usage: []string{
				"oikade devices [--all] [--wide] [--json]",
				"oikade devices remove DEVICE [--allow-safety] [--json]",
				"oikade devices remove --plugin NAME [--allow-safety] [--json]",
			},
			About: "A device's name, room and type are your home's code's: home.Device declares them. A removed " +
				"device keeps its Matter endpoint, so hardware that comes back is the same device.",
			Example: []string{"oikade devices --wide", "oikade devices remove --plugin zigbee2mqtt"},
		},
		{
			Name:    "oikade setup",
			Summary: "Give a plugin what it needs, such as a sign-in, on the host.",
			Usage:   []string{"oikade setup PLUGIN [--json]"},
			Example: []string{"oikade setup overkiz"},
		},
		{
			Name:    "oikade matter",
			Summary: "Show the Matter bridge, or open its window to pair a controller.",
			Usage:   []string{"oikade matter [--json]", "oikade matter open [--for DURATION] [--json]"},
			Example: []string{"oikade matter open --for 10m"},
		},
	}},
	{"Host access", []cli.Command{
		{
			Name:    "oikade login",
			Summary: "Trust a host's network API, and use it from now on.",
			Usage:   []string{"oikade login ADDRESS --pin PIN [--code CODE] [--context NAME]"},
			About:   "oikade-host client add, on the host's machine, prints the line to run.",
			Example: []string{"oikade login pi.home:8443 --pin sha256:9f2c… --code k7Qx…"},
		},
		{
			Name:    "oikade client",
			Summary: "List or revoke who may call the host's network API.",
			Usage:   []string{"oikade client list [--json]", "oikade client revoke NAME [--json]"},
			About:   "To add one, run oikade-host client add NAME on the host's machine.",
			Example: []string{"oikade client revoke old-laptop"},
		},
		{
			Name:    "oikade secret",
			Summary: "List, set or remove the secrets your home and its plugins read.",
			Usage: []string{
				"oikade secret list [--json]",
				"oikade secret set NAME [--json]",
				"oikade secret remove NAME [--json]",
			},
			About:   "set takes the value from stdin, or asks for it, and never from an argument, which ps would show.",
			Example: []string{"oikade secret set weather-api-key"},
		},
		{
			Name:    "oikade backup",
			Summary: "Back up the host's store, on the host, and print where it went.",
			Usage:   []string{"oikade backup"},
		},
		{
			Name:    "oikade version",
			Summary: "Print this oikade's version, and its host's when one answers.",
			About: "It asks the host a command would reach, the current context's or this machine's, or the one " +
				"--context or --data-dir names, and waits 3 s at most.",
			Usage: []string{"oikade version"},
		},
	}},
}

// runners are what runs each command, by its word.
func runners() map[string]func(ctx context.Context, args []string) error {
	return map[string]func(ctx context.Context, args []string) error{
		"init": runInit, "check": runCheck, "test": runTest, "dev": runDev, "replay": runReplay, "types": runTypes,
		"deploy": runDeploy, "rollback": runRollback,
		"status": runStatus, "tail": runTail, "set": runSet,
		"pause":   func(ctx context.Context, args []string) error { return runPause(ctx, args, true) },
		"resume":  func(ctx context.Context, args []string) error { return runPause(ctx, args, false) },
		"devices": runDevices, "setup": runSetup, "matter": runMatter,
		"login": runLogin, "client": runClient, "secret": runSecret, "backup": runBackup, "version": runVersion,
	}
}

// hostWait is how long oikade version waits for a host to say its version.
const hostWait = 3 * time.Second

func runVersion(ctx context.Context, args []string) error {
	fs := newFlags("version")
	target := addTarget(fs)
	if extra := cli.Parse(fs, args); len(extra) > 0 {
		return cli.UsageErrorf("oikade version takes no %q", extra[0])
	}
	std.out.Println("oikade " + cli.Version(version))
	raw, name := target.resolve()
	if f, ok := raw.(failing); ok { // A context that isn't there, say.
		return f.err
	}
	if name != thisMachine {
		std.err.Printf("Asking %s for its version\n", name)
	}
	ctx, cancel := context.WithTimeout(ctx, hostWait)
	defer cancel()
	info, err := api.Call(ctx, target.transport(), api.Request{Type: api.TypeInfo})
	if err == nil && info.Error != "" {
		err = errors.New(info.Error)
	}
	if err != nil {
		why := err.Error()
		if e, ok := errors.AsType[*cli.Error](err); ok {
			why = e.Message
		}
		std.err.Println(std.err.Dim("Couldn't ask the host for its version: " + why))
		return nil
	}
	std.out.Println(hostVersionLine(info, name))
	return nil
}

// hostVersionLine is a host's version, where it is and what it runs on:
// "oikade-host v0.2.0 on this machine, linux/arm64".
func hostVersionLine(info api.Response, name string) string {
	where := "on this machine"
	if name != thisMachine {
		where = "at " + name
	}
	if info.HostVersion == "" {
		return fmt.Sprintf("oikade-host %s, %s/%s, too old to say its version", where, info.GOOS, info.GOARCH)
	}
	return fmt.Sprintf("oikade-host %s %s, %s/%s", info.HostVersion, where, info.GOOS, info.GOARCH)
}

// overviewAfter is what oikade help says after its commands.
var overviewAfter = []string{
	"oikade help COMMAND, or oikade COMMAND --help, says more of one.",
	"Every command that talks to a host takes --data-dir DIR, for the host on this machine, and --context " +
		"NAME, which reaches a host you logged in to over its network API. Without either, a command uses the " +
		"context oikade login last saved, or the host on this machine. OIKADE_TOKEN is the token of a context " +
		"saved without one. Every command takes --debug, or OIKADE_DEBUG=1, to show an error in full.",
}

// find is the command named word, as in oikade help deploy.
func find(word string) (cli.Command, bool) {
	for _, g := range groups {
		for _, c := range g.commands {
			if c.Word() == word {
				return c, true
			}
		}
	}
	return cli.Command{}, false
}

// words are every command's, for a slip's suggestion.
func words() []string {
	var all []string
	for _, g := range groups {
		for _, c := range g.commands {
			all = append(all, c.Word())
		}
	}
	return append(all, "help")
}

// printHelp prints oikade help.
func printHelp(p *cli.Printer) {
	cliGroups := make([]cli.Group, len(groups))
	for i, g := range groups {
		cliGroups[i].Title = g.title
		cliGroups[i].Commands = append(cliGroups[i].Commands, g.commands...)
	}
	p.Overview("oikade", "oikade COMMAND [ARGUMENTS] [FLAGS]", cliGroups, overviewAfter...)
}

// newFlags is the flag set of the command named word, whose -h prints its help and whose bad flags
// print its usage.
func newFlags(word string) *flag.FlagSet {
	fs := flag.NewFlagSet(word, flag.ExitOnError)
	c, ok := find(word)
	if !ok {
		panic(fmt.Sprintf("no help for oikade %s", word))
	}
	c.Values = targetValues
	cli.HelpFlags(fs, c, std.out, std.err, exit)
	return fs
}

// unknown says there's no command word, and names the one it's likely a slip for.
func unknown(word string) error {
	hint := "oikade help lists them"
	if slip := cli.Closest(word, words()); slip != "" {
		hint = fmt.Sprintf("did you mean %s? %s", slip, hint)
	}
	return cli.UsageErrorf("unknown command %s", word).WithHint("%s", hint)
}

// dispatch runs oikade with args, the command first, and returns the code to exit with.
func dispatch(ctx context.Context, args []string, debug bool) int {
	if len(args) == 0 {
		printHelp(std.err)
		return cli.ExitUsage
	}
	word, args := args[0], args[1:]
	switch word {
	case "--version":
		word = "version"
	case "help", "-h", "--help":
		if len(args) == 0 || args[0] == "help" {
			printHelp(std.out)
			return 0
		}
		word, args = args[0], []string{"--help"}
		if _, ok := find(word); !ok {
			return std.err.Fail(unknown(word), debug)
		}
	}
	c, ok := find(word)
	if !ok {
		return std.err.Fail(unknown(word), debug)
	}
	err := runners()[word](ctx, args)
	if err == nil {
		return 0
	}
	if e, ok := errors.AsType[*cli.Error](err); ok && e.Usage {
		return std.err.FailUsage(err, debug, c)
	}
	return std.err.Fail(err, debug)
}
