package simhome

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/registry"
)

func TestTheTypesFileGivesDevicesAndTheHomeTheirRoomsAndTypes(t *testing.T) {
	file, err := registry.Render(registry.Snapshot{Next: 4, Devices: []registry.Device{
		{Name: "attic-fan", Endpoint: 2, Capabilities: []string{"on"}, Room: "attic"},
		{Name: "porch-light", Endpoint: 3, Capabilities: []string{"on", "brightness"}},
	}}, "main")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "devices.go")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTypes(path, []wire.DeviceDeclaration{{Name: "porch-light", Room: new("porch"), DeviceType: devicetype.DimmableLight}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	fan, light := got[0], got[1]
	if fan.Name != "attic-fan" || fan.Room != "" || !slices.Equal(fan.Capabilities, []string{"on"}) {
		t.Errorf("an undeclared device is %+v, want it in no room", fan)
	}
	if light.Room != "porch" || !slices.Contains(light.DeviceTypes, devicetype.DimmableLight) ||
		!slices.Equal(light.Capabilities, []string{"brightness", "on"}) {
		t.Errorf("a declared device is %+v", light)
	}
}
