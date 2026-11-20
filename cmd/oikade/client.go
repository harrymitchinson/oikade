package main

import (
	"encoding/json"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// printClients lists the network API's clients, with when they were created and last used as of
// now, and those waiting for their login, or says there are none and how to add one.
func printClients(stdout, stderr *cli.Printer, clients []api.Client, asJSON bool, now time.Time) error {
	if asJSON {
		return json.NewEncoder(stdout).Encode(append([]api.Client{}, clients...))
	}
	if len(clients) == 0 {
		stderr.Println("No clients. oikade-host add client/NAME, on the host's machine, adds one.")
		return nil
	}
	table := cli.Table{Columns: []cli.Column{{Header: nameHeader}, {Header: "CREATED"}, {Header: "LAST USED"}}}
	waiting := false
	for _, c := range clients {
		used := "never"
		switch {
		case !c.Expires.IsZero():
			used, waiting = "waiting for its login, until "+c.Expires.Local().Format("15:04"), true
		case !c.LastUsed.IsZero():
			used = cli.Ago(c.LastUsed, now)
		}
		table.Rows = append(table.Rows, []string{c.Name, cli.Ago(c.Created, now), used})
	}
	stdout.Table(table)
	if waiting {
		stderr.Hint("A waiting client is added once oikade login uses its code, and oikade remove client/NAME cancels it")
	}
	return nil
}
