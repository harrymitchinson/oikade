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
	var parts []oikade.Part
	decide := func(*oikade.Home) {}
	` + body + `
	home := oikade.NewHome(oikade.Options{}, parts...)
	decide(home)
	oikade.Run(home)
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
		decide = func(home *oikade.Home) {
			home.Decide(PorchLight.On).
				Case("motion", true, PorchSensor.Occupied).Linger(5 * time.Minute).
				Default(false)
			home.Decide(Hallway.Brightness).Case("evening", 40, oikade.Daily("18:00", "23:00")).Flows().Default(0)
		}
		parts = append(parts, oikade.NewMoment("goodnight", oikade.InRoom("living-room", oikade.Lights, oikade.On).To(false), Hallway.Brightness.To(10).For(time.Hour)).
At(oikade.Clock("23:30")).
On(PorchSensor.Occupied, oikade.InRoom("porch", oikade.Everything, oikade.Occupied)))`},
		"a capability the device hasn't": {
			body: `decide = func(home *oikade.Home) { home.Decide(PorchLight.Occupied).Default(false) }`,
			want: "PorchLight.Occupied undefined",
		},
		"a reserved device's capability": {
			body: `decide = func(home *oikade.Home) { home.Decide(GardenLight.On).Default(false) }`,
			want: "GardenLight.On undefined",
		},
		"a number set to a bool": {
			body: `parts = append(parts, oikade.NewMoment("a", Hallway.Brightness.To(true)))`,
			want: "cannot use true",
		},
		"a case of the wrong type": {
			body: `decide = func(home *oikade.Home) { home.Decide(Hallway.Brightness).Case("a", true, Hallway.On).Default(0) }`,
			want: "cannot use true",
		},
		"a bool read as a number": {
			body: `decide = func(home *oikade.Home) { home.Decide(Hallway.On).Case("a", true, Hallway.On.Above(5)).Default(false) }`,
			want: "Hallway.On.Above undefined",
		},
		"a room's capability set to the wrong type": {
			body: `parts = append(parts, oikade.NewMoment("a", oikade.InRoom("hall", oikade.Lights, oikade.On).To(10)))`,
			want: "cannot use 10",
		},
		"a room write that doesn't say what it's for": {
			body: `parts = append(parts, oikade.NewMoment("a", oikade.InRoom("hall", oikade.On).To(true)))`,
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
