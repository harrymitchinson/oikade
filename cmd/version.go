package main

import (
	"flag"
	"fmt"
	"io"
)

// version is the release this binary was built from. Releases set it with
// -ldflags "-X main.version=v0.1.0".
var version = "dev"

// versionFlag adds --version to a role's flags.
func versionFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("version", false, "Print the version and exit.")
}

func printVersion(w io.Writer) {
	_, _ = fmt.Fprintln(w, version)
}
