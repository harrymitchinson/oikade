package main

import (
	"bytes"
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

// requireHome fails unless module is in a Go module, as a home is, saying how to name one. It looks
// for a go.mod there and above, as go build does, and leaves any other trouble to go.
func requireHome(module string) error {
	if _, ok := goModDir(module); ok {
		return nil
	}
	where := "here"
	if module != "." {
		where = "in " + module
	}
	return cli.Errorf("there's no home %s: a home is a Go module, with a go.mod", where).
		WithHint("cd into your home's module, or pass --module DIR. oikade init starts one")
}

// goModDir is the directory with the go.mod that module is in, looking there and above as go build
// does, or module itself, made absolute, if there's none.
func goModDir(module string) (string, bool) {
	dir, err := filepath.Abs(module)
	if err != nil {
		return module, false
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); !errors.Is(err, os.ErrNotExist) {
			return d, true
		}
		if filepath.Dir(d) == d {
			return dir, false
		}
	}
}

// ranAndFailed is whether err is a program that ran and failed, which has said why on stderr.
func ranAndFailed(err error) bool {
	_, ok := errors.AsType[*exec.ExitError](err)
	return ok
}

// runCheck finds what's wrong with a home without touching one: the compiler, then the source, then the
// checks your binary makes of itself, which are the ones a deploy makes.
func runCheck(ctx context.Context, args []string) error {
	fs := newFlags("check")
	target := addTarget(fs)
	module := fs.String("module", ".", "Your home's module.")
	envFile := fs.String("env-file", "", "Your values, from this file rather than the .env beside your go.mod.")
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
	// With your values, as a deploy's check has them. A pull request has none, so one missing is only
	// a warning.
	m, vals, missing, err := homeValues(ctx, *module, path, *envFile, os.LookupEnv)
	if err != nil {
		return here(err)
	}
	if err := sameProtocol(m); err != nil {
		return err
	}
	for _, w := range m.Warnings {
		fmt.Fprintln(os.Stderr, "Warning:", w)
	}
	vals.warnMissing(std.err, missing, "a deploy from here would stop")
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

// homeManifest builds a home and asks it what it is, with your values, as a deploy does.
func homeManifest(ctx context.Context, module string) (wire.Manifest, error) {
	path, done, err := buildHome(ctx, module)
	if err != nil {
		return wire.Manifest{}, err
	}
	defer done()
	m, _, _, err := homeValues(ctx, module, path, "", os.LookupEnv)
	if err != nil {
		return wire.Manifest{}, here(err)
	}
	return m, sameProtocol(m)
}

// notHere is a home's check that couldn't run on this machine, or whose answer this oikade can't
// read, though the host may.
type notHere struct{ error }

func (e notHere) Unwrap() error { return e.error }

// cantRunHere is a check that failed only because it couldn't run here, or be read.
func cantRunHere(err error) bool {
	_, ok := errors.AsType[notHere](err)
	return ok
}

// readManifest asks a built home what it is, whichever protocol it speaks, with env added to its
// environment.
func readManifest(ctx context.Context, path string, env []string) (wire.Manifest, error) {
	cmd := exec.CommandContext(ctx, path, wire.RoleCheck)
	cmd.Stderr = os.Stderr
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.Output()
	if err != nil {
		if !ranAndFailed(err) {
			return wire.Manifest{}, notHere{fmt.Errorf("couldn't run your home's checks: %w", err)}
		}
		return wire.Manifest{}, cli.Wrap(err, "your home failed its own checks, as it says above")
	}
	// A home's own code may have printed first.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var m wire.Manifest
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		return wire.Manifest{}, notHere{cli.Wrap(err, "your home's check said something this oikade can't read").
			WithHint("build your home against the same version of Oikade as this oikade, %s", cli.Version(version))}
	}
	return m, nil
}

// manifestOf asks a built home what it is, and refuses one that speaks another protocol.
func manifestOf(ctx context.Context, path string) (wire.Manifest, error) {
	m, err := readManifest(ctx, path, nil)
	if err != nil {
		return wire.Manifest{}, here(err)
	}
	return m, sameProtocol(m)
}

// here unwraps a notHere, for a command that checks your home only here, with no host to try it
// instead.
func here(err error) error {
	if e, ok := errors.AsType[notHere](err); ok {
		return e.error
	}
	return err
}

// sameProtocol refuses a home built for another protocol than this oikade's.
func sameProtocol(m wire.Manifest) error {
	if m.Protocol != wire.Version {
		return cli.Errorf("your home was built for protocol %d, and this oikade speaks %d",
			m.Protocol, wire.Version).
			WithHint("build your home against the same version of Oikade as this oikade, %s", cli.Version(version))
	}
	return nil
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
		if local, ok := api.Unexplained(t).(api.Local); ok {
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
					"(oikade list devices lists them)", h.Plugin, name))
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
		if !ranAndFailed(err) {
			return fmt.Errorf("couldn't run go test: %w", err)
		}
		return cli.Wrap(err, "your tests didn't pass, as go test says above")
	}
	return nil
}

// runDev runs a home against a simulated one, built from the committed types file.
func runDev(ctx context.Context, args []string) error {
	fs := newFlags("dev")
	module := fs.String("module", ".", "Your home's module.")
	types := fs.String("types", "devices.go", "The file oikade types wrote, in your module.")
	envFile := fs.String("env-file", "", "Your values, from this file rather than the .env beside your go.mod.")
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
	// Your code gets your values, as it does on the host, so what it builds from them, such as a
	// plugin's config, is as it will be there. Its check says which it uses that nothing gives: one
	// that can't run here, or be read, says nothing, and dev runs the home itself.
	_, vals, missing, err := homeValues(ctx, *module, path, *envFile, os.LookupEnv)
	switch {
	case cantRunHere(err):
	case err != nil:
		return err
	default:
		vals.warnMissing(std.err, missing, `oikade.Var gives "" for `+them(missing)+" here")
	}
	env, remove, err := valuesFile(vals.set)
	if err != nil {
		return err
	}
	defer remove()
	cmd := exec.CommandContext(ctx, path, wire.RoleDev, file)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), env...)
	return cmd.Run()
}

// runInit starts a home in a directory that hasn't one.
func runInit(ctx context.Context, args []string) error {
	fs := newFlags("init")
	name := fs.String("module-path", "home", "Your module's path, such as github.com/you/home.")
	renamed(fs, "module", "module-path", false) // --module is a directory everywhere else.
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
	// So that the same command can be run again.
	undo := func(err error) error {
		for _, path := range written {
			_ = os.Remove(filepath.Join(dir, path))
		}
		// Only if they're empty: these may have been here already.
		_ = os.Remove(filepath.Join(dir, ".github", "workflows"))
		_ = os.Remove(filepath.Join(dir, ".github"))
		return err
	}
	if *source != "" {
		err = moduleFromSource(dir, *name, *source)
	} else {
		err = moduleFromRelease(ctx, os.Stderr, dir, *name)
	}
	if err != nil {
		return undo(err)
	}
	// Your values stay out of Git, in a .gitignore that's there already or a new one.
	ignored, err := ignoreValues(dir)
	if err != nil {
		return undo(cli.Wrap(err, "couldn't add .env to the .gitignore in %s", dir))
	}
	if ignored {
		written = append(written, ".gitignore")
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

// ignoreValues makes the .gitignore in dir list .env, where your values go, writing one if there's
// none, and says whether it changed it.
func ignoreValues(dir string) (bool, error) {
	path := filepath.Join(dir, ".gitignore")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == dotEnv || line == "/"+dotEnv {
			return false, nil
		}
	}
	if len(raw) > 0 && !bytes.HasSuffix(raw, []byte("\n")) {
		raw = append(raw, '\n')
	}
	raw = append(raw, "# Your values, which oikade deploy sends the host: never in Git.\n/"+dotEnv+"\n"...)
	return true, os.WriteFile(path, raw, 0o644)
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
			if !ranAndFailed(err) {
				return fmt.Errorf("couldn't run go %s: %w", strings.Join(args, " "), err)
			}
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
	// room and what it is to the Home app. oikade list devices prints the line to declare each
	// one that isn't yet:
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

// homeWorkflow tests every change on GitHub's runners, and deploys what reaches main only once the
// variable OIKADE_ADDRESS is set, so a new home's first push passes. A deploy also needs OIKADE_PIN,
// the pin add printed, as a key taken from whatever answers proves nothing, and the secret
// OIKADE_TOKEN, from oikade-host add client/ci --token. It runs on a self-hosted runner, as GitHub's
// reach the host only if it's on the internet, and keeps its oikade and context in RUNNER_TEMP,
// emptied for each job, as that runner's user may be a person's too.
const homeWorkflow = `name: Home

on:
  pull_request:
  push:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go install github.com/harrymitchinson/oikade/cmd/oikade@OIKADE_VERSION
      # Your values, from the secret OIKADE_ENV, which a pull request from a fork doesn't get: then
      # oikade check only warns. RUNNER_TEMP is emptied after the job.
      - name: Write your values
        env:
          OIKADE_ENV: ${{ secrets.OIKADE_ENV }}
        run: (umask 077 && printf '%s\n' "$OIKADE_ENV" > "$RUNNER_TEMP/.env")
      - run: oikade check --env-file "$RUNNER_TEMP/.env"
      - run: oikade test
      - name: Deploy is off
        if: github.event_name == 'push' && vars.OIKADE_ADDRESS == ''
        run: |
          echo "::notice title=Deploy is off::` + deployIsOff + `"

  deploy:
    needs: test
    if: github.event_name == 'push' && vars.OIKADE_ADDRESS != ''
    runs-on: self-hosted
    # One deploy at a time, and a push replaces a deploy still waiting, so the last push wins.
    concurrency: deploy
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      # A self-hosted runner keeps its home between jobs, so this oikade and its login stay out of it.
      - name: Keep to the job's own directory
        run: |
          echo "GOBIN=$RUNNER_TEMP/bin" >> "$GITHUB_ENV"
          echo "XDG_CONFIG_HOME=$RUNNER_TEMP/config" >> "$GITHUB_ENV"
          echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"
      - run: go install github.com/harrymitchinson/oikade/cmd/oikade@OIKADE_VERSION
      # Not in the checkout, which a self-hosted runner keeps: RUNNER_TEMP is emptied after the job.
      - name: Write your values
        env:
          OIKADE_ENV: ${{ secrets.OIKADE_ENV }}
        run: (umask 077 && printf '%s\n' "$OIKADE_ENV" > "$RUNNER_TEMP/.env")
      - name: Deploy
        env:
          OIKADE_TOKEN: ${{ secrets.OIKADE_TOKEN }}
          OIKADE_ADDRESS: ${{ vars.OIKADE_ADDRESS }}
          OIKADE_PIN: ${{ vars.OIKADE_PIN }}
        run: |
          oikade login "$OIKADE_ADDRESS" --pin "$OIKADE_PIN"
          oikade deploy --env-file "$RUNNER_TEMP/.env"
`

// deployIsOff is the notice on a push to main before deploys are on, naming what turns them on.
const deployIsOff = "Set the variables OIKADE_ADDRESS and OIKADE_PIN and the secret OIKADE_TOKEN to deploy " +
	"what reaches main, from a self-hosted runner: " + deployFromCIDocs

// deployFromCIDocs is the section of the docs that sets up deploys from CI.
const deployFromCIDocs = "https://github.com/harrymitchinson/oikade/blob/main/docs/running-a-home.md#deploying-from-ci"
