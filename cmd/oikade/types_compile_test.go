package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"oikade.dev/internal/devicetype"
)

// homeDevices are a home's devices, as oikade types adds them.
var homeDevices = []newDevice{
	{name: "porch-light", key: "hue/0x2", room: "porch", typ: devicetype.OnOffLight},
	{name: "porch-sensor", key: "hue/0x3", room: "porch", typ: devicetype.OccupancySensor},
	{name: "hallway", key: "hue/0x4", typ: devicetype.DimmableLight},
	{name: "garden-door", key: "hue/0x5", typ: devicetype.ContactSensor, more: []string{"battery"}},
	{name: "bedroom-ac", key: "toshibaac/ac1", room: "bedroom", typ: devicetype.RoomAirConditioner},
}

// homeWith is a home whose rules are body, which adds them to home.
func homeWith(body string) string {
	return `package main

import (
	"time"

	"oikade.dev"
)

func main() {
	_ = GardenDoor.Key
	home := oikade.NewHome(oikade.Options{})
	` + body + `
	home.Run()
}

var _ = time.Minute
`
}

// The compiler is what checks a home's capabilities, so these homes are built against the lines
// oikade types adds.
func TestAHomeCompilesOnlyWithItsDevicesCapabilities(t *testing.T) {
	devices, _, err := addDevices(nil, "main", nil, homeDevices)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"fine": {body: `
		home.Decide(PorchLight.On).
			Case("motion", true, PorchSensor.Occupied).Linger(5 * time.Minute).
			Default(false)
		home.Decide(Hallway.Brightness).Case("evening", 40, oikade.Daily("18:00", "23:00")).Flows().Default(0)
		home.Flow("goodnight", func(r *oikade.FlowRun) {
			r.Set(home.Room(oikade.Room("living-room"), oikade.Lights).On.To(false))
			r.Set(Hallway.Brightness.To(10).For(time.Hour))
		}).
			At(oikade.Clock("23:30")).
			When(PorchSensor.Occupied, home.Room(oikade.Room("porch")).Occupied)
		porch := home.Group("porch", PorchLight, PorchSensor, GardenDoor)
		home.Decide(porch.On).Case("dusk", true, oikade.Daily("18:00", "23:00")).Default(false)
		home.Decide(BedroomAc.Mode).Case("humid", oikade.ModeDry, PorchSensor.Occupied).Default(oikade.ModeCool)
		home.Flow("cool-down", func(r *oikade.FlowRun) {
			if r.Get(BedroomAc.Mode) != oikade.ModeCool {
				r.Set(BedroomAc.Mode.To(oikade.ModeCool))
			}
			r.Set(home.Room(oikade.Room("bedroom")).Mode.To(oikade.ModeFan))
		}).At(oikade.Clock("14:00"))`},
		"a misspelt mode": {
			body: `home.Decide(BedroomAc.Mode).Default(oikade.ModeDyr)`,
			want: "undefined: oikade.ModeDyr",
		},
		"a mode set to a number": {
			body: `home.Flow("a", func(r *oikade.FlowRun) { r.Set(BedroomAc.Mode.To(3)) })`,
			want: "cannot use 3",
		},
		"a mode as a case of a bool": {
			body: `home.Decide(BedroomAc.On).Case("a", oikade.ModeDry, PorchSensor.Occupied).Default(false)`,
			want: "cannot use oikade.ModeDry",
		},
		"a capability the device hasn't": {
			body: `home.Decide(PorchLight.Occupied).Default(false)`,
			want: "PorchLight.Occupied undefined",
		},
		"a capability beyond its type's that it hasn't": {
			body: `home.Decide(GardenDoor.Temperature).Default(0)`,
			want: "GardenDoor.Temperature undefined",
		},
		"one it has": {body: `home.Decide(Porch.On).Case("low", true, GardenDoor.Battery.Below(20)).Default(false)`},
		"a number set to a bool": {
			body: `home.Flow("a", func(r *oikade.FlowRun) { r.Set(Hallway.Brightness.To(true)) })`,
			want: "cannot use true",
		},
		"a case of the wrong type": {
			body: `home.Decide(Hallway.Brightness).Case("a", true, Hallway.On).Default(0)`,
			want: "cannot use true",
		},
		"a bool read as a number": {
			body: `home.Decide(Hallway.On).Case("a", true, Hallway.On.Above(5)).Default(false)`,
			want: "Hallway.On.Above undefined",
		},
		"a room's capability set to the wrong type": {
			body: `home.Flow("a", func(r *oikade.FlowRun) { r.Set(home.Room(oikade.Room("hall"), oikade.Lights).On.To(10)) })`,
			want: "cannot use 10",
		},
		"a group as a member": {
			body: `home.Group("g", home.Room(oikade.Room("hall")))`,
			want: "does not implement",
		},
		"a capability as a member": {
			body: `home.Group("g", PorchLight.On)`,
			want: "does not implement",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := buildOverlaid(t, devices, homeWith(tc.body))
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

// buildOverlaid builds a home inside this module, from files that exist only in an overlay, so that it
// needs no module of its own.
func buildOverlaid(t *testing.T, devices []byte, main string) (string, error) {
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
