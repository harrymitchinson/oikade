// Package hometest tests a home like the software it is: set device values, move a fake clock and
// look at what your automations did. It runs the real engine, arbiters and planner in your test, with
// no host and no hardware.
//
//	func TestFanRunsWhenItsHumid(t *testing.T) {
//		h := hometest.New(t, home(), hometest.Options{})
//		h.Set(BathroomSensor, "sensor.relative-humidity", 80.0)
//		h.ExpectValue(BathroomFan, "switch.on", true)
//		h.Advance(time.Hour)
//	}
package hometest

import (
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/harrymitchinson/oikade/framework"
	"github.com/harrymitchinson/oikade/internal/simhome"
)

// Device is a device in the simulated home.
type Device struct {
	Name, Room   string
	Capabilities []string
	// Values are its first values, by capability.
	Values map[string]any
}

// Options say what home to simulate. All of them can be left out.
type Options struct {
	// Start is when the test starts. Zero means midnight UTC on Monday 7 January 2030. Its location
	// is the home's.
	Start time.Time
	// Devices are the devices. Empty means the ones in Types.
	Devices []Device
	// Types is the file oikade types wrote, which a test finds beside itself. Empty means devices.go.
	Types string
}

// Write is something your automations wrote to a device.
type Write struct {
	Time               time.Time
	Device, Capability string
	Value              any
}

// Claim is a claim on an arbitrated capability.
type Claim struct {
	// Source says who holds it: Rule/<name>, or an outside change.
	Source string
	// Level is Normal, Override or Safety.
	Level string
	Value any
	// Winner is the claim that decides the value.
	Winner bool
	// Latched stays until a Release.
	Latched bool
}

// Home is a simulated home for one test.
type Home struct {
	t   testing.TB
	sim *simhome.Sim
}

// New starts a simulated home, which stops with the test.
func New(t testing.TB, home framework.Home, o Options) *Home {
	t.Helper()
	if o.Start.IsZero() {
		o.Start = time.Date(2030, time.January, 7, 0, 0, 0, 0, time.UTC)
	}
	devices := make([]simhome.Device, 0, len(o.Devices))
	for _, d := range o.Devices {
		devices = append(devices, simhome.Device(d))
	}
	if len(devices) == 0 {
		if o.Types == "" {
			o.Types = "devices.go"
		}
		var err error
		if devices, err = simhome.ReadTypes(o.Types); err != nil {
			t.Fatalf("Reading the home's devices: %v. Run oikade types, or give Options.Devices", err)
		}
	}
	compiled, err := simhome.FromFramework(home)
	if err != nil {
		t.Fatalf("Your home has mistakes in it: %v", err)
	}
	sim, err := simhome.New(simhome.Options{Home: compiled, Devices: devices, Start: o.Start, Log: testLog(t)})
	if err != nil {
		t.Fatalf("Starting the simulated home: %v", err)
	}
	t.Cleanup(sim.Close)
	return &Home{t: t, sim: sim}
}

// testLog sends your automations' log to the test's, which shows a mistake in a condition.
func testLog(t testing.TB) logr.Logger {
	return funcr.New(func(prefix, args string) { t.Log(prefix, args) }, funcr.Options{})
}

// Now is the time in the home.
func (h *Home) Now() time.Time { return h.sim.Now() }

// Set changes a device from outside your automations, as its sensor or a tap in an app would, and
// waits for your automations to take it in.
func (h *Home) Set(device, capability string, value any) {
	h.t.Helper()
	if err := h.sim.Set(device, capability, value); err != nil {
		h.t.Fatal(err)
	}
}

// Advance moves the clock on and waits for your automations to catch up.
func (h *Home) Advance(d time.Duration) {
	h.t.Helper()
	if err := h.sim.Advance(d); err != nil {
		h.t.Fatal(err)
	}
}

// Restart crashes your automations and starts them again. Flows carry on from where they'd got to.
func (h *Home) Restart() {
	h.t.Helper()
	if err := h.sim.Restart(); err != nil {
		h.t.Fatal(err)
	}
}

// Value reads a device's value, or nil if it has none yet.
func (h *Home) Value(device, capability string) any {
	v, _ := h.sim.Value(device, capability)
	return v
}

// Writes are what your automations have written, oldest first.
func (h *Home) Writes() []Write {
	writes := h.sim.Writes()
	out := make([]Write, 0, len(writes))
	for _, w := range writes {
		out = append(out, Write(w))
	}
	return out
}

// Claims are the claims on an arbitrated capability.
func (h *Home) Claims(device, capability string) []Claim {
	claims := h.sim.Claims(device, capability)
	out := make([]Claim, 0, len(claims))
	for _, c := range claims {
		out = append(out, Claim(c))
	}
	return out
}

// Finished says a flow or moment has run to the end, successfully, with no run left.
func (h *Home) Finished(rule string) bool { return h.sim.Finished(rule) }

// ExpectValue fails the test unless a device has this value.
func (h *Home) ExpectValue(device, capability string, want any) {
	h.t.Helper()
	if got := h.Value(device, capability); !same(got, want) {
		h.t.Fatalf("%s %s is %v, want %v", device, capability, got, want)
	}
}

// ExpectWrite fails the test unless your automations have written this value to a device.
func (h *Home) ExpectWrite(device, capability string, want any) {
	h.t.Helper()
	for _, w := range h.Writes() {
		if w.Device == device && w.Capability == capability && same(w.Value, want) {
			return
		}
	}
	h.t.Fatalf("nothing wrote %v to %s %s. Writes: %v", want, device, capability, h.Writes())
}

// ExpectClaim fails the test unless source holds a claim at this level.
func (h *Home) ExpectClaim(device, capability, source, level string) {
	h.t.Helper()
	claims := h.Claims(device, capability)
	for _, c := range claims {
		if c.Source == source && c.Level == level {
			return
		}
	}
	h.t.Fatalf("%s holds no %s claim on %s %s. Claims: %+v", source, level, device, capability, claims)
}

// ExpectFinished fails the test unless a flow or moment finished.
func (h *Home) ExpectFinished(rule string) {
	h.t.Helper()
	if !h.Finished(rule) {
		h.t.Fatalf("%s hasn't finished. How it ended otherwise: %v", rule, h.sim.Failed(rule))
	}
}

// same compares values, taking an int for the number a device reports.
func same(got, want any) bool {
	return fmt.Sprint(got) == fmt.Sprint(want)
}
