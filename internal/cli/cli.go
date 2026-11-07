// Package cli is what oikade and oikade-host share as commands: how they read their arguments,
// print JSON and say their version, and where a host keeps its state.
package cli

import (
	"encoding/json"
	"flag"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
)

// DefaultDataDir is where a host keeps its state, unless told otherwise. oikade finds a host on
// its own machine there.
const DefaultDataDir = "/var/lib/oikade"

// DevVersion is a build no release or go install put a version on.
const DevVersion = "dev"

// Version is a binary's: stamped, what a release's -ldflags "-X main.version=..." said, or what go
// install recorded.
func Version(stamped string) string {
	if stamped != DevVersion {
		return stamped
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return stamped
}

// Parse takes flags from anywhere among the positional arguments, and returns the positional ones. A
// negative number is a value, not a flag.
func Parse(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for len(args) > 0 {
		i := slices.IndexFunc(args, negative)
		if i < 0 {
			return append(positional, ParseFlags(fs, args)...)
		}
		positional = append(append(positional, ParseFlags(fs, args[:i])...), args[i])
		args = args[i+1:]
	}
	return positional
}

// ParseFlags takes flags from anywhere among the arguments, and returns the rest as they are.
func ParseFlags(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func negative(arg string) bool {
	_, err := strconv.ParseFloat(arg, 64)
	return err == nil && strings.HasPrefix(arg, "-")
}

// PrintJSON writes v to stdout, as one line.
func PrintJSON(v any) error {
	return json.NewEncoder(os.Stdout).Encode(v)
}
