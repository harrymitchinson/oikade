package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
)

const clientUsage = "usage: oikade client list | revoke NAME. " +
	"To add one, run oikade-host client add NAME on the host's machine"

// runClient lists and revokes who may call the host's network API.
func runClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	t := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := cli.Parse(fs, args)
	req := api.Request{}
	switch {
	case len(words) == 1 && words[0] == "list":
		req.Type = api.TypeClients
	case len(words) == 2 && words[0] == "revoke":
		name, err := nameOf(ident.Client, words[1])
		if err != nil {
			return err
		}
		req = api.Request{Type: api.TypeClientRevoke, Client: name}
	default:
		return cli.UsageErrorf(clientUsage)
	}
	resp, err := api.Call(ctx, t.transport(), req)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if req.Type == api.TypeClients {
		return printClients(cli.NewPrinter(os.Stdout), cli.NewPrinter(os.Stderr), resp.Clients, *asJSON, time.Now())
	}
	if *asJSON {
		return cli.PrintJSON(struct{}{})
	}
	fmt.Printf("Revoked %s\n", words[1])
	return nil
}

// printClients lists the network API's clients, with when they were created and last used as of
// now, or says there are none and how to add one.
func printClients(stdout, stderr *cli.Printer, clients []api.Client, asJSON bool, now time.Time) error {
	if asJSON {
		return json.NewEncoder(stdout).Encode(append([]api.Client{}, clients...))
	}
	if len(clients) == 0 {
		stderr.Println("No clients. oikade-host client add NAME, on the host's machine, adds one.")
		return nil
	}
	table := cli.Table{Columns: []cli.Column{{Header: nameHeader}, {Header: "CREATED"}, {Header: "LAST USED"}}}
	for _, c := range clients {
		used := "never"
		if !c.LastUsed.IsZero() {
			used = cli.Ago(c.LastUsed, now)
		}
		table.Rows = append(table.Rows, []string{c.Name, cli.Ago(c.Created, now), used})
	}
	stdout.Table(table)
	return nil
}
