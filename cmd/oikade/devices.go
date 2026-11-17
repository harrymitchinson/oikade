package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/registry"
)

const devicesUsage = "usage: oikade devices [remove DEVICE | remove --plugin NAME]. " +
	"A device's name, room and type are your home's code's: home.Device declares them"

// verbRemove is the one verb --plugin goes with.
const verbRemove = "remove"

func runDevices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	all := fs.Bool("all", false, "List removed devices too, which keep their Matter endpoints.")
	allow := fs.Bool("allow-safety", false, "Remove a device under a Safety claim.")
	plugin := fs.String("plugin", "", "Remove every device of this plugin, such as one your home no longer imports.")
	words := cli.Parse(fs, args)
	if len(words) == 0 {
		return listDevices(ctx, target, *asJSON, *all)
	}

	verb, words := words[0], words[1:]
	words, err := namesOf(ident.Device, words)
	if err != nil {
		return err
	}
	if *plugin, err = nameOf(ident.Plugin, *plugin); err != nil {
		return err
	}
	if *plugin != "" && verb != verbRemove {
		return errors.New("--plugin is only for oikade devices remove")
	}
	req := api.Request{AllowSafety: *allow}
	var message string
	switch {
	case verb == verbRemove && len(words) <= 1 && (len(words) == 1) == (*plugin == ""): // A device, or --plugin.
		req.Type, req.Plugin = api.TypeDeviceRemove, *plugin
		if *plugin == "" {
			req.Device = words[0]
			message = fmt.Sprintf("Removed %s, and ended its claims. "+
				"If it's still paired, it comes back when it next reports", words[0])
		}
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
	if req.Plugin != "" && resp.Error == "" {
		message = fmt.Sprintf("Removed %s, every device of %s. If %s is imported again, they come back when they next report",
			strings.Join(resp.Removed, ", "), req.Plugin, req.Plugin)
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
	removed := 0
	if !all {
		before := len(list.Devices)
		list.Devices = slices.DeleteFunc(list.Devices, func(d registry.Device) bool { return d.Removed })
		removed = before - len(list.Devices)
	}
	marks := deviceMarks{unimported: map[string]bool{}, hidden: map[string]bool{}, problems: resp.Problems}
	for _, u := range resp.Unimported {
		marks.unimported[u.Device] = true
	}
	for _, name := range resp.HiddenFromMatter {
		marks.hidden[name] = true
	}
	if asJSON {
		return cli.PrintJSON(listed(list, marks))
	}
	if len(list.Devices) == 0 {
		noDevices(os.Stderr, removed)
		return nil
	}
	printUndeclared(os.Stderr, printDevices(os.Stdout, list, marks))
	return nil
}

// printUndeclared gives the lines that would declare the devices your home doesn't, which it needs
// only to name, place or type them.
func printUndeclared(out io.Writer, lines []string) {
	if len(lines) == 0 {
		return
	}
	slices.Sort(lines)
	_, _ = fmt.Fprintln(out, "To name, place or type a device that's not in your code, declare it in your home:")
	for _, l := range lines {
		_, _ = fmt.Fprintln(out, "  "+l)
	}
}

// deviceMarks are what the host says of devices beyond the registry, by name.
type deviceMarks struct {
	// unimported are those whose plugin the running version doesn't import, and hidden those your
	// home keeps off Matter.
	unimported, hidden map[string]bool
	// problems are what's wrong with them, as their drivers say.
	problems map[string]string
}

// noDevices says what would put devices on record, and that --all shows any removed ones.
func noDevices(stderr io.Writer, removed int) {
	_, _ = fmt.Fprintln(stderr, "No devices. A plugin your home imports reports its devices once you deploy it.")
	if removed > 0 {
		_, _ = fmt.Fprintf(stderr,
			"%d removed devices keep their Matter endpoints: oikade devices --all lists them.\n", removed)
	}
}

const (
	// notImported is the state of a device whose plugin the running version doesn't import.
	notImported = "plugin not imported"
	// hiddenFromMatter is the state of a device your home keeps off Matter.
	hiddenFromMatter = "hidden from Matter"
	// notInYourCode is the state of a device your home doesn't declare.
	notInYourCode = "not in your code"
)

// printDevices prints the table, and returns the lines that would declare the devices that are
// marked not in your code.
func printDevices(out io.Writer, list registry.Snapshot, marks deviceMarks) (undeclared []string) {
	slices.SortFunc(list.Devices, func(a, b registry.Device) int { return strings.Compare(a.Name, b.Name) })
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tROOM\tSTATE\tHARDWARE\tENDPOINT\tDEVICE TYPES\tCAPABILITIES")
	for _, d := range list.Devices {
		state := "paired"
		switch {
		case d.Removed:
			state = "removed"
		case d.Reserved():
			state = "reserved"
		case marks.unimported[d.Name]:
			state = notImported
		case marks.problems[d.Name] != "":
			state = marks.problems[d.Name]
		}
		if !d.Removed && marks.hidden[d.Name] {
			if state == "paired" {
				state = hiddenFromMatter
			} else {
				state += ", " + hiddenFromMatter
			}
		}
		if line, ok := registry.Undeclared(d); ok {
			state += ", " + notInYourCode
			undeclared = append(undeclared, line)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", d.Name, or(d.Room, "-"), state,
			or(registry.HardwareRef(d).String(), hardwareOf(d)), d.Endpoint,
			or(devicetype.Join(d.DeviceTypes()), "-"), strings.Join(d.Capabilities, ", "))
	}
	_ = w.Flush()
	return undeclared
}

// hardwareOf names hardware that isn't a plugin's, such as a simulated device's, by its driver and
// ID, or "-" for a reserved name.
func hardwareOf(d registry.Device) string {
	if d.Driver == "" {
		return "-"
	}
	return registry.DescribeHardware(d.Driver, d.ID)
}

// listedDevice is a device as oikade devices --json prints it: as on record, with what it is.
type listedDevice struct {
	registry.Device
	// Ref is its ref, such as device/desk-plug.
	Ref string `json:"ref"`
	// Hardware is its hardware's ref, such as hardware/zigbee2mqtt/0x00158d0001a2b3c4, for a device a
	// plugin reports.
	Hardware    string            `json:"hardware,omitempty"`
	DeviceTypes []devicetype.Type `json:"deviceTypes"`
	// PluginNotImported is a device whose plugin the running version doesn't import.
	PluginNotImported bool `json:"pluginNotImported,omitempty"`
	// Problem is what's wrong with it, as its driver says.
	Problem string `json:"problem,omitempty"`
	// Matter is false for a device your home keeps off Matter, and left out otherwise.
	Matter *bool `json:"matter,omitempty"`
}

// removedHasNone is a device's problem, which a removed one doesn't have.
func removedHasNone(d registry.Device, problems map[string]string) string {
	if d.Removed {
		return ""
	}
	return problems[d.Name]
}

// listed is the registry as oikade devices --json prints it.
func listed(list registry.Snapshot, marks deviceMarks) any {
	devices := make([]listedDevice, 0, len(list.Devices))
	for _, d := range list.Devices {
		l := listedDevice{
			Device: d, Ref: ident.DeviceRef(d.Name).String(), Hardware: registry.HardwareRef(d).String(),
			DeviceTypes:       append([]devicetype.Type{}, d.DeviceTypes()...),
			PluginNotImported: !d.Removed && marks.unimported[d.Name],
			Problem:           removedHasNone(d, marks.problems),
		}
		if !d.Removed && marks.hidden[d.Name] {
			l.Matter = new(bool)
		}
		devices = append(devices, l)
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
	module := fs.String("module", ".", "Your home's module, whose declarations name its devices.")
	if extra := cli.Parse(fs, args); len(extra) > 0 {
		return fmt.Errorf("oikade types takes no %q", extra[0])
	}
	list, err := registryOf(ctx, target)
	if err != nil {
		return err
	}
	list = declaredIn(ctx, os.Stderr, *module, list, func(ctx context.Context) (wire.Manifest, error) {
		return homeManifest(ctx, *module)
	})
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

// declaredIn is the host's devices as your home's declarations would make them once it's deployed,
// so that a name you declare has its handle before the deploy that gives it. A home that doesn't
// build, as when it already names that handle, has its declarations read from its source.
func declaredIn(ctx context.Context, stderr io.Writer, module string, list registry.Snapshot,
	manifest func(context.Context) (wire.Manifest, error),
) registry.Snapshot {
	if _, err := os.Stat(filepath.Join(module, "go.mod")); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s isn't a Go module, so devices are under the host's names, not your declarations': "+
			"pass your home's with --module\n", module)
		return list
	}
	_, _ = fmt.Fprintln(stderr, "Building your home for its device declarations")
	var inventory []wire.DeviceDeclaration
	if m, err := manifest(ctx); err == nil {
		inventory = m.Inventory
	} else {
		if inventory, err = declarationsInSource(module); err != nil {
			_, _ = fmt.Fprintf(stderr, "Your home doesn't build, and its source can't be read, so devices are under "+
				"the host's names: %v\n", err)
			return list
		}
		_, _ = fmt.Fprintf(stderr, "Your home doesn't build, so its %d device declarations are read from its source\n",
			len(inventory))
	}
	declared, err := registry.Declaring(list, wire.Declarations(inventory))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "The host would refuse your declarations, so devices are under its names: %v\n", err)
		return list
	}
	return declared
}

// printTakenOffMatter says which devices a deploy took off Matter, which controllers have lost with
// their rooms and scenes.
func printTakenOffMatter(out io.Writer, devices []string) {
	if len(devices) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "Took %s off Matter, as your home hides them: controllers lost their rooms and scenes\n",
		strings.Join(devices, ", "))
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
