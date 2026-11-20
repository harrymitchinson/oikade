package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// Each error a person is likely to meet is one sentence in their words, then what fixes it, with the
// exit code a script can tell apart: 2 for the wrong arguments, 1 for the rest.
func TestErrorsReadAsSentences(t *testing.T) {
	noHost := t.TempDir() // A data directory with no host in it.
	empty := t.TempDir()  // A directory with no home in it.
	for _, tc := range []struct {
		args []string
		want string
		code int
	}{
		// set checks its value before it says it's setting it.
		{[]string{"set", "salon", "on", "maybe"}, "Error: device/salon/on takes true or false, not maybe\n", 1},
		{
			[]string{"set", "device/salon/setpoint", "warm"},
			"Error: device/salon/setpoint takes a number, such as 25, not warm\n",
			1,
		},
		{[]string{"set", "salon", "setpoint", "90"}, "Error: device/salon/setpoint takes 0°C to 50°C, not 90\n", 1},
		{
			[]string{"set", "salon", "mode", "off"},
			"Error: device/salon/mode takes auto, heat, cool, dry or fan, not off\n",
			1,
		},
		{
			[]string{"set", "salon", "temperature", "3"},
			"Error: device/salon/temperature is read-only: salon reports it, and nothing sets it\n",
			1,
		},
		// A ref says what it got, and the forms the command takes.
		{
			[]string{"pause", "device/salon/on"},
			"Error: device/salon/on isn't a rule's ref: oikade pause takes a case's, case/DEVICE/CAPABILITY/CASE, " +
				"or a flow's, flow/FLOW\n",
			2,
		},
		{[]string{"resume", "device/salon/on"}, "Error: device/salon/on isn't a rule's ref: oikade resume takes", 2},
		{
			[]string{"pause", "case/Salon/on/night"},
			`Error: "case/Salon/on/night" isn't a ref: "Salon" isn't a device name`,
			2,
		},
		{
			[]string{"status", "secret/overkiz"},
			"Error: oikade status doesn't take secret/overkiz\n",
			2,
		},
		{
			[]string{"set", "dev/salon/on", "true"},
			"Error: dev/salon/on isn't a capability's ref: give one as device/DEVICE/CAPABILITY",
			2,
		},
		// No host, and no home.
		{
			[]string{"status", "--data-dir", noHost},
			"Error: no host is running on this machine with its data in " + noHost + "\n" +
				"Hint: start one with oikade-host, pass --data-dir DIR for another, " +
				"or --context NAME for a host you've logged in to\n",
			1,
		},
		{
			[]string{"replay", "--module", empty},
			"Error: there's no home in " + empty + ": a home is a Go module, with a go.mod\n" +
				"Hint: cd into your home's module, or pass --module DIR. oikade init starts one\n",
			1,
		},
		{[]string{"check", "--module", empty}, "Error: there's no home in " + empty, 1},
		{[]string{"test", "--module", empty}, "Error: there's no home in " + empty, 1},
		{[]string{"dev", "--module", empty}, "Error: there's no home in " + empty, 1},
		{[]string{"deploy", "--module", empty}, "Error: there's no home in " + empty, 1},
		{
			[]string{"replay", "--since", "-1h", "--module", empty},
			"Error: --since is how far back to look, so more than zero, not -1h0m0s\n",
			2,
		},
	} {
		args := tc.args
		if args[0] != "test" && args[0] != "dev" { // Neither reaches a host.
			args = append(args, "--data-dir", noHost)
		}
		_, stderr, code := runOikade(t, cli.Plain, args...)
		if code != tc.code || !strings.HasPrefix(stderr, tc.want) {
			t.Errorf("oikade %s exits %d:\n%s\nwant %d, starting:\n%s",
				strings.Join(tc.args, " "), code, stderr, tc.code, tc.want)
		}
		if strings.Contains(stderr, "Setting") || strings.Contains(stderr, "dial unix") {
			t.Errorf("oikade %s:\n%s", strings.Join(tc.args, " "), stderr)
		}
	}
}

func TestAHostOutOfReachSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		transport api.Transport
		err       error
		want      string
	}{
		{
			api.Local{DataDir: "/var/lib/oikade"},
			&os.PathError{Op: "dial", Path: "/var/lib/oikade/api.sock", Err: os.ErrPermission},
			"Error: you may not reach this machine's host: its socket, /var/lib/oikade/api.sock, is the host's user's\n" +
				"Hint: run it with sudo, or pass --context NAME for a host you've logged in to\n"},
		{api.Local{DataDir: "/srv/oikade"}, syscall.ECONNREFUSED,
			"Error: no host is running on this machine with its data in /srv/oikade\n"},
		{api.Remote{Address: "pi.home:8443"}, fmt.Errorf("the host's pin is sha256:a, not sha256:b: %w", api.ErrOtherHost),
			"Error: europa, at pi.home:8443, isn't the host you logged in to: its key has another pin\n" +
				"Hint: oikade login it again, with the pin oikade-host add prints on it\n"},
		{api.Remote{Address: "pi.home:8443"}, &net.DNSError{Err: "no such host", Name: "pi.home", IsNotFound: true},
			"Error: couldn't reach europa, at pi.home:8443\n" +
				"Hint: check the host is running there with --api-addr, and that this machine can reach it\n"},
		// Anything else, such as a cancel, is as it was.
		{api.Remote{Address: "pi.home:8443"}, context.Canceled, "Error: context canceled\n"},
		{api.Remote{Address: "pi.home:8443"}, errors.New("no pin for the host"), "Error: no pin for the host\n"},
	} {
		var b strings.Builder
		cli.Plain(&b, 0).Fail(unreachable(tc.transport, "europa", tc.err), false)
		if !strings.HasPrefix(b.String(), tc.want) {
			t.Errorf("%v:\n%s\nwant:\n%s", tc.err, b.String(), tc.want)
		}
	}
}

// A home's module may be above --module, as go build finds it.
func TestAHomeIsAnyDirectoryInAModule(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "cmd", "home")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := requireHome(sub); err != nil {
		t.Errorf("%s is in a module: %v", sub, err)
	}
	if err := requireHome(t.TempDir()); err == nil {
		t.Error("a directory in no module is a home")
	}
}
