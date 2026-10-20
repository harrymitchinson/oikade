package hap

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
)

func serviceTypes(a *accessory.A) []string {
	out := make([]string, 0, len(a.Ss)-1)
	for _, s := range a.Ss[1:] { // skip AccessoryInformation
		out = append(out, s.Type)
	}
	return out
}

func char(a *accessory.A, svc, typ string) *characteristic.C {
	for _, s := range a.Ss {
		if s.Type != svc {
			continue
		}
		for _, c := range s.Cs {
			if c.Type == typ {
				return c
			}
		}
	}
	return nil
}

// fromHomeApp is a request, which is how hap tells a write from the Home app from a local update.
var fromHomeApp = httptest.NewRequest("PUT", "/characteristics", nil)

type recorded struct {
	capability string
	value      any
}

func TestDimmableLight(t *testing.T) {
	var sets []recorded
	a := Build(Device{
		AID: 7, Name: "Hallway Light", Manufacturer: "IKEA",
		Capabilities: []string{capability.LightOn, capability.LightLevel, capability.LinkQuality},
		Values:       map[string]any{capability.LightOn: true, capability.LightLevel: 49.6},
	}, func(c string, v any) error { sets = append(sets, recorded{c, v}); return nil })

	if a == nil || a.A.Id != 7 || a.A.Type != accessory.TypeLightbulb {
		t.Fatalf("accessory = %+v", a)
	}
	if got := serviceTypes(a.A); !slices.Equal(got, []string{service.TypeLightbulb}) {
		t.Fatalf("services = %v", got)
	}
	on := char(a.A, service.TypeLightbulb, characteristic.TypeOn)
	brightness := char(a.A, service.TypeLightbulb, characteristic.TypeBrightness)
	if on.Val != true || brightness.Val != 50 {
		t.Fatalf("initial values on=%v brightness=%v", on.Val, brightness.Val)
	}

	a.Update(capability.LightLevel, 12.0)
	a.Update(capability.LinkQuality, 80.0) // not mapped, ignored
	if brightness.Val != 12 {
		t.Fatalf("brightness after update = %v", brightness.Val)
	}

	// A write from the Home app goes to the driver.
	if _, code := on.SetValueRequest(false, fromHomeApp); code != 0 {
		t.Fatalf("SetValueRequest code %d", code)
	}
	if _, code := brightness.SetValueRequest(30, fromHomeApp); code != 0 {
		t.Fatalf("SetValueRequest code %d", code)
	}
	want := []recorded{{capability.LightOn, false}, {capability.LightLevel, 30.0}}
	if !slices.Equal(sets, want) {
		t.Fatalf("sets = %v, want %v", sets, want)
	}
}

func TestFailedWriteIsReported(t *testing.T) {
	a := Build(Device{AID: 2, Name: "Plug", Capabilities: []string{capability.SwitchOn}},
		func(string, any) error { return errors.New("offline") })
	on := char(a.A, service.TypeSwitch, characteristic.TypeOn)
	if _, code := on.SetValueRequest(true, fromHomeApp); code == 0 {
		t.Fatal("a failed write should not report success to HomeKit")
	}
}

func TestOutletOverride(t *testing.T) {
	a := Build(Device{AID: 3, Name: "Desk Plug", Capabilities: []string{capability.SwitchOn},
		Service: homev1alpha1.HomeKitServiceOutlet}, func(string, any) error { return nil })
	if a.A.Type != accessory.TypeOutlet || !slices.Equal(serviceTypes(a.A), []string{service.TypeOutlet}) {
		t.Fatalf("services = %v", serviceTypes(a.A))
	}
}

func TestMultiSensor(t *testing.T) {
	a := Build(Device{
		AID: 4, Name: "Hallway Motion",
		Capabilities: []string{capability.Occupancy, capability.Temperature, capability.Battery, capability.LinkQuality},
		Values:       map[string]any{capability.Temperature: -4.5, capability.Battery: 15.0},
	}, nil)
	if got := serviceTypes(a.A); !slices.Equal(got, []string{service.TypeMotionSensor, service.TypeTemperatureSensor, service.TypeBatteryService}) {
		t.Fatalf("services = %v", got)
	}
	if v := char(a.A, service.TypeTemperatureSensor, characteristic.TypeCurrentTemperature).Val; v != -4.5 {
		t.Fatalf("sub-zero temperature clamped to %v", v)
	}
	if v := char(a.A, service.TypeBatteryService, characteristic.TypeStatusLowBattery).Val; v != 1 {
		t.Fatalf("low battery = %v, want 1", v)
	}
	a.Update(capability.Occupancy, true)
	if v := char(a.A, service.TypeMotionSensor, characteristic.TypeMotionDetected).Val; v != true {
		t.Fatalf("motion = %v", v)
	}
}

func TestContactSensorIsInverted(t *testing.T) {
	a := Build(Device{AID: 5, Name: "Front Door", Capabilities: []string{capability.ContactOpen},
		Values: map[string]any{capability.ContactOpen: true}}, nil)
	state := char(a.A, service.TypeContactSensor, characteristic.TypeContactSensorState)
	if state.Val != characteristic.ContactSensorStateContactNotDetected {
		t.Fatalf("open door state = %v", state.Val)
	}
	a.Update(capability.ContactOpen, false)
	if state.Val != characteristic.ContactSensorStateContactDetected {
		t.Fatalf("closed door state = %v", state.Val)
	}
}

func TestNothingToExpose(t *testing.T) {
	if a := Build(Device{AID: 6, Name: "Router", Capabilities: []string{capability.LinkQuality}}, nil); a != nil {
		t.Fatal("a device with no mappable capability should not become an accessory")
	}
}

func TestPin(t *testing.T) {
	if got := Pin("031-45-154"); got != "03145154" {
		t.Fatalf("Pin = %s", got)
	}
}

func TestServerAcceptsEveryAccessoryKind(t *testing.T) {
	nop := func(string, any) error { return nil }
	kinds := [][]string{
		{capability.LightOn, capability.LightLevel, capability.LinkQuality},
		{capability.SwitchOn},
		{capability.OutletOn},
		{capability.Occupancy, capability.Temperature, capability.Humidity, capability.Battery},
		{capability.ContactOpen, capability.Battery},
	}
	as := make([]*accessory.A, 0, len(kinds))
	for i, caps := range kinds {
		a := Build(Device{AID: uint64(i + 2), Name: fmt.Sprintf("Device %d", i), Capabilities: caps}, nop)
		as = append(as, a.A)
	}
	bridge := accessory.NewBridge(accessory.Info{Name: "Oikade"})
	bridge.Id = 1
	if _, err := hap.NewServer(hap.NewMemStore(), bridge.A, as...); err != nil {
		t.Fatal(err)
	}
	for i, a := range as {
		if a.Id != uint64(i+2) {
			t.Errorf("accessory %d has AID %d; the server must keep assigned AIDs", i, a.Id)
		}
	}
}
