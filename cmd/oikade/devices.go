package main

import (
	"cmp"
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

	"oikade.dev/internal/capability"
	"oikade.dev/internal/cli"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/registry"
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

// deviceMarks are what the host says of devices beyond the registry, by their hardware: those whose
// plugin the running version doesn't import, those your home keeps off Matter, and those its code
// declares.
type deviceMarks struct{ unimported, hidden, declared map[string]bool }

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

// What a STATE column says of a device, or of a plugin that isn't imported.
const (
	notImported      = "plugin not imported"
	hiddenFromMatter = "hidden from Matter"
	notInYourCode    = "not in your code"
)

// printDevices prints the table, fitted to out's width unless wide, and returns the refs of the
// devices marked not in your code, and the columns that didn't fit. A device's STATE is its
// record's, and never what's wrong with it now, which is oikade status's.
func printDevices(out *cli.Printer, list registry.Snapshot, marks deviceMarks, wide bool,
) (undeclared, dropped []string) {
	list.Devices = slices.Clone(list.Devices)
	slices.SortFunc(list.Devices, func(a, b registry.Device) int {
		return strings.Compare(ident.DeviceRef(a.Key()).String(), ident.DeviceRef(b.Key()).String())
	})
	table := cli.Table{
		Columns: []cli.Column{
			{Header: "DEVICE", Shrink: true, ShrinkLast: true},
			{Header: "APP NAME", Shrink: true},
			{Header: "ROOM"},
			{Header: stateHeader},
			{Header: "DEVICE TYPES", Drop: 2, Shrink: true},
			{Header: "CAPABILITIES", Drop: 3, Shrink: true},
			{Header: "ENDPOINT", Drop: 1},
		},
		Wide: wide,
	}
	for _, d := range list.Devices {
		state := "paired"
		switch {
		case d.Removed:
			state = "forgotten"
		case marks.unimported[d.Key()]:
			state = notImported
		}
		if !d.Removed && marks.hidden[d.Key()] {
			if state == "paired" {
				state = hiddenFromMatter
			} else {
				state += ", " + hiddenFromMatter
			}
		}
		if !d.Removed && !marks.declared[d.Key()] {
			state += ", " + notInYourCode
			undeclared = append(undeclared, ident.DeviceRef(d.Key()).String())
		}
		endpoint := ""
		if d.Endpoint != 0 {
			endpoint = strconv.Itoa(int(d.Endpoint))
		}
		table.Rows = append(table.Rows, []string{ident.DeviceRef(d.Key()).String(), d.AppName, d.Room, state,
			devicetype.Join(d.DeviceTypes()), strings.Join(d.Capabilities, ", "), endpoint})
	}
	return undeclared, out.Table(table)
}

// listedDevice is a device as oikade list devices --json prints it: as on record, with what it is.
type listedDevice struct {
	registry.Device
	// Ref is its ref, its hardware's, such as hardware/zigbee2mqtt/0x00158d0001a2b3c4, and Hardware what
	// its line in your code says it is, as zigbee2mqtt/0x00158d0001a2b3c4.
	Ref         string            `json:"ref"`
	Hardware    string            `json:"hardware"`
	DeviceTypes []devicetype.Type `json:"deviceTypes"`
	// Declared is a device a line in your home's code declares.
	Declared bool `json:"declared,omitempty"`
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
			Device: d, Ref: ident.DeviceRef(d.Key()).String(), Hardware: d.Key(),
			DeviceTypes:       append([]devicetype.Type{}, d.DeviceTypes()...),
			Declared:          marks.declared[d.Key()],
			PluginNotImported: !d.Removed && marks.unimported[d.Key()],
		}
		if !d.Removed && marks.hidden[d.Key()] {
			l.Matter = new(bool)
		}
		devices = append(devices, l)
	}
	return devices
}

func runTypes(ctx context.Context, args []string) error {
	fs := newFlags("types")
	target := addTarget(fs)
	out := fs.String("out", "devices.go",
		"Your devices file, in your module, which it adds to, or - to print devices.go as it would be.")
	pkg := fs.String("package", "main", "A new file's Go package.")
	module := fs.String("module", ".", "Your home's module, whose code names its devices.")
	if extra := cli.Parse(fs, args); len(extra) > 0 {
		return cli.UsageErrorf("oikade types takes no %q", extra[0])
	}
	list, err := registryOf(ctx, target)
	if err != nil {
		return err
	}
	path := *out
	if path == "-" {
		path = "devices.go"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(*module, path)
	}
	file, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cli.Errorf("couldn't read %s: %v", path, err)
	}
	if len(file) == 0 {
		file = nil
	}
	replaced := generatedByOldTypes(file)
	if replaced {
		*pkg, file = packageOf(file, *pkg), nil
	}
	others, err := packageFiles(path)
	if err != nil {
		return cli.Errorf("couldn't read your home's package: %v", err)
	}
	list = undeclaredIn(ctx, os.Stderr, *module, list, func(ctx context.Context) (wire.Manifest, error) {
		return homeManifest(ctx, *module)
	})
	devices, unknown := toAdd(list)
	result, got, err := addDevices(file, *pkg, others, devices)
	if err != nil {
		return cli.Errorf("couldn't add your devices to %s: %v", path, err).
			WithHint("fix %s, and run oikade types again", path)
	}
	if *out == "-" {
		_, err = os.Stdout.Write(result)
	} else if file == nil || len(got.names) > 0 {
		err = writeAtomically(path, result)
	}
	if err != nil {
		return cli.Errorf("couldn't write %s: %v", path, err)
	}
	printAdded(std.err, path, *out == "-", replaced, got, unknown)
	return nil
}

// toAdd are the devices on record that list, from undeclaredIn, has, which oikade types adds, and the
// hardware it can't, as no device type fits it, or it hasn't reported.
func toAdd(list registry.Snapshot) ([]newDevice, []registry.Device) {
	var devices []newDevice
	var unknown []registry.Device
	for _, d := range list.Devices {
		if d.Removed || d.ID == "" {
			continue
		}
		i := slices.IndexFunc(d.DeviceTypes(), func(t devicetype.Type) bool {
			return !slices.ContainsFunc(devicetype.Has(t), func(c string) bool { return !slices.Contains(d.Capabilities, c) })
		})
		if i < 0 {
			unknown = append(unknown, d)
			continue
		}
		typ := d.DeviceTypes()[i]
		var more []string
		for _, c := range d.Capabilities {
			if _, ok := capability.Lookup(c); ok && !slices.Contains(devicetype.Has(typ), c) && !slices.Contains(more, c) {
				more = append(more, c)
			}
		}
		slices.SortFunc(more, func(a, b string) int {
			sa, _ := capability.Lookup(a)
			sb, _ := capability.Lookup(b)
			return strings.Compare(sa.Go, sb.Go)
		})
		devices = append(devices, newDevice{name: cmp.Or(d.AppName, d.ID), key: d.Key(), room: d.Room, typ: typ, more: more})
	}
	return devices, unknown
}

// count is n things, as 1 device or 2 devices.
func count(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return strconv.Itoa(n) + " " + thing + "s"
}

// printAdded says what oikade types added to path, or would have, when printing it.
func printAdded(p *cli.Printer, path string, printing, replaced bool, got added, unknown []registry.Device) {
	done, add, replace := p.Done, "Added", "Replaced"
	if printing {
		done, add, replace = func(format string, a ...any) { p.Printf(format+"\n", a...) }, "Would add", "Would replace"
	}
	if replaced {
		done("%s %s, which an older oikade types wrote: its devices are lines of yours now", replace, path)
	}
	switch n := len(got.names); {
	case n == 0 && len(unknown) > 0:
		p.Println("Nothing to add to " + path)
	case n == 0:
		p.Println(path + " has every device on record. Pair one, and run oikade types again.")
	default:
		what := count(n, "device")
		if len(got.rooms) > 0 {
			what += " and " + count(len(got.rooms), "room")
		}
		names := ""
		if n <= 5 {
			names = ": " + andList(slices.Sorted(maps.Values(got.names)))
		}
		done("%s %s to %s%s", add, what, path, names)
	}
	if len(got.clashed) > 0 {
		p.Hint("%s had a name that was taken, so it ends with a hash of its hardware: rename it in %s",
			andList(got.clashed), path)
	}
	var unseen, untyped []string
	for _, d := range unknown {
		if len(d.Capabilities) == 0 {
			unseen = append(unseen, d.Key())
		} else {
			untyped = append(untyped, d.Key())
		}
	}
	if len(unseen) > 0 {
		p.Hint("Left out %s, which hasn't reported yet: run oikade types again once it has", andList(unseen))
	}
	if len(untyped) > 0 {
		p.Hint("Left out %s, as no device type fits what it has: name a capability of %s yourself, as "+
			"oikade.Number{Device: %q, Capability: oikade.Battery}", andList(untyped), them(untyped), untyped[0])
	}
}

// undeclaredIn is the host's devices without those your home's code declares, which it names
// itself. A home that doesn't build, as when it already names a device oikade types hasn't written,
// has its declarations read from its source.
func undeclaredIn(ctx context.Context, stderr io.Writer, module string, list registry.Snapshot,
	manifest func(context.Context) (wire.Manifest, error),
) registry.Snapshot {
	if _, err := os.Stat(filepath.Join(module, "go.mod")); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s isn't a Go module, so which devices your code declares can't be read: pass your "+
			"home's with --module\n", module)
		return list
	}
	_, _ = fmt.Fprintln(stderr, "Building your home for its devices")
	var inventory []wire.DeviceDeclaration
	if m, err := manifest(ctx); err == nil {
		inventory = m.Inventory
	} else {
		if inventory, err = declarationsInSource(module); err != nil {
			_, _ = fmt.Fprintf(stderr, "Your home doesn't build, and its source can't be read, so which devices it "+
				"declares can't be: %v\n", err)
			return list
		}
		_, _ = fmt.Fprintf(stderr, "Your home doesn't build, so its %d devices are read from its source\n", len(inventory))
	}
	declared := map[string]bool{}
	for _, d := range inventory {
		declared[d.Hardware] = true
	}
	list.Devices = slices.DeleteFunc(slices.Clone(list.Devices), func(d registry.Device) bool { return declared[d.Key()] })
	return list
}

// printTakenOffMatter says which devices a deploy took off Matter, which controllers have lost with
// their rooms and scenes.
func printTakenOffMatter(out io.Writer, devices []string) {
	if len(devices) == 0 {
		return
	}
	refs := make([]string, 0, len(devices))
	for _, key := range devices {
		refs = append(refs, ident.DeviceRef(key).String())
	}
	_, _ = fmt.Fprintf(out, "Took %s off Matter, as your home hides them: controllers lost their rooms and scenes\n",
		strings.Join(refs, ", "))
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
