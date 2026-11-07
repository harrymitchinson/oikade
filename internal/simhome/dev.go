package simhome

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/harrymitchinson/oikade/internal/registry"
)

// ReadTypes reads the devices from the file oikadectl types wrote.
func ReadTypes(path string) ([]Device, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	snapshot, err := registry.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var out []Device
	for _, d := range snapshot.Devices {
		if !d.Removed {
			out = append(out, Device{Name: d.Name, Room: d.Room, Capabilities: d.Capabilities})
		}
	}
	return out, nil
}

const devHelp = `Commands:
  advance <duration>             Move the clock on, as in advance 90m
  set <device> <capability> <value>
                                 Change a device from outside, as a sensor or an app would
  status                         Show every device, and what holds it
  restart                        Crash your automations, and start them again
  help                           Show this
  quit                           Stop`

// Dev runs a simulated home from commands on in, printing what it does to out.
func Dev(ctx context.Context, o Options, in io.Reader, out io.Writer) error {
	sim, err := New(o)
	if err != nil {
		return err
	}
	defer sim.Close()
	printed := 0
	report := func() {
		events := sim.Events()
		for _, e := range events[printed:] {
			_, _ = fmt.Fprintf(out, "%s %s\n", e.Time.In(sim.location).Format("Mon 15:04"), e.Text)
		}
		printed = len(events)
	}
	_, _ = fmt.Fprintf(out, "Simulating %d devices from %s. Type help for the commands.\n", len(o.Devices), sim.Now().Format("Mon 15:04"))
	report()

	lines := make(chan string)
	go func() {
		defer close(lines)
		scan := bufio.NewScanner(in)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case line, more := <-lines:
			if !more {
				return nil
			}
			done, err := sim.command(strings.Fields(line), out)
			if err != nil {
				_, _ = fmt.Fprintln(out, "Error:", err)
			}
			report()
			if done {
				return nil
			}
		}
	}
}

// command carries out one command, and says whether that's the end.
func (s *Sim) command(words []string, out io.Writer) (bool, error) {
	if len(words) == 0 {
		return false, nil
	}
	switch args := words[1:]; {
	case words[0] == "quit" || words[0] == "exit":
		return true, nil
	case words[0] == "help":
		_, _ = fmt.Fprintln(out, devHelp)
	case words[0] == "advance" && len(args) == 1:
		d, err := time.ParseDuration(args[0])
		if err != nil {
			return false, err
		}
		if d < 0 {
			return false, fmt.Errorf("the clock only goes forward, not %s", d)
		}
		return false, s.Advance(d)
	case words[0] == "set" && len(args) == 3:
		return false, s.Set(args[0], args[1], parseValue(args[2]))
	case words[0] == "restart" && len(args) == 0:
		return false, s.Restart()
	case words[0] == "status" && len(args) == 0:
		s.status(out)
	default:
		return false, fmt.Errorf("that isn't a command: %s. Type help for the commands", strings.Join(words, " "))
	}
	return false, nil
}

func parseValue(s string) any {
	switch s {
	case "true", "on":
		return true
	case "false", "off":
		return false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func (s *Sim) status(out io.Writer) {
	_, _ = fmt.Fprintln(out, s.Now().Format("Mon 15:04"))
	for _, d := range s.devices {
		for _, name := range d.Capabilities {
			v, ok := s.Value(d.Name, name)
			if !ok {
				_, _ = fmt.Fprintf(out, "  %s %s: not reported\n", d.Name, name)
				continue
			}
			line := fmt.Sprintf("  %s %s: %v", d.Name, name, v)
			i := slices.IndexFunc(s.Claims(d.Name, name), func(c Claim) bool { return c.Winner })
			if i >= 0 {
				line += ", held by " + s.Claims(d.Name, name)[i].Source
			}
			_, _ = fmt.Fprintln(out, line)
		}
	}
}
