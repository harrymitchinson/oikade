package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/simhome"
)

const defaultReplay = 7 * 24 * time.Hour

// runReplay shows what your code would have done: it fetches the host's event log, runs your local
// code against it on a fake clock, and compares that with what the deployed version did. The only
// things it asks the host for are its status, its devices and its log.
func runReplay(ctx context.Context, args []string) error {
	fs := newFlags("replay")
	target := addTarget(fs)
	module := fs.String("module", ".", "Your home's module.")
	types := fs.String("types", "devices.go", "The file oikade types wrote, in your module.")
	since := fs.Duration("since", defaultReplay, "How far back to compare.")
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	if extra := cli.Parse(fs, args); len(extra) > 0 {
		return cli.UsageErrorf("oikade replay takes no %q", extra[0])
	}
	if *since <= 0 {
		return cli.UsageErrorf("--since is how far back to look, so more than zero, not %s", *since)
	}
	file, err := filepath.Abs(filepath.Join(*module, *types))
	if err != nil {
		return err
	}
	path, done, err := buildHome(ctx, *module)
	if err != nil {
		return err
	}
	defer done()
	dir, err := os.MkdirTemp("", "oikade-replay")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	log := filepath.Join(dir, "events")
	names, err := fetchLog(ctx, target.transport(), log, *since)
	if err != nil {
		return err
	}
	result := filepath.Join(dir, "report")
	cmd := exec.CommandContext(ctx, path, wire.RoleReplay, log, file, result)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		if !ranAndFailed(err) {
			return fmt.Errorf("couldn't run your home's replay: %w", err)
		}
		return cli.Wrap(err, "your home's replay failed, as it says above")
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := fmt.Println(string(raw))
		return err
	}
	var report simhome.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return cli.Wrap(err, "your home's replay wrote something this oikade can't read").
			WithHint("build your home against the same version of Oikade as this oikade, %s", cli.Version(version))
	}
	fmt.Print(render(report, time.Local, names))
	return nil
}

// fetchLog writes the host's devices and event log to path, as your binary's replay reads them, and
// returns the devices' names by their keys. The host's answers name a device, and your binary's
// handles have its key, so the file has the keys.
func fetchLog(ctx context.Context, t api.Transport, path string, since time.Duration) (map[string]string, error) {
	keys, names, err := deviceKeys(ctx, t)
	if err != nil {
		return nil, err
	}
	keyOf := func(name string) string {
		if k, ok := keys[name]; ok {
			return k
		}
		return name
	}
	resp, err := api.Call(ctx, t, api.Request{Type: api.TypeStatus})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("asking the host for its devices: %s", resp.Error)
	}
	var devices []simhome.Device
	for _, d := range resp.Status.Devices {
		device := simhome.Device{Name: keyOf(d.Name), Room: d.Room, DeviceTypes: d.DeviceTypes}
		for _, c := range d.Capabilities {
			device.Capabilities = append(device.Capabilities, c.Name)
		}
		devices = append(devices, device)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buffered := bufio.NewWriter(f)
	lines := json.NewEncoder(buffered)
	err = api.Events(ctx, t, api.EventFilter{}, func(log api.EventLog) error {
		return lines.Encode(simhome.ReplayHeader{Log: log, Since: log.Now.Add(-since), Devices: devices})
	}, func(e api.Event) error {
		if e.Device != "" {
			e.Device = keyOf(e.Device)
		}
		return lines.Encode(e)
	})
	if err != nil {
		return nil, err
	}
	return names, buffered.Flush()
}

// deviceKeys are the keys of the devices on record by their names, a live one's over one removed,
// and their names by their keys.
func deviceKeys(ctx context.Context, t api.Transport) (keys, names map[string]string, err error) {
	resp, err := api.Call(ctx, t, api.Request{Type: api.TypeDevices})
	if err != nil {
		return nil, nil, err
	}
	if resp.Error != "" || resp.Registry == nil {
		return nil, nil, fmt.Errorf("asking the host for its devices: %s", resp.Error)
	}
	keys, names = map[string]string{}, map[string]string{}
	for _, removed := range []bool{true, false} {
		for _, d := range resp.Registry.Devices {
			if d.Removed == removed {
				keys[d.Name], names[d.Key()] = d.Key(), d.Name
			}
		}
	}
	return keys, names, nil
}

// render is a report for a person: removed writes with a minus, added with a plus, by time, each
// device named by names, from its key.
func render(r simhome.Report, zone *time.Location, names map[string]string) string {
	var b strings.Builder
	when := func(t time.Time) string { return t.In(zone).Format("Mon 2 Jan 15:04:05") }
	fmt.Fprintf(&b, "Replayed %s to %s: %d changes. The deployed version wrote %d times, your code %d.\n",
		when(r.From), when(r.To), r.Changes, r.Deployed, r.Yours)
	type line struct {
		write simhome.Write
		sign  string
	}
	lines := make([]line, 0, len(r.Removed)+len(r.Added))
	for _, w := range r.Removed {
		lines = append(lines, line{w, "-"})
	}
	for _, w := range r.Added {
		lines = append(lines, line{w, "+"})
	}
	slices.SortStableFunc(lines, func(a, b line) int { return a.write.Time.Compare(b.write.Time) })
	if len(lines) == 0 {
		b.WriteString("No differences.\n")
	}
	for _, l := range lines {
		if name, ok := names[l.write.Device]; ok {
			l.write.Device = name
		}
		fmt.Fprintf(&b, "%s %s %s\n", l.sign, when(l.write.Time), l.write)
	}
	for _, name := range slices.Sorted(maps.Keys(r.Skipped)) {
		fmt.Fprintf(&b, "Skipped %s %d times: a replay makes no outside calls, and has no answer on record\n",
			name, r.Skipped[name])
	}
	named := namedKeys(names)
	for _, n := range r.Notes {
		fmt.Fprintln(&b, "Note:", named.Replace(n))
	}
	return b.String()
}

// namedKeys puts names in place of the hardware keys in a replay's notes, a longer key before one it
// starts with.
func namedKeys(names map[string]string) *strings.Replacer {
	var pairs []string
	for _, key := range slices.SortedFunc(maps.Keys(names), func(a, b string) int { return len(b) - len(a) }) {
		if names[key] != key {
			pairs = append(pairs, key, names[key])
		}
	}
	return strings.NewReplacer(pairs...)
}
