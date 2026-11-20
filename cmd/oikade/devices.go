package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/registry"
)

// nameHeader is the header of a table's first column, of what each row is, and stateHeader of a
// column that says how each is.
const (
	nameHeader  = "NAME"
	stateHeader = "STATE"
)

// verbRemove removes what the host owns outright, as forget drops what a plugin reports.
const verbRemove = "remove"

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

// andList is "A", "A and B", or "A, B and C".
func andList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
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

// deviceMarks are what the host says of devices beyond the registry, by name: those whose plugin the
// running version doesn't import, and those your home keeps off Matter.
type deviceMarks struct{ unimported, hidden map[string]bool }

// noDevices says what would put devices on record, and that --all shows any forgotten ones.
func noDevices(stderr io.Writer, forgotten int) {
	_, _ = fmt.Fprintln(stderr, "No devices. A plugin your home imports reports its devices once you deploy it.")
	switch {
	case forgotten == 1:
		_, _ = fmt.Fprintln(stderr, "A forgotten device keeps its Matter endpoint: oikade list devices --all lists it.")
	case forgotten > 1:
		_, _ = fmt.Fprintf(stderr,
			"%d forgotten devices keep their Matter endpoints: oikade list devices --all lists them.\n", forgotten)
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

// printDevices prints the table, fitted to out's width unless wide, and returns the lines that would
// declare the devices that are marked not in your code, and the columns that didn't fit. A device's
// STATE is its record's, and never what's wrong with it now, which is oikade status's.
func printDevices(out *cli.Printer, list registry.Snapshot, marks deviceMarks, wide bool,
) (undeclared, dropped []string) {
	list.Devices = slices.Clone(list.Devices)
	slices.SortFunc(list.Devices, func(a, b registry.Device) int { return strings.Compare(a.Name, b.Name) })
	table := cli.Table{
		Columns: []cli.Column{
			{Header: nameHeader},
			{Header: "ROOM"},
			{Header: stateHeader},
			{Header: "DEVICE TYPES", Drop: 3, Shrink: true},
			{Header: "CAPABILITIES", Shrink: true},
			{Header: "PLUGIN"},
			{Header: "HARDWARE", Drop: 1, Dim: true},
			{Header: "ENDPOINT", Drop: 2},
		},
		Wide: wide,
	}
	for _, d := range list.Devices {
		state := "paired"
		switch {
		case d.Removed:
			state = "forgotten"
		case d.Reserved():
			state = "reserved"
		case marks.unimported[d.Name]:
			state = notImported
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
		endpoint := ""
		if d.Endpoint != 0 {
			endpoint = strconv.Itoa(int(d.Endpoint))
		}
		plugin, _ := api.PluginOf(d)
		table.Rows = append(table.Rows, []string{d.Name, d.Room, state, devicetype.Join(d.DeviceTypes()),
			strings.Join(d.Capabilities, ", "), plugin, or(registry.HardwareRef(d).String(), hardwareOf(d)), endpoint})
	}
	return undeclared, out.Table(table)
}

// hardwareOf names hardware that isn't a plugin's, such as a simulated device's, by its driver and
// ID, or nothing for a reserved name.
func hardwareOf(d registry.Device) string {
	if d.Driver == "" {
		return ""
	}
	return registry.DescribeHardware(d.Driver, d.ID)
}

// listedDevice is a device as oikade list devices --json prints it: as on record, with what it is.
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
	// Matter is false for a device your home keeps off Matter, and left out otherwise.
	Matter *bool `json:"matter,omitempty"`
}

// listed is the devices on record as oikade list devices --json prints them: an array, each with its
// ref.
func listed(list registry.Snapshot, marks deviceMarks) []listedDevice {
	devices := make([]listedDevice, 0, len(list.Devices))
	for _, d := range list.Devices {
		l := listedDevice{
			Device: d, Ref: ident.DeviceRef(d.Name).String(), Hardware: registry.HardwareRef(d).String(),
			DeviceTypes:       append([]devicetype.Type{}, d.DeviceTypes()...),
			PluginNotImported: !d.Removed && marks.unimported[d.Name],
		}
		if !d.Removed && marks.hidden[d.Name] {
			l.Matter = new(bool)
		}
		devices = append(devices, l)
	}
	return devices
}

func runTypes(ctx context.Context, args []string) error {
	fs := newFlags("types")
	target := addTarget(fs)
	out := fs.String("out", "devices.go", "The file to write, or - for standard output.")
	pkg := fs.String("package", "main", "The file's Go package.")
	module := fs.String("module", ".", "Your home's module, whose declarations name its devices.")
	if extra := cli.Parse(fs, args); len(extra) > 0 {
		return cli.UsageErrorf("oikade types takes no %q", extra[0])
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
// to forget them. They stay, so that a plugin briefly missing loses nothing.
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
				_, _ = fmt.Fprintf(out, "    A Safety claim holds %s, and forgetting it ends that claim\n", u.Device)
			}
		}
		_, _ = fmt.Fprintf(out, "    Forget them with oikade forget --plugin %s\n", plugin)
	}
}
