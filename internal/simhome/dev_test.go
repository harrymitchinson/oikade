package simhome

import (
	"slices"
	"testing"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/wire"
)

// A device is simulated from its line in your code, in its room and of its type.
func TestTheDevicesAreTheHomesLines(t *testing.T) {
	lines := []wire.DeviceDeclaration{
		{Hardware: "hue/0x1", Room: "salon", DeviceType: devicetype.OccupancySensor, Capabilities: []string{"occupied", "temperature"}},
		{Hardware: "attic-fan", DeviceType: devicetype.DimmableLight, Capabilities: []string{"brightness", "on"}},
	}
	got := Declared(lines)
	if len(got) != 2 {
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
}
