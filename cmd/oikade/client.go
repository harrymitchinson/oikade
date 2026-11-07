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

const clientUsage = "usage: oikade client list | add NAME [--token] | revoke NAME"

// runClient manages who may call the host's network API.
func runClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	t := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	token := fs.Bool("token", false,
		"Print a token for the client to send, such as CI, rather than a code to log in with.")
	words := parse(fs, args)
	req := api.Request{}
	switch {
	case len(words) == 1 && words[0] == "list":
		req.Type = api.TypeClients
	case len(words) == 2 && words[0] == "add":
		req = api.Request{Type: api.TypeClientAdd, Client: words[1], Token: *token}
	case len(words) == 2 && words[0] == "revoke":
		req = api.Request{Type: api.TypeClientRevoke, Client: words[1]}
	default:
		return errors.New(clientUsage)
	}
	var transport api.Transport
	if req.Type == api.TypeClientAdd {
		transport = t.local()
	} else {
		transport = t.transport()
	}
	resp, err := api.Call(ctx, transport, req)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	switch req.Type {
	case api.TypeClients:
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
	case api.TypeClientAdd:
		if !*token {
			if *asJSON {
				return printJSON(struct {
					Name    string    `json:"name"`
					Code    string    `json:"code"`
					Expires time.Time `json:"expires"`
					Pin     string    `json:"pin"`
					Address string    `json:"address"`
				}{words[1], resp.Code, resp.Expires, resp.Pin, resp.Address})
			}
			fmt.Printf("On %s, by %s, run:\n  oikade login %s --pin %s --code %s\n",
				words[1], resp.Expires.Local().Format(time.TimeOnly), resp.Address, resp.Pin, resp.Code)
			fmt.Println("The code works once. If the host's name doesn't reach it from there, use its address.")
			return nil
		}
		if resp.Pin == "" {
			_, _ = fmt.Fprintln(os.Stderr,
				"The host has no network API identity, so this token has nothing to reach yet: its log says why")
		}
		if *asJSON {
			return printJSON(struct {
				Name  string `json:"name"`
				Token string `json:"token"`
				Pin   string `json:"pin"`
			}{words[1], resp.Token, resp.Pin})
		}
		fmt.Printf("Token for %s. It isn't shown again, and it can do anything you can:\n  %s\n", words[1], resp.Token)
		fmt.Printf("The host's pin, which a client checks it by:\n  %s\n", resp.Pin)
		return nil
	}
	if *asJSON {
		return printJSON(struct{}{})
	}
	fmt.Printf("Revoked %s\n", words[1])
	return nil
}
