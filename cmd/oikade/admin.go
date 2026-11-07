package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"golang.org/x/term"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// currentVersion is this binary's: what a release's -ldflags said, or what go install recorded.
func currentVersion() string {
	if version != devVersion {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// publishedVersion is the version go can fetch of what this binary is, or main if it's a local build.
func publishedVersion() string {
	if v := currentVersion(); strings.HasPrefix(v, "v") && !strings.Contains(v, "+") {
		return v
	}
	return "main"
}

func runBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	t := addTarget(fs)
	_ = fs.Parse(args)
	resp, err := api.Call(ctx, t.transport(), api.Request{Type: api.TypeBackup})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	fmt.Println(resp.Backup)
	return nil
}

func runSecret(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secret", flag.ExitOnError)
	t := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := parse(fs, args)
	req := api.Request{}
	switch {
	case len(words) == 1 && words[0] == "list":
		req.Type = api.TypeSecrets
	case len(words) == 2 && words[0] == "set":
		value, err := readSecret(os.Stdin)
		if err != nil {
			return err
		}
		req = api.Request{Type: api.TypeSecretSet, Secret: words[1], SecretValue: value}
	case len(words) == 2 && words[0] == "remove":
		req = api.Request{Type: api.TypeSecretRemove, Secret: words[1]}
	default:
		return errors.New("usage: oikade secret list | set NAME | remove NAME")
	}
	resp, err := api.Call(ctx, t.transport(), req)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if req.Type != api.TypeSecrets {
		if !*asJSON {
			fmt.Println("Done. Your process and plugins read it when they next ask, so one that reads it once needs a restart")
			return nil
		}
		return printJSON(struct{}{})
	}
	if *asJSON {
		return printJSON(resp.Secrets)
	}
	for _, s := range resp.Secrets {
		fmt.Println(s.Name)
	}
	return nil
}

// readSecret takes a value from a pipe, or asks for it without echoing. Never from an argument,
// which would leave it in ps and the shell's history.
func readSecret(in *os.File) (string, error) {
	var value []byte
	var err error
	if term.IsTerminal(int(in.Fd())) {
		fmt.Fprint(os.Stderr, "Value: ")
		value, err = term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(os.Stderr)
	} else {
		value, err = io.ReadAll(in)
	}
	if err != nil {
		return "", err
	}
	text := strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r")
	if text == "" {
		return "", errors.New("the value is empty")
	}
	return text, nil
}
