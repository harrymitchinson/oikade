package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// target says which host a command is for.
type target struct {
	fs                     *flag.FlagSet
	dataDir, host, context *string
}

func addTarget(fs *flag.FlagSet) target {
	return target{
		fs:      fs,
		dataDir: fs.String("data-dir", defaultDataDir, "The host's data directory, on this machine."),
		host:    fs.String("host", "", "Run this on that SSH host, using your SSH config."),
		context: fs.String("context", "", "The host to reach over its network API, from oikade login."),
	}
}

// transport is --host, then --context, then the socket if --data-dir is given, then the current
// context, then the socket.
func (t target) transport() api.Transport {
	if *t.host != "" {
		return api.SSH{Host: *t.host, DataDir: *t.dataDir}
	}
	given := false
	t.fs.Visit(func(f *flag.Flag) { given = given || f.Name == "data-dir" })
	if given && *t.context == "" {
		return api.Local{DataDir: *t.dataDir}
	}
	cfg, err := loadConfig()
	if err != nil {
		return failing{err}
	}
	name := *t.context
	if name == "" {
		name = cfg.Current
	}
	if name == "" {
		return api.Local{DataDir: *t.dataDir}
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		return failing{fmt.Errorf("there's no context called %s: oikade login makes one", name)}
	}
	return remote(c)
}

// parse takes flags from anywhere among the positional arguments. A negative number is a value, not
// a flag.
func parse(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for len(args) > 0 {
		i := slices.IndexFunc(args, negative)
		if i < 0 {
			return append(positional, parseFlags(fs, args)...)
		}
		positional = append(append(positional, parseFlags(fs, args[:i])...), args[i])
		args = args[i+1:]
	}
	return positional
}

func parseFlags(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func negative(arg string) bool {
	_, err := strconv.ParseFloat(arg, 64)
	return err == nil && strings.HasPrefix(arg, "-")
}

func printJSON(v any) error {
	return json.NewEncoder(os.Stdout).Encode(v)
}

func runStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	devices := parse(fs, args)
	resp, err := api.Call(ctx, target.transport(), api.Request{Type: api.TypeStatus})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	st := resp.Status
	if st == nil {
		return errors.New("the host sent no status")
	}
	if len(devices) > 0 {
		st.Devices = slices.DeleteFunc(st.Devices, func(d api.Device) bool { return !slices.Contains(devices, d.Name) })
		if len(st.Devices) == 0 {
			return fmt.Errorf("no device %s", strings.Join(devices, ", "))
		}
	}
	if *asJSON {
		return printJSON(st)
	}
	printStatus(os.Stdout, st, len(devices) == 0)
	return nil
}

func printStatus(out io.Writer, st *api.Status, summary bool) {
	if summary {
		process := "down"
		if st.Process {
			process = "connected"
		}
		_, _ = fmt.Fprintf(out, "Version %s, automations %s\n\n", or(st.Version, "unknown"), process)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, d := range st.Devices {
		for _, c := range d.Capabilities {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", d.Name, c.Name, display(c.Value), why(c))
		}
	}
	_ = w.Flush()
	if !summary {
		return
	}
	_, _ = fmt.Fprintln(out)
	for _, r := range st.Rules {
		line := []string{r.Name, or(r.Kind, "gone")}
		if r.Safety {
			line = append(line, "Safety")
		}
		if r.Paused {
			line = append(line, "paused")
		}
		_, _ = fmt.Fprintln(out, strings.Join(line, "  "))
	}
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func display(v any) any {
	if v == nil {
		return "-"
	}
	return v
}

// why says who has a capability.
func why(c api.Capability) string {
	if c.Level == "" {
		return ""
	}
	for _, claim := range c.Claims {
		if claim.Winner {
			return fmt.Sprintf("%s wants %v at %s", claim.Source, display(claim.Value), claim.Level)
		}
	}
	return "no claims"
}

func runTail(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print a line of JSON for each event, whose fields stay stable for scripts.")
	_ = fs.Parse(args)
	return api.Tail(ctx, target.transport(), func(e api.Event) error {
		if *asJSON {
			return printJSON(e)
		}
		_, err := fmt.Println(e.Time.Local().Format("15:04:05"), describe(e))
		return err
	})
}

func describe(e api.Event) string {
	switch e.Type {
	case api.EventChange:
		return fmt.Sprintf("%s %s = %v", e.Device, e.Capability, display(e.Value))
	case api.EventRule:
		return fmt.Sprintf("rule %s %s", e.Rule, e.Result)
	case api.EventPause:
		return fmt.Sprintf("rule %s %s", e.Rule, e.Message)
	case api.EventWrite:
		return fmt.Sprintf("wrote %s %s = %v", e.Device, e.Capability, display(e.Value))
	case api.EventDeploy:
		return "deployed " + e.Message
	}
	return "automations " + e.Message
}

func runSet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := parse(fs, args)
	if len(words) != 3 {
		return errors.New("usage: oikade set DEVICE CAPABILITY VALUE")
	}
	resp, err := api.Call(ctx, target.transport(), api.Request{
		Type: api.TypeSet, Device: words[0], Capability: words[1], Value: parseValue(words[2]),
	})
	if err != nil {
		return err
	}
	return done(resp, *asJSON, fmt.Sprintf("Set %s %s to %s", words[0], words[1], words[2]))
}

// parseValue reads a value from the command line. The host checks that it suits the capability.
func parseValue(s string) any {
	switch s {
	case "true", "on":
		return true
	case "false", "off":
		return false
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n
	}
	return s
}

func runPause(ctx context.Context, args []string, pause bool) error {
	name, kind, did := "resume", api.TypeResume, "Resumed"
	if pause {
		name, kind, did = "pause", api.TypePause, "Paused"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	allow := fs.Bool("allow-safety", false, "Pause a Safety rule. Its Safety claims stay.")
	words := parse(fs, args)
	if len(words) != 1 {
		return fmt.Errorf("usage: oikade %s RULE", name)
	}
	resp, err := api.Call(ctx, target.transport(), api.Request{Type: kind, Rule: words[0], AllowSafety: *allow})
	if err != nil {
		return err
	}
	if resp.NeedsAllowSafety {
		fmt.Fprintln(os.Stderr, "Pass --allow-safety to pause it anyway.")
	}
	return done(resp, *asJSON, did+" "+words[0])
}

// done prints the outcome of a command that changes something.
func done(resp api.Response, asJSON bool, message string) error {
	if asJSON {
		out := struct {
			OK    bool     `json:"ok"`
			Error string   `json:"error,omitempty"`
			Moved []string `json:"moved,omitempty"`
		}{resp.Error == "", resp.Error, resp.Moved}
		if err := printJSON(out); err != nil {
			return err
		}
	} else if resp.Error == "" {
		fmt.Println(message)
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

// runProxy joins stdin and stdout to the API socket. It's the far end of oikade --host.
func runProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "The host's data directory.")
	_ = fs.Parse(args)
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(*dataDir, api.SocketName))
	if err != nil {
		return fmt.Errorf("reaching the host: %w", err)
	}
	defer func() { _ = conn.Close() }()
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		// The other end has gone. A call under way may still answer, and a tail stops here.
		if u, ok := conn.(*net.UnixConn); ok {
			_ = u.CloseWrite()
		}
	}()
	_, err = io.Copy(os.Stdout, conn)
	return err
}
