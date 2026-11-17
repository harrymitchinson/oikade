// Package hometest tests a home like the software it is: set device values, move a fake clock and
// look at what your automations did. It runs the real engine, arbiters and planner in your test, with
// no host and no hardware.
//
//	func TestPorchLightFollowsThePorchSensor(t *testing.T) {
//		h := hometest.New(t, home(), hometest.Options{})
//
//		h.Set(PorchSensor.Occupied, true)
//		h.ExpectValue(PorchLight.On, true)
//
//		h.Set(PorchSensor.Occupied, false)
//		h.Advance(4 * time.Minute)
//		h.ExpectValue(PorchLight.On, true)
//
//		h.Advance(2 * time.Minute)
//		h.ExpectValue(PorchLight.On, false)
//	}
package hometest

import (
	"log/slog"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/simhome"
)

// Device is a device in the simulated home.
type Device struct {
	Name, Room   string
	Capabilities []string
	// DeviceType is what a device that's on or off is, as Declaration.Is would say. Empty is
	// what its capabilities say: a dimmable light with oikade.Brightness, and a plug without.
	DeviceType oikade.DeviceType
	// Values are its first values, by capability.
	Values map[string]any
}

// Options say what home to simulate. All of them can be left out.
type Options struct {
	// Start is when the test starts. Zero means midnight UTC on Monday 7 January 2030. Its location
	// is the home's.
	Start time.Time
	// Devices are the devices. Empty means the ones in Types, in the rooms and of the types your home
	// declares.
	Devices []Device
	// Types is the file oikade types wrote, which a test finds beside itself. Empty means devices.go.
	Types string
}

// Write is something your automations wrote to a device.
type Write struct {
	Time               time.Time
	Device, Capability string
	// Value is a bool, a float64 or an oikade.ClimateMode.
	Value any
}

// Claim is a claim on an arbitrated capability.
type Claim struct {
	// Source is the ref of who holds it: a case's, such as case/porch-light/on/motion, a flow's,
	// such as flow/arrival, or matter, device or command for an outside change.
	Source string
	// Level is Normal, Override or Safety.
	Level string
	// Value is a bool, a float64 or an oikade.ClimateMode.
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
func New(t testing.TB, home *oikade.Home, o Options) *Home {
	t.Helper()
	if o.Start.IsZero() {
		o.Start = time.Date(2030, time.January, 7, 0, 0, 0, 0, time.UTC)
	}
	devices := make([]simhome.Device, 0, len(o.Devices))
	for _, d := range o.Devices {
		if d.DeviceType != "" && !devicetype.Choosable(devicetype.Type(d.DeviceType)) {
			t.Fatalf("Device %s has device type %q: use one of %s", d.Name, d.DeviceType, devicetype.Join(devicetype.OnOff))
		}
		devices = append(devices, simhome.Device{
			Name: d.Name, Room: d.Room, Capabilities: d.Capabilities, Values: d.Values,
			DeviceTypes: devicetype.Of(devicetype.Type(d.DeviceType), "", d.Capabilities),
		})
	}
	compiled, err := simhome.FromHome(home)
	if err != nil {
		t.Fatalf("Your home has mistakes in it: %v", err)
	}
	if len(devices) == 0 {
		if o.Types == "" {
			o.Types = "devices.go"
		}
		if devices, err = simhome.ReadTypes(o.Types, compiled.Inventory); err != nil {
			t.Fatalf("Reading the home's devices: %v. Run oikade types, or give Options.Devices", err)
		}
	}
	sim, err := simhome.New(simhome.Options{Home: compiled, Devices: devices, Start: o.Start, Log: testLog(t)})
	if err != nil {
		t.Fatalf("Starting the simulated home: %v", err)
	}
	t.Cleanup(sim.Close)
	return &Home{t: t, sim: sim}
}

// testLog sends your automations' log to the test's, which shows a mistake in a condition.
func testLog(t testing.TB) *slog.Logger {
	t.Helper()
	return logging.Test(t)
}

// Now is the time in the home.
func (h *Home) Now() time.Time { return h.sim.Now() }

// Set changes a device from outside your automations, as its sensor or a tap in an app would, and
// waits for your automations to take it in.
func (h *Home) Set[T oikade.Value](c oikade.Ref[T], value T) {
	h.t.Helper()
	// A Set is the value as the host holds it: a mode is its name.
	device, name, v, err := simhome.FromSet(c.To(value))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.sim.Set(device, name, v); err != nil {
		h.t.Fatal(err)
	}
}

// names are the device and capability c names.
func (h *Home) names[T oikade.Value](c oikade.Ref[T]) (device, name string) {
	h.t.Helper()
	var zero T
	device, name, _, err := simhome.FromSet(c.To(zero))
	if err != nil {
		h.t.Fatal(err)
	}
	return device, name
}

// Report changes several values of one device at once, as one message from it would, and waits for
// your automations to take it in. Your automations see every value before any of them acts, as
// they do on a real host:
//
//	h.Report(HallSensor.Humidity.To(72), HallSensor.Occupied.To(true))
func (h *Home) Report(values ...oikade.Set) {
	h.t.Helper()
	var device string
	report := map[string]any{}
	for _, set := range values {
		dev, name, v, err := simhome.FromSet(set)
		if err != nil {
			h.t.Fatal(err)
		}
		if device != "" && dev != device {
			h.t.Fatalf("Report has %s and %s: a report is one device's", device, dev)
		}
		if _, twice := report[name]; twice {
			h.t.Fatalf("Report has %s's %s twice", dev, name)
		}
		device, report[name] = dev, v
	}
	if device == "" {
		h.t.Fatal("Report has no values")
	}
	if err := h.sim.Report(device, report); err != nil {
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

// Value reads a device's value, and whether it has one yet.
func (h *Home) Value[T oikade.Value](c oikade.Ref[T]) (T, bool) {
	h.t.Helper()
	var zero T
	device, name := h.names(c)
	v, ok := h.sim.Value(device, name)
	if !ok {
		return zero, false
	}
	t, ok := as[T](name, v)
	return t, ok
}

// public is a value the simulated home holds as a home writes it: a mode's name is a ClimateMode.
func public(v any) any {
	if mode, ok := v.(string); ok {
		return oikade.ClimateMode(mode)
	}
	return v
}

// as is a value the simulated home holds as T, which takes an int for a number, and a mode's name.
func as[T oikade.Value](name string, v any) (T, bool) {
	if valid, err := capability.Validate(name, v); err == nil {
		v = valid
	}
	t, ok := public(v).(T)
	return t, ok
}

// Writes are what your automations have written, oldest first.
func (h *Home) Writes() []Write {
	writes := h.sim.Writes()
	out := make([]Write, 0, len(writes))
	for _, w := range writes {
		w.Value = public(w.Value)
		out = append(out, Write(w))
	}
	return out
}

// Claims are the claims on an arbitrated capability.
func (h *Home) Claims[T oikade.Value](c oikade.Ref[T]) []Claim {
	h.t.Helper()
	claims := h.sim.Claims(h.names(c))
	out := make([]Claim, 0, len(claims))
	for _, c := range claims {
		c.Value = public(c.Value)
		out = append(out, Claim(c))
	}
	return out
}

// Finished says a flow has run to the end, successfully, with no run left.
func (h *Home) Finished(rule string) bool { return h.sim.Finished(rule) }

// ExpectValue fails the test unless a device has this value.
func (h *Home) ExpectValue[T oikade.Value](c oikade.Ref[T], want T) {
	h.t.Helper()
	if got, ok := h.Value(c); ok && got == want {
		return
	}
	device, name := h.names(c)
	if raw, ok := h.sim.Value(device, name); ok {
		h.t.Fatalf("%s %s is %v, want %v", device, name, raw, want)
	}
	h.t.Fatalf("%s %s has no value, want %v", device, name, want)
}

// ExpectWrite fails the test unless your automations have written this value to a device.
func (h *Home) ExpectWrite[T oikade.Value](c oikade.Ref[T], want T) {
	h.t.Helper()
	device, name := h.names(c)
	for _, w := range h.Writes() {
		if w.Device != device || w.Capability != name {
			continue
		}
		if got, ok := as[T](w.Capability, w.Value); ok && got == want {
			return
		}
	}
	h.t.Fatalf("nothing wrote %v to %s %s. Writes: %v", want, device, name, h.Writes())
}

// ExpectClaim fails the test unless source, a ref such as case/porch-light/on/motion or
// flow/arrival, holds a claim at this level.
func (h *Home) ExpectClaim[T oikade.Value](c oikade.Ref[T], source, level string) {
	h.t.Helper()
	claims := h.Claims(c)
	for _, claim := range claims {
		if claim.Source == source && claim.Level == level {
			return
		}
	}
	device, name := h.names(c)
	h.t.Fatalf("%s holds no %s claim on %s %s. Claims: %+v", source, level, device, name, claims)
}

// ExpectFinished fails the test unless a flow finished.
func (h *Home) ExpectFinished(rule string) {
	h.t.Helper()
	if !h.Finished(rule) {
		h.t.Fatalf("%s hasn't finished. How it ended otherwise: %v", rule, h.sim.Failed(rule))
	}
}
