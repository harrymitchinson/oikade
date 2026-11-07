package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/simhome"
)

// defaultReplay is "last week".
const defaultReplay = 7 * 24 * time.Hour

// runReplay shows what your code would have done: it fetches the host's event log, runs your local
// code against it on a fake clock, and compares that with what the deployed version did. The only
// things it asks the host for are its status and its log.
func runReplay(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	target := addTarget(fs)
	module := fs.String("module", ".", "Your home's module.")
	types := fs.String("types", "devices.go", "The file oikade types wrote, in your module.")
	since := fs.Duration("since", defaultReplay, "How far back to compare.")
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	if extra := cli.Parse(fs, args); len(extra) > 0 {
		return fmt.Errorf("oikade replay takes no %q", extra[0])
	}
	if *since <= 0 {
		return fmt.Errorf("--since must be more than zero, not %s", *since)
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
	if err := fetchLog(ctx, target.transport(), log, *since); err != nil {
		return err
	}
	result := filepath.Join(dir, "report")
	cmd := exec.CommandContext(ctx, path, wire.RoleReplay, log, file, result)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("replaying your home: %w", err)
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
		return fmt.Errorf("reading the replay: %w", err)
	}
	fmt.Print(render(report, time.Local))
	return nil
}

// fetchLog writes the host's log to a file, as your binary reads it.
func fetchLog(ctx context.Context, t api.Transport, path string, since time.Duration) error {
	resp, err := api.Call(ctx, t, api.Request{Type: api.TypeStatus})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("asking the host for its devices: %s", resp.Error)
	}
	var devices []simhome.Device
	for _, d := range resp.Status.Devices {
		device := simhome.Device{Name: d.Name, Room: d.Room, DeviceTypes: d.DeviceTypes}
		for _, c := range d.Capabilities {
			device.Capabilities = append(device.Capabilities, c.Name)
		}
		devices = append(devices, device)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	buffered := bufio.NewWriter(f)
	lines := json.NewEncoder(buffered)
	err = api.Events(ctx, t, func(log api.EventLog) error {
		return lines.Encode(simhome.ReplayHeader{Log: log, Since: log.Now.Add(-since), Devices: devices})
	}, func(e api.Event) error { return lines.Encode(e) })
	if err != nil {
		return err
	}
	return buffered.Flush()
}

// render is a report for a person: removed writes with a minus, added with a plus, by time.
func render(r simhome.Report, zone *time.Location) string {
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
		fmt.Fprintf(&b, "%s %s %s\n", l.sign, when(l.write.Time), l.write)
	}
	for _, name := range slices.Sorted(maps.Keys(r.Skipped)) {
		fmt.Fprintf(&b, "Skipped %s %d times: a replay makes no outside calls, and has no answer on record\n",
			name, r.Skipped[name])
	}
	for _, n := range r.Notes {
		fmt.Fprintln(&b, "Note:", n)
	}
	return b.String()
}
