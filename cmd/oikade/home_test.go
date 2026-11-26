package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/registry"
)

// fakeGo puts a go on PATH that prints Go's own lines and fails on the subcommand failOn, if any.
func fakeGo(t *testing.T, failOn string) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
echo "go: to add module requirements and sums:" >&2
echo "	go mod tidy" >&2
echo "go: running $1" 
if [ "$1" = "` + failOn + `" ]; then
	echo "go: $1 went wrong" >&2
	exit 1
fi
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestModuleFromReleaseHidesGoWhenItPasses(t *testing.T) {
	fakeGo(t, "")
	var stderr bytes.Buffer
	if err := moduleFromRelease(t.Context(), &stderr, t.TempDir(), "home"); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); strings.Contains(got, "go:") || strings.Contains(got, "go mod tidy") {
		t.Errorf("passed on Go's lines:\n%s", got)
	}
	if !strings.HasPrefix(stderr.String(), "Fetching Oikade ") {
		t.Errorf("didn't say what it's doing: %q", stderr.String())
	}
}

func TestModuleFromReleaseShowsGoWhenItFails(t *testing.T) {
	fakeGo(t, "get")
	var stderr bytes.Buffer
	err := moduleFromRelease(t.Context(), &stderr, t.TempDir(), "home")
	if err == nil || !strings.Contains(err.Error(), "go get") {
		t.Fatalf("got %v", err)
	}
	for _, want := range []string{"go: to add module requirements and sums:", "go: running get", "go: get went wrong"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("no %q in:\n%s", want, stderr.String())
		}
	}
	if strings.Count(stderr.String(), "go: running") != 1 {
		t.Errorf("showed the output of commands that passed:\n%s", stderr.String())
	}
}

func TestCheckFindsAShownDeviceThatIsntThePlugins(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{Name: "garage-door", ID: "s2", Driver: "plugin/tahoma", Endpoint: 2},
		{Name: "desk-plug", ID: "p1", Driver: "plugin/lan", Endpoint: 3},
		{Name: "old-shutter", ID: "s9", Driver: "plugin/tahoma", Endpoint: 4, Removed: true},
	}}
	if err := checkShown(list, []wire.HiddenPlugin{{Plugin: "tahoma", Shown: []string{"garage-door"}}}); err != nil {
		t.Errorf("a plugin's own device: %v", err)
	}
	err := checkShown(list, []wire.HiddenPlugin{{Plugin: "tahoma", Shown: []string{"desk-plug", "old-shutter", "gate"}}})
	for _, name := range []string{"desk-plug", "old-shutter", "gate"} {
		if err == nil || !strings.Contains(err.Error(), "ShowOnMatter names "+name+",") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestCheckSaysWhichDevicesADeployHides(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{Name: "bedroom-shutter", ID: "s1", Driver: "plugin/tahoma", Endpoint: 2, Capabilities: []string{"position"}},
		{Name: "garage-door", ID: "s2", Driver: "plugin/tahoma", Endpoint: 3, Capabilities: []string{"position"}},
		{Name: "attic-shutter", ID: "s3", Driver: "plugin/tahoma", Endpoint: 4, Capabilities: []string{"position"}},
		// Matter has nothing to show of it, so it was never on the bridge.
		{Name: "box-signal", ID: "s4", Driver: "plugin/tahoma", Endpoint: 5, Capabilities: []string{"link-quality"}},
	}}
	hidden := []wire.HiddenPlugin{{Plugin: "tahoma", Shown: []string{"garage-door"}}}
	var out bytes.Buffer
	printHiding(&out, list, hidden, []string{"tahoma/s3"})
	if want := "A deploy hides hardware/tahoma/s1 from Matter,"; !strings.HasPrefix(out.String(), want) {
		t.Errorf("got %q, want %q", out.String(), want)
	}
	out.Reset()
	printHiding(&out, list, hidden, []string{"tahoma/s1", "tahoma/s3"})
	if out.Len() != 0 {
		t.Errorf("with nothing new to hide: %q", out.String())
	}
}
