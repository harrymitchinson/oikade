package main

import (
	"fmt"
	"io"
)

// version is the release this binary was built from. Releases set it with
// -ldflags "-X main.version=v0.1.0".
var version = "dev"

func printVersion(w io.Writer) {
	_, _ = fmt.Fprintln(w, version)
}
