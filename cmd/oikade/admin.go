package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// publishedVersion is the version go can fetch of what this binary is, or main if it's a local build.
func publishedVersion() string {
	if v := cli.Version(version); strings.HasPrefix(v, "v") && !strings.Contains(v, "+") {
		return v
	}
	return "main"
}

func runBackup(ctx context.Context, args []string) error {
	fs := newFlags("backup")
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
	fs := newFlags("secret")
	t := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := cli.Parse(fs, args)
	if len(words) == 2 {
		name, err := nameOf(ident.Secret, words[1])
		if err != nil {
			return err
		}
		words[1] = name
	}
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
		return cli.UsageErrorf("oikade secret takes list, set NAME, or remove NAME")
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
		return cli.PrintJSON(struct{}{})
	}
	return printSecrets(std.out, std.err, resp.Secrets, *asJSON)
}

// printSecrets lists the secrets' names, or says there are none and how to add one.
func printSecrets(stdout, stderr *cli.Printer, secrets []api.Secret, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(stdout).Encode(append([]api.Secret{}, secrets...))
	}
	if len(secrets) == 0 {
		stderr.Println("No secrets. oikade secret set NAME adds one.")
		return nil
	}
	// Names alone, with no header, so that a script can read them a line each.
	for _, s := range secrets {
		stdout.Println(s.Name)
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
