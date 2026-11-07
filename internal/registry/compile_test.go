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
	{Name: "porch-light", Room: "porch", Capabilities: []string{"light.on"}, Endpoint: 2},
	{Name: "porch-sensor", Room: "porch", Capabilities: []string{"sensor.occupancy-detected"}, Endpoint: 3},
	{Name: "hallway", Capabilities: []string{"light.on", "light.level"}, Endpoint: 4},
	{Name: "garden-light", Endpoint: 5},
}}

// homeWith is a home whose rules are body.
func homeWith(body string) string {
	return `package main

import (
	"time"

	"github.com/harrymitchinson/oikade/framework"
)

func main() {
	_ = GardenLight.Name
	framework.Run(framework.Home{` + body + `})
}

var _ = time.Minute
`
}

// The compiler is what checks a home's capabilities, so these homes are built against the file
// oikadectl types writes.
func TestAHomeCompilesOnlyWithItsDevicesCapabilities(t *testing.T) {
	if testing.Short() {
		t.Skip("builds homes")
	}
	devices, err := Render(homeDevices, "main")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"fine": {body: `
		Holds: []framework.Hold{{
			Name: "porch-light",
			Set:  PorchLight.LightOn.To(true),
			When: func(s framework.State) bool { return s.Get(PorchSensor.Occupancy) },
			OffDelay: 5 * time.Minute,
		}},
		Moments: []framework.Moment{{
			Name: "goodnight", At: []string{"23:30"},
			On:   []framework.Change{PorchSensor.Occupancy, framework.InRoom("porch", framework.Occupancy)},
			Do: []framework.Action{
				framework.InRoom("living-room", framework.LightOn).To(false),
				Hallway.LightLevel.To(10).For(time.Hour),
			},
		}},`},
		"a capability the device hasn't": {
			body: `Holds: []framework.Hold{{Name: "a", Set: PorchLight.Occupancy.To(true)}},`,
			want: "PorchLight.Occupancy undefined",
		},
		"a reserved device's capability": {
			body: `Holds: []framework.Hold{{Name: "a", Set: GardenLight.LightOn.To(true)}},`,
			want: "GardenLight.LightOn undefined",
		},
		"a number set to a bool": {
			body: `Moments: []framework.Moment{{Name: "a", Do: []framework.Action{Hallway.LightLevel.To(true)}}},`,
			want: "cannot use true",
		},
		"a bool read as a number": {
			body: `Holds: []framework.Hold{{Name: "a", When: func(s framework.State) bool { return s.Get(Hallway.LightOn) > 5 }}},`,
			want: "mismatched types",
		},
		"a room's capability set to the wrong type": {
			body: `Moments: []framework.Moment{{Name: "a", Do: []framework.Action{framework.InRoom("hall", framework.LightOn).To(10)}}},`,
			want: "cannot use 10",
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
