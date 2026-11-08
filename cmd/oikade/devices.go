package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/registry"
)

const devicesUsage = "usage: oikade devices [name DEVICE NAME | room DEVICE ROOM | type DEVICE TYPE | reserve NAME | " +
	"replace DEVICE WITH | remove DEVICE | remove --plugin NAME | restore FILE]"

// verbRemove is the one verb --plugin goes with.
const verbRemove = "remove"

// typesReminder is what a change to a device's name leaves for you to do.
const typesReminder = "Run oikade types and deploy, because running code has lost track of it until then."

func runDevices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	all := fs.Bool("all", false, "List removed devices too, which keep their Matter endpoints.")
	room := fs.String("room", "", "The room of a reserved name.")
	allow := fs.Bool("allow-safety", false,
		"Remove a device under a Safety claim, change a name while a Safety rule runs, or change a type "+
			"that takes a device out of a Safety rule's room.")
	force := fs.Bool("force", false, "Restore a file that disagrees with the registry.")
	plugin := fs.String("plugin", "", "Remove every device of this plugin, such as one your home no longer imports.")
	words := cli.Parse(fs, args)
	if len(words) == 0 {
		return listDevices(ctx, target, *asJSON, *all)
	}

	verb, words := words[0], words[1:]
	if *plugin != "" && verb != verbRemove {
		return errors.New("--plugin is only for oikade devices remove")
	}
	req := api.Request{AllowSafety: *allow, Force: *force}
	var message string
	switch {
	case verb == "name" && len(words) == 2:
		req.Type, req.Device, req.To = api.TypeDeviceName, words[0], words[1]
		message = fmt.Sprintf("Renamed %s to %s. %s", words[0], words[1], typesReminder)
	case verb == "room" && len(words) == 2:
		req.Type, req.Device, req.Room = api.TypeDeviceRoom, words[0], words[1]
		message = fmt.Sprintf("Moved %s to %s", words[0], or(words[1], "no room"))
	case verb == "type" && len(words) == 2:
		req.Type, req.Device, req.DeviceType = api.TypeDeviceType, words[0], devicetype.Type(words[1])
		message = fmt.Sprintf("%s is %s", words[0], or(words[1], "what its driver or its capabilities say"))
	case verb == "reserve" && len(words) == 1:
		req.Type, req.Device, req.Room = api.TypeDeviceReserve, words[0], *room
		message = fmt.Sprintf("Reserved %s", words[0])
	case verb == "replace" && len(words) == 2:
		req.Type, req.Device, req.To = api.TypeDeviceReplace, words[0], words[1]
		message = fmt.Sprintf("Moved %s onto %s's hardware. %s", words[0], words[1], typesReminder)
	case verb == verbRemove && len(words) <= 1 && (len(words) == 1) == (*plugin == ""): // A device, or --plugin.
		req.Type, req.Plugin = api.TypeDeviceRemove, *plugin
		if *plugin == "" {
			req.Device = words[0]
			message = fmt.Sprintf("Removed %s. If it's still paired, it comes back when it next reports", words[0])
		}
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
	if req.Plugin != "" && resp.Error == "" {
		message = fmt.Sprintf("Removed %s, every device of %s. If %s is imported again, they come back when they next report",
			strings.Join(resp.Removed, ", "), req.Plugin, req.Plugin)
	}
	for _, name := range resp.Moved {
		fmt.Fprintln(os.Stderr, name, "isn't in the file, and the file has its name or Matter endpoint, so it has a new one")
	}
	return done(resp, *asJSON, message)
}

func registryOf(ctx context.Context, target target) (registry.Snapshot, error) {
	resp, err := devicesOf(ctx, target.transport())
	if err != nil {
		return registry.Snapshot{}, err
	}
	return *resp.Registry, nil
}

// devicesOf answers a devices request, which has the registry.
func devicesOf(ctx context.Context, t api.Transport) (api.Response, error) {
	resp, err := api.Call(ctx, t, api.Request{Type: api.TypeDevices})
	if err != nil {
		return api.Response{}, err
	}
	if resp.Error != "" {
		return api.Response{}, errors.New(resp.Error)
	}
	if resp.Registry == nil {
		return api.Response{}, errors.New("the host sent no devices")
	}
	return resp, nil
}

func listDevices(ctx context.Context, target target, asJSON, all bool) error {
	resp, err := devicesOf(ctx, target.transport())
	if err != nil {
		return err
	}
	list := *resp.Registry
	if !all {
		list.Devices = slices.DeleteFunc(list.Devices, func(d registry.Device) bool { return d.Removed })
	}
	unimported := map[string]bool{}
	for _, u := range resp.Unimported {
		unimported[u.Device] = true
	}
	if asJSON {
		return cli.PrintJSON(listed(list, unimported))
	}
	printDevices(os.Stdout, list, unimported)
	return nil
}

// notImported is the state of a device whose plugin the running version doesn't import.
const notImported = "plugin not imported"

func printDevices(out io.Writer, list registry.Snapshot, unimported map[string]bool) {
	slices.SortFunc(list.Devices, func(a, b registry.Device) int { return strings.Compare(a.Name, b.Name) })
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tROOM\tSTATE\tDRIVER\tID\tENDPOINT\tDEVICE TYPES\tCAPABILITIES")
	for _, d := range list.Devices {
		state := "paired"
		switch {
		case d.Removed:
			state = "removed"
		case d.Reserved():
			state = "reserved"
		case unimported[d.Name]:
			state = notImported
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", d.Name, or(d.Room, "-"), state, or(d.Driver, "-"),
			or(d.ID, "-"), d.Endpoint, or(devicetype.Join(d.DeviceTypes()), "-"), strings.Join(d.Capabilities, ", "))
	}
	_ = w.Flush()
}

// listedDevice is a device as oikade devices --json prints it: as on record, with what it is.
type listedDevice struct {
	registry.Device
	DeviceTypes []devicetype.Type `json:"deviceTypes"`
	// PluginNotImported is a device whose plugin the running version doesn't import.
	PluginNotImported bool `json:"pluginNotImported,omitempty"`
}

// listed is the registry as oikade devices --json prints it.
func listed(list registry.Snapshot, unimported map[string]bool) any {
	devices := make([]listedDevice, 0, len(list.Devices))
	for _, d := range list.Devices {
		devices = append(devices, listedDevice{
			Device: d, DeviceTypes: append([]devicetype.Type{}, d.DeviceTypes()...),
			PluginNotImported: !d.Removed && unimported[d.Name],
		})
	}
	// Devices here is shallower than the Snapshot's, so it's the one encoding/json writes, and any
	// other field of a Snapshot comes along.
	return struct {
		registry.Snapshot
		Devices []listedDevice `json:"devices"`
	}{list, devices}
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

// printUnimported says which devices are left on record by plugins a version doesn't import, and how
// to remove them. They stay, so that a plugin briefly missing loses nothing.
func printUnimported(out io.Writer, devices []api.Unimported) {
	if len(devices) == 0 {
		return
	}
	byPlugin := map[string][]api.Unimported{}
	for _, u := range devices {
		byPlugin[u.Plugin] = append(byPlugin[u.Plugin], u)
	}
	_, _ = fmt.Fprintln(out, "These devices stay on record, offline, because your home doesn't import their plugin:")
	for _, plugin := range slices.Sorted(maps.Keys(byPlugin)) {
		var names []string
		for _, u := range byPlugin[plugin] {
			names = append(names, u.Device)
		}
		_, _ = fmt.Fprintf(out, "  %s: %s\n", plugin, strings.Join(names, ", "))
		for _, u := range byPlugin[plugin] {
			if u.Safety {
				_, _ = fmt.Fprintf(out, "    A Safety claim holds %s, and removing it ends that claim\n", u.Device)
			}
		}
		_, _ = fmt.Fprintf(out, "    Remove them with oikade devices remove --plugin %s\n", plugin)
	}
}
