package oikade_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade"
	"github.com/harrymitchinson/oikade/hometest"
)

// The devices, as oikade types writes them into devices.go.
var (
	BathroomFan = struct {
		oikade.Device
		On oikade.Bool
	}{
		Name: "bathroom-fan",
		On:   oikade.Bool{Device: "bathroom-fan", Capability: oikade.On},
	}
	BathroomSensor = struct {
		oikade.Device
		Humidity oikade.Number
	}{
		Name:     "bathroom-sensor",
		Humidity: oikade.Number{Device: "bathroom-sensor", Capability: oikade.Humidity},
	}
)

// A home decides what sets each capability. In main, home.Run() runs it on the host. Here hometest
// runs it on a fake clock instead, as a test of it would.
func ExampleNewHome() {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(BathroomFan.On).
		Case("humid", true, BathroomSensor.Humidity.Above(70)).Linger(5 * time.Minute).
		Default(false)

	h := hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{Name: "bathroom-fan", Capabilities: []string{string(oikade.On)}},
		{Name: "bathroom-sensor", Capabilities: []string{string(oikade.Humidity)}},
	}})
	h.Set(BathroomSensor.Humidity, 80)
	fmt.Println(h.Value(BathroomFan.On))
	h.Set(BathroomSensor.Humidity, 50)
	h.Advance(5 * time.Minute)
	fmt.Println(h.Value(BathroomFan.On))
	// Output:
	// true true
	// false true
}

// A flow is ordinary Go that runs when its trigger fires and its condition holds, and can wait for
// hours.
func ExampleHome_Flow() {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("shower", func(r *oikade.FlowRun) {
		r.Set(BathroomFan.On.To(true))
		r.Sleep(20 * time.Minute)
		r.Set(BathroomFan.On.To(false))
	}).When(BathroomSensor.Humidity).If(BathroomSensor.Humidity.Above(70))

	on, humidity := string(oikade.On), string(oikade.Humidity)
	h := hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{Name: "bathroom-fan", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "bathroom-sensor", Capabilities: []string{humidity}, Values: map[string]any{humidity: 50.0}},
	}})
	h.Set(BathroomSensor.Humidity, 85)
	for _, w := range h.Writes() {
		fmt.Println(w.Time.Format("15:04"), w.Device, w.Capability, w.Value)
	}
	h.Advance(20 * time.Minute)
	fmt.Println(h.Finished("shower"))
	// Output:
	// 00:00 bathroom-fan on true
	// true
}

// t stands in, in the examples, for the *testing.T a test is given. A failure panics with its
// message, which fails the example. The examples' homes stop when the test binary exits.
var t testing.TB = exampleT{}

type exampleT struct{ testing.TB }

func (exampleT) Helper()                        {}
func (exampleT) Log(...any)                     {}
func (exampleT) Cleanup(func())                 {}
func (exampleT) Fatal(args ...any)              { panic(fmt.Sprint(args...)) }
func (exampleT) Fatalf(format string, a ...any) { panic(fmt.Sprintf(format, a...)) }
