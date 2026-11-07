package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/registry"
)

const devicesUsage = "usage: oikade devices [name DEVICE NAME | room DEVICE ROOM | reserve NAME | " +
	"replace DEVICE WITH | remove DEVICE | restore FILE]"

// typesReminder is what a change to a device's name leaves for you to do.
const typesReminder = "Run oikade types and deploy, because running code has lost track of it until then."

func runDevices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	all := fs.Bool("all", false, "List removed devices too, which keep their Matter endpoints.")
	room := fs.String("room", "", "The room of a reserved name.")
	allow := fs.Bool("allow-safety", false,
		"Remove a device under a Safety claim, or change a name while a Safety rule runs.")
	force := fs.Bool("force", false, "Restore a file that disagrees with the registry.")
	words := cli.Parse(fs, args)
	if len(words) == 0 {
		return listDevices(ctx, target, *asJSON, *all)
	}

	verb, words := words[0], words[1:]
	req := api.Request{AllowSafety: *allow, Force: *force}
	var message string
	switch {
	case verb == "name" && len(words) == 2:
		req.Type, req.Device, req.To = api.TypeDeviceName, words[0], words[1]
		message = fmt.Sprintf("Renamed %s to %s. %s", words[0], words[1], typesReminder)
	case verb == "room" && len(words) == 2:
		req.Type, req.Device, req.Room = api.TypeDeviceRoom, words[0], words[1]
		message = fmt.Sprintf("Moved %s to %s", words[0], or(words[1], "no room"))
	case verb == "reserve" && len(words) == 1:
		req.Type, req.Device, req.Room = api.TypeDeviceReserve, words[0], *room
		message = fmt.Sprintf("Reserved %s", words[0])
	case verb == "replace" && len(words) == 2:
		req.Type, req.Device, req.To = api.TypeDeviceReplace, words[0], words[1]
		message = fmt.Sprintf("Moved %s onto %s's hardware. %s", words[0], words[1], typesReminder)
	case verb == "remove" && len(words) == 1:
		req.Type, req.Device = api.TypeDeviceRemove, words[0]
		message = fmt.Sprintf("Removed %s. If it's still paired, it comes back when it next reports", words[0])
	case verb == "restore" && len(words) == 1:
		file, err := os.ReadFile(words[0])
		if err != nil {
			return err
		}
		snapshot, err := registry.Parse(file)
		if err != nil {
			return fmt.Errorf("%s: %w", words[0], err)
		}
		req.Type, req.Registry = api.TypeDeviceRestore, &snapshot
		message = fmt.Sprintf("Restored %d devices", len(snapshot.Devices))
	default:
		return errors.New(devicesUsage)
	}
	resp, err := api.Call(ctx, target.transport(), req)
	if err != nil {
		return err
	}
	if resp.NeedsAllowSafety {
		fmt.Fprintln(os.Stderr, "Pass --allow-safety to go ahead anyway.")
	}
	if resp.NeedsForce {
		fmt.Fprintln(os.Stderr, "Pass --force to make the registry match the file.")
	}
	for _, name := range resp.Moved {
		fmt.Fprintln(os.Stderr, name, "isn't in the file, and the file has its name or Matter endpoint, so it has a new one")
	}
	return done(resp, *asJSON, message)
}

func registryOf(ctx context.Context, target target) (registry.Snapshot, error) {
	resp, err := api.Call(ctx, target.transport(), api.Request{Type: api.TypeDevices})
	if err != nil {
		return registry.Snapshot{}, err
	}
	if resp.Error != "" {
		return registry.Snapshot{}, errors.New(resp.Error)
	}
	if resp.Registry == nil {
		return registry.Snapshot{}, errors.New("the host sent no devices")
	}
	return *resp.Registry, nil
}

func listDevices(ctx context.Context, target target, asJSON, all bool) error {
	list, err := registryOf(ctx, target)
	if err != nil {
		return err
	}
	if !all {
		list.Devices = slices.DeleteFunc(list.Devices, func(d registry.Device) bool { return d.Removed })
	}
	if asJSON {
		return cli.PrintJSON(list)
	}
	printDevices(os.Stdout, list)
	return nil
}

func printDevices(out io.Writer, list registry.Snapshot) {
	slices.SortFunc(list.Devices, func(a, b registry.Device) int { return strings.Compare(a.Name, b.Name) })
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tROOM\tSTATE\tDRIVER\tID\tENDPOINT\tCAPABILITIES")
	for _, d := range list.Devices {
		state := "paired"
		switch {
		case d.Removed:
			state = "removed"
		case d.Reserved():
			state = "reserved"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", d.Name, or(d.Room, "-"), state, or(d.Driver, "-"),
			or(d.ID, "-"), d.Endpoint, strings.Join(d.Capabilities, ", "))
	}
	_ = w.Flush()
}

func runTypes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("types", flag.ExitOnError)
	target := addTarget(fs)
	out := fs.String("out", "devices.go", "The file to write, or - for standard output.")
	pkg := fs.String("package", "main", "The file's Go package.")
	if extra := cli.Parse(fs, args); len(extra) > 0 {
		return fmt.Errorf("oikade types takes no %q", extra[0])
	}
	list, err := registryOf(ctx, target)
	if err != nil {
		return err
	}
	file, err := registry.Render(list, *pkg)
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err = os.Stdout.Write(file)
		return err
	}
	return os.WriteFile(*out, file, 0o644)
}
