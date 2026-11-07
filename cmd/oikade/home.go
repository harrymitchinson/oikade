package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/registry"
)

// frameworkModule is what a home imports.
const frameworkModule = "github.com/harrymitchinson/oikade"

// buildHome builds a module for this machine, in a directory that ends with done.
func buildHome(ctx context.Context, module string) (path string, done func(), err error) {
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

// runCheck finds what's wrong with a home without touching one: the compiler, then flows, then the
// checks your binary makes of itself, which are the ones a deploy makes.
func runCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	module := fs.String("module", ".", "Your home's module.")
	if extra := parseFlags(fs, args); len(extra) > 0 {
		return fmt.Errorf("oikade check takes no %q", extra[0])
	}
	path, done, err := buildHome(ctx, *module)
	if err != nil {
		return err
	}
	defer done()
	if err := checkFlows(*module); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, wire.RoleCheck)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("checking your home: %w", err)
	}
	// A home's own code may have printed first.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var m wire.Manifest
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		return fmt.Errorf("reading your home's check: %w", err)
	}
	if m.Protocol != wire.Version {
		return fmt.Errorf("your home was built for protocol %d, and this oikade speaks %d: "+
			"use the same version of both", m.Protocol, wire.Version)
	}
	fmt.Printf("Your home is fine: %d rules, %d plugins\n", len(m.Rules), len(m.Plugins))
	return nil
}

// runTest runs a home's tests. Anything after the flags goes to go test.
func runTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	module := fs.String("module", ".", "Your home's module.")
	_ = fs.Parse(args)
	packages := fs.Args()
	if len(packages) == 0 {
		packages = []string{"./..."}
	}
	cmd := exec.CommandContext(ctx, "go", append([]string{"test"}, packages...)...)
	cmd.Dir = *module
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("your tests didn't pass: %w", err)
	}
	return nil
}

// runDev runs a home against a simulated one, built from the committed types file.
func runDev(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	module := fs.String("module", ".", "Your home's module.")
	types := fs.String("types", "devices.go", "The file oikade types wrote, in your module.")
	if extra := parseFlags(fs, args); len(extra) > 0 {
		return fmt.Errorf("oikade dev takes no %q", extra[0])
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
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	name := fs.String("module", "home", "Your module's path.")
	source := fs.String("source", "", "A checkout of Oikade to build against, rather than a published version.")
	extra := parseFlags(fs, args)
	if len(extra) > 1 {
		return fmt.Errorf("oikade init takes one directory, not %q", extra[1])
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
		err = moduleFromRelease(ctx, dir, *name)
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
	fmt.Println("Started a home. Next: oikade test, then oikade dev.")
	return nil
}

// moduleFromRelease writes a go.mod on the Oikade this binary is, or main if it's no release.
func moduleFromRelease(ctx context.Context, dir, name string) error {
	at := publishedVersion()
	for _, args := range [][]string{{"mod", "init", name}, {"get", frameworkModule + "@" + at}, {"mod", "tidy"}} {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
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
	if !strings.Contains(string(raw), "module "+frameworkModule+"\n") {
		return fmt.Errorf("%s isn't a checkout of Oikade", source)
	}
	mod := regexp.MustCompile(`(?m)^module .*$`).ReplaceAllLiteralString(string(raw), "module "+name)
	mod += "\nrequire " + frameworkModule + " v0.0.0\n\nreplace " + frameworkModule + " => " + source + "\n"
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
		{Name: "porch-light", Room: "porch", Capabilities: []string{"light.on"}, Endpoint: 2},
		{Name: "porch-sensor", Room: "porch", Capabilities: []string{"sensor.occupancy-detected"}, Endpoint: 3},
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

const homeMain = `// Command home is your home. Run oikade dev to try it, oikade test to test it and oikade deploy to
// ship it.
package main

import (
	"time"

	"github.com/harrymitchinson/oikade/framework"
)

// home is your automations. PorchLight and PorchSensor come from devices.go, which oikade types
// writes from your real devices: replace these two with yours.
func home() framework.Home {
	return framework.Home{
		Holds: []framework.Hold{{
			Name:       "porch-light",
			Device:     PorchLight,
			Capability: "light.on",
			Value:      true,
			When: func(s framework.State) bool {
				return s.Bool(PorchSensor, "sensor.occupancy-detected")
			},
			OffDelay: 5 * time.Minute,
		}},
	}
}

func main() {
	framework.Run(home())
}
`

const homeTest = `package main

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/framework/hometest"
)

func TestPorchLightFollowsThePorchSensor(t *testing.T) {
	h := hometest.New(t, home(), hometest.Options{})

	h.Set(PorchSensor, "sensor.occupancy-detected", true)
	h.ExpectValue(PorchLight, "light.on", true)

	h.Set(PorchSensor, "sensor.occupancy-detected", false)
	h.Advance(4 * time.Minute)
	h.ExpectValue(PorchLight, "light.on", true)

	h.Advance(2 * time.Minute)
	h.ExpectValue(PorchLight, "light.on", false)
}
`

// homeWorkflow tests every change, and deploys what reaches main. It needs three settings: the
// secret OIKADE_TOKEN, from oikade client add ci --token, and the variables OIKADE_ADDRESS, where the
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
