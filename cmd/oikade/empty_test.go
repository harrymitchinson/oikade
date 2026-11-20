package main

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/registry"
)

func TestEmptyListsSayWhatWouldFillThem(t *testing.T) {
	for _, tc := range []struct {
		name  string
		print func(stdout, stderr *bytes.Buffer, asJSON bool) error
		hint  string
	}{
		{"secrets", func(stdout, stderr *bytes.Buffer, asJSON bool) error {
			return printSecrets(cli.Plain(stdout, 0), cli.Plain(stderr, 0), nil, nil, asJSON, time.Now())
		}, "No secrets. oikade set secret/NAME adds one."},
		{"clients", func(stdout, stderr *bytes.Buffer, asJSON bool) error {
			return printClients(cli.Plain(stdout, 0), cli.Plain(stderr, 0), nil, asJSON, time.Now())
		}, "No clients. oikade-host add client/NAME, on the host's machine, adds one."},
	} {
		var stdout, stderr bytes.Buffer
		if err := tc.print(&stdout, &stderr, false); err != nil {
			t.Fatal(err)
		}
		if stdout.Len() != 0 || strings.TrimSpace(stderr.String()) != tc.hint {
			t.Errorf("%s: stdout %q, stderr %q", tc.name, stdout.String(), stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		if err := tc.print(&stdout, &stderr, true); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != "[]\n" || stderr.Len() != 0 {
			t.Errorf("%s --json: stdout %q, stderr %q", tc.name, stdout.String(), stderr.String())
		}
	}
}

func TestNoDevicesSaysWhatReportsThem(t *testing.T) {
	var stderr bytes.Buffer
	noDevices(&stderr, 0)
	if !strings.Contains(stderr.String(), "A plugin your home imports reports its devices") ||
		strings.Contains(stderr.String(), "--all") {
		t.Errorf("got %q", stderr.String())
	}
	stderr.Reset()
	noDevices(&stderr, 2)
	got := stderr.String()
	if !strings.Contains(got, "2 forgotten devices keep their") || !strings.Contains(got, "oikade list devices --all") {
		t.Errorf("with forgotten devices: %q", stderr.String())
	}
	raw, err := json.Marshal(listed(registry.Snapshot{}, deviceMarks{}))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[]" {
		t.Errorf("--json: %s", raw)
	}
}

func TestStatusWithNothingDeployedNamesDeploy(t *testing.T) {
	var stdout, stderr bytes.Buffer
	printStatus(cli.Plain(&stdout, 0), cli.Plain(&stderr, 0), &api.Status{}, false, time.Now())
	if !strings.Contains(stderr.String(), "No home deployed. Run oikade deploy from your home's module.") {
		t.Errorf("stderr %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "unknown") {
		t.Errorf("stdout %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	deploying := &api.Status{Home: api.Home{Deploying: true}}
	printStatus(cli.Plain(&stdout, 0), cli.Plain(&stderr, 0), deploying, false, time.Now())
	if !strings.Contains(stdout.String(), "while a deploy is under way") || stderr.Len() != 0 {
		t.Errorf("during a deploy: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestStatusSaysWhenAutomationsWentDown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	now := time.Date(2026, 11, 18, 14, 0, 0, 0, time.Local)
	at := now.Add(-3 * time.Minute)
	st := &api.Status{Home: api.Home{Version: "v3", Since: &at}}
	printStatus(cli.Plain(&stdout, 0), cli.Plain(&stderr, 0), st, false, now)
	if want := "Home v3 · automations went down 3 min ago\n"; !strings.HasPrefix(stdout.String(), want) {
		t.Errorf("stdout %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "oikade tail") {
		t.Errorf("stderr %q", stderr.String())
	}
}

func TestStatusStartsWithWhatsLeftToSetUp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	st := &api.Status{Devices: []api.Device{
		{Name: "attic-fan", Problem: "no key", Capabilities: []api.Capability{{Name: "on"}}},
		{Name: "desk-lamp", Capabilities: []api.Capability{{Name: "on"}}},
		{Name: "shed-relay", PluginNotImported: true, Capabilities: []api.Capability{{Name: "on"}}},
	}, Matter: &api.Matter{Serving: true}}
	printStatus(cli.Plain(&stdout, 0), cli.Plain(&stderr, 0), st, false, time.Now())
	want := []string{
		"No home deployed. Run oikade deploy from your home's module.",
		"The Matter bridge has no controller. oikade status matter says how to pair one.",
		"Devices with a problem: attic-fan. Their STATE says what's wrong.",
		"Devices whose plugin isn't imported: shed-relay. " +
			"Import it in your home and run oikade deploy, or forget its devices with oikade forget --plugin NAME.",
	}
	if got := strings.Split(strings.TrimSpace(stderr.String()), "\n"); !slices.Equal(got, want) {
		t.Errorf("stderr:\n%s\nwant:\n%s", stderr.String(), strings.Join(want, "\n"))
	}
	if !strings.Contains(stdout.String(), "attic-fan") {
		t.Errorf("stdout %q", stdout.String())
	}

	// Asked about a device, it says only what it was asked.
	stderr.Reset()
	refs, err := statusRefs([]string{"desk-lamp"})
	if err != nil {
		t.Fatal(err)
	}
	printNamed(cli.Plain(io.Discard, 0), cli.Plain(&stderr, 0), st, refs, noEvents, false, time.Now())
	if stderr.Len() != 0 {
		t.Errorf("for a device: %q", stderr.String())
	}
}

func TestAHomeFullySetUpHasNothingLeftToSetUp(t *testing.T) {
	st := &api.Status{Home: api.Home{Version: "v3", Process: true}, Devices: []api.Device{
		{Name: "desk-lamp", Capabilities: []api.Capability{{Name: "on"}}},
	}, Plugins: []api.Plugin{{Name: "kasa", Devices: []string{"desk-lamp"}}}}
	st.Matter = &api.Matter{Serving: true, Fabrics: []api.Fabric{{Index: 1}}}
	if gaps := setupGaps(st); len(gaps) != 0 {
		t.Errorf("got %q", gaps)
	}
	// With Matter turned off there's no bridge to pair, and during a deploy, no home to ask for.
	if gaps := setupGaps(&api.Status{Home: api.Home{Deploying: true}}); len(gaps) != 0 {
		t.Errorf("Matter off, during a deploy: %q", gaps)
	}
}

// A secret that isn't set comes before the plugins' problems, which it often explains.
func TestASecretNotSetSaysHowToSetIt(t *testing.T) {
	st := &api.Status{Home: api.Home{Version: "v3", Process: true, UnsetSecrets: []string{"cozytouch"}},
		Plugins: []api.Plugin{{Name: "cozytouch", Problem: "waiting for the secret cozytouch"}}}
	got := setupGaps(st)
	want := []string{
		"Secret cozytouch isn't set: oikade set secret/cozytouch", "Plugin cozytouch: waiting for the secret cozytouch",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

// A bridge that's down can't say whether it's paired, so it says it's down instead.
func TestABridgeThatsDownSaysSoRatherThanUnpaired(t *testing.T) {
	st := &api.Status{Home: api.Home{Version: "v3", Process: true},
		Matter: &api.Matter{Problem: "port 5540 is taken", Fabrics: []api.Fabric{}}}
	got := setupGaps(st)
	want := []string{"The Matter bridge is down: port 5540 is taken. oikade status matter says more."}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
	st.Matter = &api.Matter{}
	got = setupGaps(st)
	if !slices.Equal(got, []string{"The Matter bridge is down. oikade status matter says more."}) {
		t.Errorf("with no problem: %q", got)
	}
}

func TestManyDevicesAreCut(t *testing.T) {
	got := names([]string{"a", "b", "c", "d", "e", "f", "g"})
	if got != "a, b, c, d, e and 2 more" {
		t.Errorf("got %q", got)
	}
}

// A plugin that says what's wrong with it, such as a refused login, says so before its devices do.
func TestAPluginsProblemIsLeftToSetUp(t *testing.T) {
	st := &api.Status{Home: api.Home{Version: "v3", Process: true}, Plugins: []api.Plugin{
		{Name: "blinds", Problem: "hub unreachable"},
		{Name: "thermostats", Problem: `login refused, check the secret "thermostats"`},
	}, Devices: []api.Device{{Name: "study-heater", Problem: "not found"}}}
	got := setupGaps(st)
	want := []string{
		"Plugin blinds: hub unreachable",
		`Plugin thermostats: login refused, check the secret "thermostats"`,
		"Devices with a problem: study-heater. Their STATE says what's wrong.",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestAPluginThatNeedsItsSetupSaysHowToRunIt(t *testing.T) {
	st := &api.Status{Home: api.Home{Version: "v3", Process: true},
		Plugins: []api.Plugin{{Name: "ewelink-lan", NeedsSetup: true}}}
	got := setupGaps(st)
	want := []string{"Plugin ewelink-lan needs setup: run oikade setup ewelink-lan"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

// A deployed home with no devices says what would report them. A deploy restarting the automations
// isn't news, and a device's state says each thing that's wrong with it.
func TestStatusSaysWhatsMissingAndOnlyThat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	now := time.Now()
	out, errs := cli.Plain(&stdout, 0), cli.Plain(&stderr, 0)
	printStatus(out, errs, &api.Status{Home: api.Home{Version: "v3", Process: true}}, false, now)
	if !strings.Contains(stderr.String(), "No devices.") {
		t.Errorf("no devices: stderr %q", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	printStatus(out, errs, &api.Status{Home: api.Home{Version: "v3", Deploying: true}}, false, now)
	if !strings.HasPrefix(stdout.String(), "Home v3 · automations down · a deploy is under way\n") || stderr.Len() != 0 {
		t.Errorf("during a deploy: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	off := false
	both := api.Device{PluginNotImported: true, Problem: "not seen on the LAN", Online: &off, Declare: "home.Device(…)"}
	if got := deviceState(both); got != "plugin not imported, not seen on the LAN, not in your code" {
		t.Errorf("deviceState = %q", got)
	}
}
