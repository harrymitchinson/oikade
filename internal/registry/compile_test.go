package registry

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// homeDevices are a home's devices, one of them reserved.
var homeDevices = Snapshot{Next: 6, Devices: []Device{
	{Name: "porch-light", Room: "porch", Capabilities: []string{"on"}, Endpoint: 2},
	{Name: "porch-sensor", Room: "porch", Capabilities: []string{"occupied"}, Endpoint: 3},
	{Name: "hallway", Capabilities: []string{"on", "brightness"}, Endpoint: 4},
	{Name: "garden-light", Endpoint: 5},
}}

// homeWith is a home whose rules are body.
func homeWith(body string) string {
	return `package main

import (
	"time"

	"github.com/harrymitchinson/oikade"
)

func main() {
	_ = GardenLight.Name
	oikade.Run(oikade.NewHome(oikade.Options{}, ` + body + `))
}

var _ = time.Minute
`
}

// The compiler is what checks a home's capabilities, so these homes are built against the file
// oikade types writes.
func TestAHomeCompilesOnlyWithItsDevicesCapabilities(t *testing.T) {
	devices, err := Render(homeDevices, "main")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"fine": {body: `
		oikade.NewHold("porch-light", PorchLight.On.To(true)).
When(PorchSensor.Occupied).
OffDelay(5 * time.Minute),
		oikade.NewMoment("goodnight", oikade.InRoom("living-room", oikade.Lights, oikade.On).To(false), Hallway.Brightness.To(10).For(time.Hour)).
At(oikade.Clock("23:30")).
On(PorchSensor.Occupied, oikade.InRoom("porch", oikade.Everything, oikade.Occupied)),`},
		"a capability the device hasn't": {
			body: `oikade.NewHold("a", PorchLight.Occupied.To(true)),`,
			want: "PorchLight.Occupied undefined",
		},
		"a reserved device's capability": {
			body: `oikade.NewHold("a", GardenLight.On.To(true)),`,
			want: "GardenLight.On undefined",
		},
		"a number set to a bool": {
			body: `oikade.NewMoment("a", Hallway.Brightness.To(true)),`,
			want: "cannot use true",
		},
		"a bool read as a number": {
			body: `oikade.NewHold("a", oikade.Set{}).
When(Hallway.On.Above(5)),`,
			want: "Hallway.On.Above undefined",
		},
		"a room's capability set to the wrong type": {
			body: `oikade.NewMoment("a", oikade.InRoom("hall", oikade.Lights, oikade.On).To(10)),`,
			want: "cannot use 10",
		},
		"a room write that doesn't say what it's for": {
			body: `oikade.NewMoment("a", oikade.InRoom("hall", oikade.On).To(true)),`,
			want: "not enough arguments in call to oikade.InRoom",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := buildHome(t, devices, homeWith(tc.body))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("%v\n%s", err, out)
			case tc.want != "" && err == nil:
				t.Fatal("it compiled")
			case tc.want != "" && !strings.Contains(out, tc.want):
				t.Fatalf("no %q in:\n%s", tc.want, out)
			}
		})
	}
}

// buildHome builds a home inside this module, from files that exist only in an overlay, so that it
// needs no module of its own.
func buildHome(t *testing.T, devices []byte, main string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(wd, "testdata", "home-"+filepath.Base(dir))
	replace := map[string]string{}
	for file, content := range map[string][]byte{"devices.go": devices, "main.go": []byte(main)} {
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		replace[filepath.Join(pkg, file)] = path
	}
	overlay, err := json.Marshal(map[string]any{"Replace": replace})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-overlay", overlayPath, "-o", os.DevNull, pkg)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
