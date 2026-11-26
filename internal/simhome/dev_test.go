package simhome

import (
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/wire"
)

// A device is simulated from its line in your code, in its room and of its type, and nothing
// declared without one is.
func TestTheDevicesAreTheHomesLines(t *testing.T) {
	salon, none := "salon", ""
	lines := []wire.DeviceDeclaration{
		{Hardware: "hue/0x1", Room: &salon, DeviceType: devicetype.OccupancySensor, Capabilities: []string{"occupied", "temperature"}},
		{Hardware: "attic-fan", Room: &none, DeviceType: devicetype.DimmableLight, Capabilities: []string{"brightness", "on"}},
		{Name: "porch-light", Room: new("porch"), DeviceType: devicetype.OnOffLight},
	}
	got, err := Declared(lines)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	fan, sensor := got[0], got[1]
	if fan.Name != "attic-fan" || fan.Room != "" || !slices.Equal(fan.DeviceTypes, []devicetype.Type{devicetype.DimmableLight}) {
		t.Errorf("a light in no room is %+v", fan)
	}
	if sensor.Name != "hue/0x1" || sensor.Room != "salon" ||
		!slices.Equal(sensor.Capabilities, []string{"occupied", "temperature"}) ||
		!slices.Equal(sensor.DeviceTypes, []devicetype.Type{devicetype.OccupancySensor, devicetype.TemperatureSensor}) {
		t.Errorf("a sensor is %+v", sensor)
	}
	// Hardware Home.Device declares has no capabilities, so it needs a line of its own.
	old := append(slices.Clip(lines), wire.DeviceDeclaration{Name: "pool-pump", Plugin: "ewelink", ID: "1002a9b9e2"})
	if _, err := Declared(old); err == nil || !strings.Contains(err.Error(), "pool-pump with home.Device") {
		t.Errorf("got %v", err)
	}
}
