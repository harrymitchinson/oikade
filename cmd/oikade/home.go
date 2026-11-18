package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/registry"
)

// oikadeModule is what a home imports.
const oikadeModule = "github.com/harrymitchinson/oikade"

// buildHome builds a module for this machine, in a directory that ends with done.
func buildHome(ctx context.Context, module string) (path string, done func(), err error) {
	if err := requireHome(module); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "oikade-home")
	if err != nil {
		return "", nil, err
	}
	path = filepath.Join(dir, "home")
	// As a deploy builds it, so that what passes here builds there.
	if err := build(ctx, module, path, api.Response{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}

// requireHome fails unless module is a Go module, as a home is, saying how to name one.
func requireHome(module string) error {
	if _, err := os.Stat(filepath.Join(module, "go.mod")); err != nil {
		where := "here"
		if module != "." {
			where = "in " + module
		}
		return cli.Wrap(err, "there's no home %s: a home is a Go module, with a go.mod", where).
			WithHint("cd into your home's module, or pass --module DIR. oikade init starts one")
	}
	return nil
}

// runCheck finds what's wrong with a home without touching one: the compiler, then the source, then the
// checks your binary makes of itself, which are the ones a deploy makes.
func runCheck(ctx context.Context, args []string) error {
	fs := newFlags("check")
	target := addTarget(fs)
	module := fs.String("module", ".", "Your home's module.")
	if extra := cli.ParseFlags(fs, args); len(extra) > 0 {
		return cli.UsageErrorf("oikade check takes no %q", extra[0])
	}
	path, done, err := buildHome(ctx, *module)
	if err != nil {
		return err
	}
	defer done()
	if err := checkSource(*module); err != nil {
		return err
	}
	m, err := manifestOf(ctx, path)
	if err != nil {
		return err
	}
	for _, w := range m.Warnings {
		fmt.Fprintln(os.Stderr, "Warning:", w)
	}
	devices, reached := hostDevices(ctx, target.transport())
	if reached {
		if err := checkShown(*devices.Registry, m.HiddenFromMatter); err != nil {
			return err
		}
	}
	fmt.Printf("Your home is fine: %d rules, %d plugins\n", len(m.Rules), len(m.Plugins))
	if !reached {
		return nil
	}
	printUnimported(os.Stderr, api.UnimportedOf(*devices.Registry, m.Plugins, devices.OutsidePlugins,
		func(name string) bool { return slices.Contains(devices.SafetyHeld, name) }))
	printHiding(os.Stderr, *devices.Registry, m.HiddenFromMatter, devices.HiddenFromMatter)
	printUnseen(os.Stderr, *devices.Registry, m.Inventory)
	return nil
}

// homeManifest builds a home and asks it what it is, as a deploy does.
func homeManifest(ctx context.Context, module string) (wire.Manifest, error) {
	path, done, err := buildHome(ctx, module)
	if err != nil {
		return wire.Manifest{}, err
	}
	defer done()
	return manifestOf(ctx, path)
}

// manifestOf asks a built home what it is.
func manifestOf(ctx context.Context, path string) (wire.Manifest, error) {
	cmd := exec.CommandContext(ctx, path, wire.RoleCheck)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return wire.Manifest{}, cli.Wrap(err, "your home failed its own checks, as it says above")
	}
	// A home's own code may have printed first.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var m wire.Manifest
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		return wire.Manifest{}, cli.Wrap(err, "your home's check said something this oikade can't read").
			WithHint("build your home against the same version of Oikade as this oikade, %s", cli.Version(version))
	}
	if m.Protocol != wire.Version {
		return wire.Manifest{}, cli.Errorf("your home was built for protocol %d, and this oikade speaks %d",
			m.Protocol, wire.Version).
			WithHint("build your home against the same version of Oikade as this oikade, %s", cli.Version(version))
	}
	return m, nil
}

// printUnseen warns of the hardware your home declares that the host has never seen, whose name
// waits for it to report.
func printUnseen(out io.Writer, list registry.Snapshot, inventory []wire.DeviceDeclaration) {
	for _, d := range wire.Declarations(inventory) {
		if d.ID == "" || slices.ContainsFunc(list.Devices, func(r registry.Device) bool {
			return r.Driver == d.Driver && r.ID == d.ID && len(r.Capabilities) > 0
		}) {
			continue
		}
		_, _ = fmt.Fprintf(out, "Warning: the host has never seen %s, so %s waits for it to report\n",
			registry.DescribeHardware(d.Driver, d.ID), d.Name)
	}
}

// hostDevices asks the host what's on record, for what a deploy would change there. A home with no
// host to reach skips that, and one that can't be asked says so.
func hostDevices(ctx context.Context, t api.Transport) (api.Response, bool) {
	devices, err := devicesOf(ctx, t)
	if err != nil {
		if local, ok := t.(api.Local); ok {
			if _, statErr := os.Stat(filepath.Join(local.DataDir, api.SocketName)); statErr != nil {
				return api.Response{}, false // There's no host here to ask.
			}
		}
		fmt.Fprintln(os.Stderr, "Couldn't ask the host which devices your home's plugins leave behind:", err)
		return api.Response{}, false
	}
	return devices, true
}

// checkShown finds the devices ShowOnMatter names that aren't that plugin's on record, which would
// otherwise stay hidden without a word.
func checkShown(list registry.Snapshot, hidden []wire.HiddenPlugin) error {
	var errs []error
	for _, h := range hidden {
		for _, name := range h.Shown {
			i := slices.IndexFunc(list.Devices, func(d registry.Device) bool { return d.Name == name })
			if i < 0 || list.Devices[i].Removed || !isPlugins(list.Devices[i], h.Plugin) {
				errs = append(errs, fmt.Errorf("plugin %s: ShowOnMatter names %s, which isn't one of its devices on record "+
					"(oikade devices lists them)", h.Plugin, name))
			}
		}
	}
	return errors.Join(errs...)
}

// isPlugins says whether plugin reports a device on record.
func isPlugins(d registry.Device, plugin string) bool {
	of, ok := api.PluginOf(d)
	return ok && of == plugin
}

// printHiding says which devices a deploy of the home would take off Matter, before it does.
func printHiding(out io.Writer, list registry.Snapshot, hidden []wire.HiddenPlugin, hiddenNow []string) {
	var hiding []string
	for _, name := range api.HiddenDevices(list, hidden) {
		if !slices.Contains(hiddenNow, name) {
			hiding = append(hiding, name)
		}
	}
	if len(hiding) > 0 {
		_, _ = fmt.Fprintf(out, "A deploy hides %s from Matter, and controllers lose their rooms and scenes for them\n",
			strings.Join(hiding, ", "))
	}
}

// runTest runs a home's tests. Anything after the flags goes to go test.
func runTest(ctx context.Context, args []string) error {
	fs := newFlags("test")
	module := fs.String("module", ".", "Your home's module.")
	_ = fs.Parse(args)
	if err := requireHome(*module); err != nil {
		return err
	}
	packages := fs.Args()
	if len(packages) == 0 {
		packages = []string{"./..."}
	}
	cmd := exec.CommandContext(ctx, "go", append([]string{"test"}, packages...)...)
	cmd.Dir = *module
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return cli.Wrap(err, "your tests didn't pass, as go test says above")
	}
	return nil
}

// runDev runs a home against a simulated one, built from the committed types file.
func runDev(ctx context.Context, args []string) error {
	fs := newFlags("dev")
	module := fs.String("module", ".", "Your home's module.")
	types := fs.String("types", "devices.go", "The file oikade types wrote, in your module.")
	if extra := cli.ParseFlags(fs, args); len(extra) > 0 {
		return cli.UsageErrorf("oikade dev takes no %q", extra[0])
	}
	path, done, err := buildHome(ctx, *module)
	if err != nil {
		return err
	}
	defer done()
	file, err := filepath.Abs(filepath.Join(*module, *types))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, wire.RoleDev, file)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// runInit starts a home in a directory that hasn't one.
func runInit(ctx context.Context, args []string) error {
	fs := newFlags("init")
	name := fs.String("module", "home", "Your module's path.")
	source := fs.String("source", "", "A checkout of Oikade to build against, rather than a published version.")
	extra := cli.ParseFlags(fs, args)
	if len(extra) > 1 {
		return cli.UsageErrorf("oikade init takes one directory, not %q", extra[1])
	}
	dir := "."
	if len(extra) == 1 {
		dir = extra[0]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files, err := homeFiles()
	if err != nil {
		return err
	}
	written := append(slices.Collect(maps.Keys(files)), "go.mod", "go.sum")
	for _, path := range written {
		if _, err := os.Stat(filepath.Join(dir, path)); err == nil {
			return fmt.Errorf("%s already has a %s", dir, path)
		}
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			return err
		}
	}
	if *source != "" {
		err = moduleFromSource(dir, *name, *source)
	} else {
		err = moduleFromRelease(ctx, os.Stderr, dir, *name)
	}
	if err != nil {
		// So that the same command can be run again.
		for _, path := range written {
			_ = os.Remove(filepath.Join(dir, path))
		}
		// Only if they're empty: these may have been here already.
		_ = os.Remove(filepath.Join(dir, ".github", "workflows"))
		_ = os.Remove(filepath.Join(dir, ".github"))
		return err
	}
	slices.Sort(written)
	if dir == "." {
		fmt.Printf("Started a home here: %s\n", strings.Join(written, ", "))
		fmt.Println("Next: oikade test, then oikade dev")
	} else {
		fmt.Printf("Started a home in %s: %s\n", dir, strings.Join(written, ", "))
		fmt.Printf("Next: cd %s, then oikade test and oikade dev\n", dir)
	}
	return nil
}

// moduleFromRelease writes a go.mod on the Oikade this binary is, or main if it's no release. Go's own
// output reaches stderr only when a command fails: on success it asks for steps, such as go mod tidy, that
// this has already run.
func moduleFromRelease(ctx context.Context, stderr io.Writer, dir, name string) error {
	at := publishedVersion()
	_, _ = fmt.Fprintf(stderr, "Fetching Oikade %s\n", at)
	for _, args := range [][]string{{"mod", "init", name}, {"get", oikadeModule + "@" + at}, {"mod", "tidy"}} {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			_, _ = stderr.Write(out)
			return cli.Wrap(err, "go %s failed, as it says above", strings.Join(args, " "))
		}
	}
	return nil
}

// moduleFromSource writes a go.mod on a checkout, with that checkout's dependencies, so that nothing
// needs fetching.
func moduleFromSource(dir, name, source string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), "module "+oikadeModule+"\n") {
		return fmt.Errorf("%s isn't a checkout of Oikade", source)
	}
	mod := regexp.MustCompile(`(?m)^module .*$`).ReplaceAllLiteralString(string(raw), "module "+name)
	mod += "\nrequire " + oikadeModule + " v0.0.0\n\nreplace " + oikadeModule + " => " + source + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		return err
	}
	sum, err := os.ReadFile(filepath.Join(source, "go.sum"))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644)
}

// homeFiles are a new home's files, by path.
func homeFiles() (map[string][]byte, error) {
	devices, err := registry.Render(registry.Snapshot{Next: 4, Devices: []registry.Device{
		{Name: "porch-light", Room: "porch", Capabilities: []string{"on"}, Endpoint: 2},
		{Name: "porch-sensor", Room: "porch", Capabilities: []string{"occupied"}, Endpoint: 3},
	}}, "main")
	if err != nil {
		return nil, errors.New("rendering the example devices: " + err.Error())
	}
	install := publishedVersion()
	return map[string][]byte{
		"main.go":                      []byte(homeMain),
		"home_test.go":                 []byte(homeTest),
		"devices.go":                   devices,
		".github/workflows/deploy.yml": []byte(strings.ReplaceAll(homeWorkflow, "OIKADE_VERSION", install)),
	}, nil
}

const homeMain = `// Command home is your home. Run oikade dev to try it, oikade test to test it and
// oikade deploy to ship it.
package main

import (
	"time"

	"github.com/harrymitchinson/oikade"
)

// home is your automations. PorchLight and PorchSensor come from devices.go, which oikade types
// writes from your real devices, with a field for each thing each one can do: replace these two with
// yours.
func home() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	// A device keeps the name its app gives it until you declare it here, with its hardware, its
	// room and what it is to the Home app. oikade devices prints the line for each one:
	//
	//	home.Device("hall-lamp").Hardware("zigbee2mqtt", "0x00158d0001a2b3c4").Room("hall").Is(oikade.OnOffLight)
	home.Decide(PorchLight.On).
		Case("motion", true, PorchSensor.Occupied).Linger(5 * time.Minute).
		Default(false)
	return home
}

func main() {
	home().Run()
}
`

const homeTest = `package main

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/hometest"
)

func TestPorchLightFollowsThePorchSensor(t *testing.T) {
	h := hometest.New(t, home(), hometest.Options{})

	h.Set(PorchSensor.Occupied, true)
	h.ExpectValue(PorchLight.On, true)

	h.Set(PorchSensor.Occupied, false)
	h.Advance(4 * time.Minute)
	h.ExpectValue(PorchLight.On, true)

	h.Advance(2 * time.Minute)
	h.ExpectValue(PorchLight.On, false)
}
`

// homeWorkflow tests every change, and deploys what reaches main. It needs three settings: the
// secret OIKADE_TOKEN, from oikade-host client add ci --token, and the variables OIKADE_ADDRESS, where the
// host serves its API, and OIKADE_PIN, the pin client add printed, because a key taken from whatever
// answers proves nothing. GitHub's runners reach the host only if it's on the internet, so the README says
// to run it on a self-hosted runner, or over a VPN.
const homeWorkflow = `name: Home

on:
  pull_request:
  push:
    branches: [main]

jobs:
  home:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go install github.com/harrymitchinson/oikade/cmd/oikade@OIKADE_VERSION
      - run: oikade check
      - run: oikade test
      - name: Deploy
        if: github.event_name == 'push'
        env:
          OIKADE_TOKEN: ${{ secrets.OIKADE_TOKEN }}
          OIKADE_ADDRESS: ${{ vars.OIKADE_ADDRESS }}
          OIKADE_PIN: ${{ vars.OIKADE_PIN }}
        run: |
          oikade login "$OIKADE_ADDRESS" --pin "$OIKADE_PIN"
          oikade deploy
`
