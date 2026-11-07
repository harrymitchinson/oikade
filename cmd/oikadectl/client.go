package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

const clientUsage = "usage: oikadectl client list | revoke NAME. " +
	"To add one, run oikade-host client add NAME on the host's machine"

// runClient lists and revokes who may call the host's network API.
func runClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	t := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := parse(fs, args)
	req := api.Request{}
	switch {
	case len(words) == 1 && words[0] == "list":
		req.Type = api.TypeClients
	case len(words) == 2 && words[0] == "revoke":
		req = api.Request{Type: api.TypeClientRevoke, Client: words[1]}
	default:
		return errors.New(clientUsage)
	}
	resp, err := api.Call(ctx, t.transport(), req)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if req.Type == api.TypeClients {
		if *asJSON {
			return printJSON(resp.Clients)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tCREATED\tLAST USED")
		for _, c := range resp.Clients {
			used := "never"
			if !c.LastUsed.IsZero() {
				used = c.LastUsed.Local().Format(time.DateTime)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", c.Name, c.Created.Local().Format(time.DateTime), used)
		}
		return w.Flush()
	}
	if *asJSON {
		return printJSON(struct{}{})
	}
	fmt.Printf("Revoked %s\n", words[1])
	return nil
}
