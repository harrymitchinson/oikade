package matterbridge

import (
	"reflect"
	"slices"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

type setCall struct {
	name  string
	value any
}

func mapDevice(t *testing.T, typ devicetype.Type, caps ...string) (*Mapped, *[]setCall) {
	t.Helper()
	var calls []setCall
	m := Map(Device{Key: ident.ID{Name: "d"}, Endpoint: 7, Name: "D", Capabilities: caps, Type: typ},
		func(name string, value any) { calls = append(calls, setCall{name, value}) })
	if m == nil {
		t.Fatalf("%v didn't map", caps)
	}
	return m, &calls
}

func ids(dts []im.DeviceType) []uint32 {
	out := make([]uint32, 0, len(dts))
	for _, d := range dts {
		out = append(out, d.ID)
	}
	return out
}

func cluster(t *testing.T, m *Mapped, id uint32) *im.Cluster {
	t.Helper()
	for _, c := range m.Clusters {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no cluster %#x", id)
	return nil
}

// read reads an attribute's value straight from its cluster.
func read(t *testing.T, c *im.Cluster, attr uint32) any {
	t.Helper()
	for _, a := range c.Attributes {
		if a.ID == attr {
			var w tlv.Writer
			if err := a.Read(nil, &w, tlv.Anonymous); err != nil {
				t.Fatal(err)
			}
			v, err := im.DecodeValue(w.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("cluster %#x has no attribute %#x", c.ID, attr)
	return nil
}

// invoke runs a command straight on its cluster, with fields written by f (nil for none).
func invoke(t *testing.T, c *im.Cluster, cmd uint32, f func(w *tlv.Writer)) {
	t.Helper()
	var r *tlv.Reader
	if f != nil {
		var w tlv.Writer
		w.StartStructure(tlv.Anonymous)
		f(&w)
		w.End()
		r = tlv.NewReader(w.Bytes())
		if err := r.Next(); err != nil {
			t.Fatal(err)
		}
	}
	for _, x := range c.Commands {
		if x.ID == cmd {
			if _, err := x.Invoke(nil, r); err != nil {
				t.Fatalf("command %#x: %v", cmd, err)
			}
			return
		}
	}
	t.Fatalf("cluster %#x has no command %#x", c.ID, cmd)
}

func TestDeviceTypes(t *testing.T) {
	for name, tc := range map[string]struct {
		typ  devicetype.Type
		caps []string
		want []uint32
	}{
		// A device that doesn't say what it is is a plug, and with a brightness a light.
		"on alone shown as a plug": {"", []string{capability.On}, []uint32{0x010A}},
		"dimmable light":           {"", []string{capability.On, capability.Brightness}, []uint32{0x0101}},
		"on/off light":             {devicetype.OnOffLight, []string{capability.On}, []uint32{0x0100}},
		"switch as a wall control": {devicetype.MountedOnOffControl, []string{capability.On}, []uint32{0x010F}},
		"dimmer in the wall":       {devicetype.MountedDimmableLoadControl, []string{capability.On, capability.Brightness}, []uint32{0x0110}},
		"dimmable light as a plug": {devicetype.DimmablePlugInUnit, []string{capability.On, capability.Brightness}, []uint32{0x010B}},
		"nothing to dim":           {devicetype.DimmableLight, []string{capability.On}, []uint32{0x0100}},
		"motion with temperature and battery": {"", []string{capability.Occupied, capability.Temperature, capability.Battery, capability.LinkQuality},
			[]uint32{0x0107, 0x0302, 0x0011}},
		"contact":  {"", []string{capability.Open}, []uint32{0x0015}},
		"humidity": {"", []string{capability.Humidity, capability.Temperature}, []uint32{0x0302, 0x0307}},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := mapDevice(t, tc.typ, tc.caps...)
			if got := ids(m.Types); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("device types %#x, want %#x", got, tc.want)
			}
		})
	}
}

func TestUnsupported(t *testing.T) {
	for _, caps := range [][]string{nil, {capability.LinkQuality}, {capability.Battery, capability.LinkQuality}} {
		if Supported(caps) || Map(Device{Capabilities: caps}, nil) != nil {
			t.Errorf("%v mapped", caps)
		}
	}
}

func TestDimmableLight(t *testing.T) {
	m, calls := mapDevice(t, "", capability.On, capability.Brightness)
	onoff, level := cluster(t, m, clusters.IDOnOff), cluster(t, m, clusters.IDLevelControl)
	if onoff.FeatureMap&1 == 0 {
		t.Error("a light's On/Off has no Lighting feature")
	}

	// The bus reaches the attributes.
	m.Update(capability.On, true)
	m.Update(capability.Brightness, 50.0)
	if read(t, onoff, 0) != true || read(t, level, 0) != uint64(128) {
		t.Errorf("OnOff %v, CurrentLevel %v", read(t, onoff, 0), read(t, level, 0))
	}
	m.Update(capability.Brightness, 100.0)
	if read(t, level, 0) != uint64(254) {
		t.Errorf("CurrentLevel at 100%% %v", read(t, level, 0))
	}

	// Commands become writes.
	invoke(t, onoff, 0x00, nil)                  // Off
	invoke(t, level, 0x04, func(w *tlv.Writer) { // MoveToLevelWithOnOff
		w.PutUint(tlv.Context(0), 1)
		w.PutUint(tlv.Context(1), 0)
		w.PutUint(tlv.Context(2), 0)
		w.PutUint(tlv.Context(3), 0)
	})
	invoke(t, level, 0x00, func(w *tlv.Writer) { // MoveToLevel
		w.PutUint(tlv.Context(0), 254)
		w.PutUint(tlv.Context(1), 0)
		w.PutUint(tlv.Context(2), 0)
		w.PutUint(tlv.Context(3), 0)
	})
	invoke(t, level, 0x04, func(w *tlv.Writer) { // MoveToLevelWithOnOff, one step above the minimum
		w.PutUint(tlv.Context(0), 2)
		w.PutUint(tlv.Context(1), 0)
		w.PutUint(tlv.Context(2), 0)
		w.PutUint(tlv.Context(3), 0)
	})
	want := []setCall{
		{capability.On, false},
		{capability.On, false}, // the minimum level with On/Off turns the light off, at its old level
		{capability.Brightness, 100.0},
		{capability.Brightness, 1.0}, {capability.On, true}, // not 0%, which would read as off
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
}

func TestSwitchAsPlug(t *testing.T) {
	m, calls := mapDevice(t, "", capability.On)
	onoff := cluster(t, m, clusters.IDOnOff)
	if onoff.FeatureMap != 0 {
		t.Errorf("a plug's On/Off has features %#x", onoff.FeatureMap)
	}
	m.Update(capability.On, true)
	invoke(t, onoff, 0x02, nil) // Toggle
	if read(t, onoff, 0) != false || !reflect.DeepEqual(*calls, []setCall{{capability.On, false}}) {
		t.Errorf("OnOff %v, writes %v", read(t, onoff, 0), *calls)
	}
}

func TestSensors(t *testing.T) {
	m, _ := mapDevice(t, "", capability.Occupied, capability.Open, capability.Temperature, capability.Humidity, capability.Battery)
	temp := cluster(t, m, clusters.IDTemperatureMeasurement)
	if read(t, temp, 0) != nil {
		t.Errorf("temperature before any report %v, want null", read(t, temp, 0))
	}
	m.Update(capability.Occupied, true)
	m.Update(capability.Open, true)
	m.Update(capability.Temperature, 19.55)
	m.Update(capability.Humidity, 61.2)
	m.Update(capability.Battery, 15.0)
	m.Update(capability.LinkQuality, 80.0) // not mapped: ignored

	for _, tc := range []struct {
		cluster, attr uint32
		want          any
	}{
		{clusters.IDOccupancySensing, 0, uint64(1)},
		{clusters.IDBooleanState, 0, false}, // open: no contact
		{clusters.IDTemperatureMeasurement, 0, int64(1955)},
		{clusters.IDRelativeHumidityMeasurement, 0, uint64(6120)},
		{clusters.IDPowerSource, 0x000C, uint64(30)}, // half percent
		{clusters.IDPowerSource, 0x000E, uint64(1)},  // Warning
	} {
		if got := read(t, cluster(t, m, tc.cluster), tc.attr); got != tc.want {
			t.Errorf("%#x/%#x = %#v, want %#v", tc.cluster, tc.attr, got, tc.want)
		}
	}
	if eps := read(t, cluster(t, m, clusters.IDPowerSource), 0x001F).([]any); !reflect.DeepEqual(eps, []any{uint64(7)}) {
		t.Errorf("EndpointList %v", eps)
	}
}

func TestLabelAndUniqueID(t *testing.T) {
	if l := Label("Wohnzimmer Stehlampe beim Fenster links"); len(l) > 32 {
		t.Errorf("label %q is %d bytes", l, len(l))
	}
	if l := Label("Küche Hängeleuchte über der Insel"); len(l) > 32 || !utf8.ValidString(l) || l != "Küche Hängeleuchte über der I" {
		t.Errorf("label %q", l)
	}
	// Controllers remember a device by this, so the hash of a given name must not change.
	if got := UniqueID("bridge/7"); got != "A62B0CAF14C80ED5F203011F658071EF" {
		t.Errorf("UniqueID = %q", got)
	}
	a, b := UniqueID("lamp"), UniqueID("lamp2")
	if len(a) != 32 || a == b || a != UniqueID("lamp") {
		t.Errorf("unique IDs %q, %q", a, b)
	}
}

// The bridge shows each capability as the attribute the registry names for it.
func TestTheBridgeShowsTheRegistrysMatterAttributes(t *testing.T) {
	for _, name := range capability.Names() {
		spec, _ := capability.Lookup(name)
		if spec.Matter == nil {
			continue
		}
		caps := []string{name}
		switch name {
		case capability.Brightness:
			caps = append(caps, capability.On) // A brightness belongs to a light.
		case capability.Battery:
			caps = append(caps, capability.Occupied) // A battery powers a device.
		case capability.Mode:
			caps = append(caps, capability.Setpoint) // A mode is an air conditioner's.
		}
		m, _ := mapDevice(t, "", caps...)
		c := cluster(t, m, spec.Matter.ClusterID)
		if !slices.ContainsFunc(c.Attributes, func(a im.Attribute) bool { return a.ID == spec.Matter.AttributeID }) {
			t.Errorf("%s: cluster %#x has no attribute %#x", name, spec.Matter.ClusterID, spec.Matter.AttributeID)
		}
	}
}

// A shut door is open false, which Boolean State shows as true: contact. The registry says it's
// inverted, and so the docs do.
func TestAShutDoorIsBooleanStateTrue(t *testing.T) {
	spec, _ := capability.Lookup(capability.Open)
	m, _ := mapDevice(t, "", capability.Open)
	m.Update(capability.Open, false)
	if got := read(t, cluster(t, m, spec.Matter.ClusterID), spec.Matter.AttributeID); got != spec.Matter.Inverted {
		t.Fatalf("Boolean State is %v for a shut door, and the registry says inverted is %v", got, spec.Matter.Inverted)
	}
}

// A device you say is an on/off light is one, brightness or not: Level Control would make it
// dimmable.
func TestAnOnOffTypeLeavesOutLevelControl(t *testing.T) {
	m, _ := mapDevice(t, devicetype.OnOffLight, capability.On, capability.Brightness)
	if got := ids(m.Types); !slices.Equal(got, []uint32{0x0100}) {
		t.Errorf("device types %#x", got)
	}
	for _, c := range m.Clusters {
		if c.ID == clusters.IDLevelControl {
			t.Error("an on/off light has Level Control")
		}
	}
	if cluster(t, m, clusters.IDOnOff).FeatureMap&1 == 0 {
		t.Error("a light's On/Off has no Lighting feature")
	}
	m.Update(capability.Brightness, 40.0) // Nothing to update, and nothing breaks.
}

// write writes an attribute straight on its cluster, with the value put by f.
func write(t *testing.T, c *im.Cluster, attr uint32, f func(w *tlv.Writer)) im.Status {
	t.Helper()
	for _, a := range c.Attributes {
		if a.ID == attr {
			if a.Write == nil {
				return im.StatusUnsupportedWrite
			}
			var w tlv.Writer
			f(&w)
			r := tlv.NewReader(w.Bytes())
			if err := r.Next(); err != nil {
				t.Fatal(err)
			}
			return a.Write(nil, r, im.Replace)
		}
	}
	t.Fatalf("cluster %#x has no attribute %#x", c.ID, attr)
	return 0
}

func putUint(v uint64) func(w *tlv.Writer) {
	return func(w *tlv.Writer) { w.PutUint(tlv.Anonymous, v) }
}

func putInt(v int64) func(w *tlv.Writer) {
	return func(w *tlv.Writer) { w.PutInt(tlv.Anonymous, v) }
}

func TestAHeaterIsAHeatOnlyThermostat(t *testing.T) {
	m, calls := mapDevice(t, devicetype.OnOffPlugInUnit, capability.On, capability.Setpoint, capability.Temperature)
	if got := ids(m.Types); !reflect.DeepEqual(got, []uint32{0x0301}) {
		t.Errorf("device types %#x, want only Thermostat", got)
	}
	for _, c := range m.Clusters {
		if c.ID == clusters.IDOnOff || c.ID == clusters.IDTemperatureMeasurement {
			t.Errorf("a heater has cluster %#x", c.ID)
		}
	}
	th := cluster(t, m, clusters.IDThermostat)
	if th.FeatureMap != 1 {
		t.Errorf("features %#x, want only Heating", th.FeatureMap)
	}
	const local, setpoint, control, mode = 0x0000, 0x0012, 0x001B, 0x001C
	if read(t, th, control) != uint64(2) {
		t.Errorf("ControlSequenceOfOperation %v, want HeatingOnly", read(t, th, control))
	}
	if read(t, th, local) != nil {
		t.Errorf("LocalTemperature before any report %v, want null", read(t, th, local))
	}
	for attr, want := range map[uint32]int64{0x0003: 500, 0x0004: 3500, 0x0015: 500, 0x0016: 3500} {
		if got := read(t, th, attr); got != want {
			t.Errorf("limit %#x is %v, want %d", attr, got, want)
		}
	}

	// The bus reaches the attributes.
	m.Update(capability.Temperature, 19.46)
	m.Update(capability.Setpoint, 21.5)
	m.Update(capability.On, true)
	if read(t, th, local) != int64(1946) || read(t, th, setpoint) != int64(2150) || read(t, th, mode) != uint64(4) {
		t.Errorf("LocalTemperature %v, setpoint %v, SystemMode %v", read(t, th, local), read(t, th, setpoint), read(t, th, mode))
	}
	m.Update(capability.Setpoint, 45.0)
	if read(t, th, setpoint) != int64(3500) {
		t.Errorf("a setpoint above the limits shows as %v", read(t, th, setpoint))
	}

	// Writes become capabilities, and a mode a heater can't do is refused.
	if s := write(t, th, setpoint, putInt(1800)); s != im.StatusSuccess {
		t.Errorf("setpoint write: %v", s)
	}
	for _, v := range []int64{499, 3501} {
		if s := write(t, th, setpoint, putInt(v)); s != im.StatusConstraintError {
			t.Errorf("setpoint %d: %v, want a constraint error", v, s)
		}
	}
	if s := write(t, th, mode, putUint(0)); s != im.StatusSuccess {
		t.Errorf("SystemMode Off: %v", s)
	}
	if s := write(t, th, mode, putUint(4)); s != im.StatusSuccess {
		t.Errorf("SystemMode Heat: %v", s)
	}
	for _, v := range []uint64{1, 3, 7, 8} {
		if s := write(t, th, mode, putUint(v)); s != im.StatusConstraintError {
			t.Errorf("SystemMode %d: %v, want a constraint error", v, s)
		}
	}
	if s := write(t, th, control, putUint(4)); s != im.StatusConstraintError {
		t.Errorf("ControlSequenceOfOperation CoolingAndHeating: %v, want a constraint error", s)
	}
	invoke(t, th, 0x00, func(w *tlv.Writer) { // SetpointRaiseLower, Heat, up 1.5 °C
		w.PutUint(tlv.Context(0), 0)
		w.PutInt(tlv.Context(1), 15)
	})
	want := []setCall{{capability.Setpoint, 18.0}, {capability.On, false}, {capability.On, true}, {capability.Setpoint, 19.5}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
}

func TestAHeaterWithoutOnAlwaysHeats(t *testing.T) {
	m, calls := mapDevice(t, "", capability.Setpoint)
	th := cluster(t, m, clusters.IDThermostat)
	if read(t, th, 0x001C) != uint64(4) {
		t.Errorf("SystemMode %v, want Heat", read(t, th, 0x001C))
	}
	if s := write(t, th, 0x001C, putUint(0)); s != im.StatusConstraintError {
		t.Errorf("SystemMode Off: %v, want a constraint error", s)
	}
	if s := write(t, th, 0x001C, putUint(4)); s != im.StatusSuccess {
		t.Errorf("SystemMode Heat: %v", s)
	}
	// LocalTemperature is an int16 of hundredths, so a faulty reading stops at its top.
	m.Update(capability.Temperature, 400.0)
	if read(t, th, 0x0000) != int64(32767) {
		t.Errorf("LocalTemperature at 400 °C %v", read(t, th, 0x0000))
	}
	if len(*calls) != 0 {
		t.Errorf("writes %v", *calls)
	}
}

// LockDoor answers at once, and the controller sees Locked only once the bolt reports it.
func TestALockSaysLockedOnlyOnceTheBoltReports(t *testing.T) {
	m, calls := mapDevice(t, "", capability.Locked, capability.Battery)
	if got := ids(m.Types); !reflect.DeepEqual(got, []uint32{0x000A, 0x0011}) {
		t.Errorf("device types %#x, want a Door Lock with a battery", got)
	}
	lock := cluster(t, m, clusters.IDDoorLock)
	for _, c := range lock.Commands {
		if (c.ID == clusters.CommandLockDoor || c.ID == clusters.CommandUnlockDoor) && !c.Timed {
			t.Errorf("command %#x isn't timed", c.ID)
		}
	}
	if read(t, lock, 0) != nil {
		t.Errorf("LockState before any report %v, want null", read(t, lock, 0))
	}
	invoke(t, lock, clusters.CommandLockDoor, nil)
	if read(t, lock, 0) != nil {
		t.Errorf("LockState after LockDoor, before a report, %v", read(t, lock, 0))
	}
	m.Update(capability.Locked, true)
	if read(t, lock, 0) != uint64(clusters.LockStateLocked) {
		t.Errorf("LockState once locked %v", read(t, lock, 0))
	}
	invoke(t, lock, clusters.CommandUnlockDoor, nil)
	if read(t, lock, 0) != uint64(clusters.LockStateLocked) {
		t.Errorf("LockState after UnlockDoor, before a report, %v", read(t, lock, 0))
	}
	m.Update(capability.Locked, false)
	if read(t, lock, 0) != uint64(clusters.LockStateUnlocked) {
		t.Errorf("LockState once unlocked %v", read(t, lock, 0))
	}
	if want := []setCall{{capability.Locked, true}, {capability.Locked, false}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
}

// coveringOn maps a blind whose moves settle when the test says, and returns it, its cluster, its
// writes and the settles waiting.
func coveringOn(t *testing.T) (*Mapped, *im.Cluster, *[]setCall, *[]func()) {
	t.Helper()
	var calls []setCall
	var settles []func()
	m := Map(Device{
		Key: ident.ID{Name: "blind"}, Endpoint: 7, Name: "Blind", Capabilities: []string{capability.Position},
		After: func(d time.Duration, f func()) func() {
			if d != capability.SettleOf(capability.Position) {
				t.Errorf("a move settles after %v", d)
			}
			settles = append(settles, f)
			return func() {}
		},
	}, func(name string, value any) { calls = append(calls, setCall{name, value}) })
	if got := ids(m.Types); !reflect.DeepEqual(got, []uint32{0x0202}) {
		t.Fatalf("device types %#x, want a Window Covering", got)
	}
	return m, cluster(t, m, clusters.IDWindowCovering), &calls, &settles
}

const (
	currentLift, targetLift, operational = 0x000E, 0x000B, 0x000A
	closingStatus                        = clusters.MovementClosing | clusters.MovementClosing<<2
)

func goToLift(hundredths uint64) func(w *tlv.Writer) {
	return func(w *tlv.Writer) { w.PutUint(tlv.Context(1), hundredths) }
}

// goToLift0 is GoToLiftPercentage as controllers send it, in field 0.
func goToLift0(hundredths uint64) func(w *tlv.Writer) {
	return func(w *tlv.Writer) { w.PutUint(tlv.Context(0), hundredths) }
}

func TestABlindMovesToWhereTheControllerSaysAndShowsOnlyWhatItReports(t *testing.T) {
	m, c, calls, settles := coveringOn(t)
	if c.FeatureMap != 0b101 {
		t.Errorf("features %#b, want Lift and PositionAwareLift", c.FeatureMap)
	}
	if read(t, c, currentLift) != nil || read(t, c, targetLift) != nil {
		t.Errorf("before any report: current %v, target %v", read(t, c, currentLift), read(t, c, targetLift))
	}
	m.Update(capability.Position, 0.0)
	invoke(t, c, clusters.CommandGoToLiftPercentage, goToLift0(7000)) // 70% closed
	if want := []setCall{{capability.Position, 70.0}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
	if read(t, c, targetLift) != uint64(7000) || read(t, c, currentLift) != uint64(0) {
		t.Errorf("after the command: target %v, current %v", read(t, c, targetLift), read(t, c, currentLift))
	}
	if read(t, c, operational) != uint64(closingStatus) {
		t.Errorf("OperationalStatus %v while it moves, want closing", read(t, c, operational))
	}
	m.Update(capability.Position, 40.0)
	if read(t, c, currentLift) != uint64(4000) || read(t, c, operational) != uint64(closingStatus) {
		t.Errorf("on its way: current %v, OperationalStatus %v", read(t, c, currentLift), read(t, c, operational))
	}
	m.Update(capability.Position, 70.0)
	if read(t, c, currentLift) != uint64(7000) || read(t, c, operational) != uint64(0) {
		t.Errorf("once there: current %v, OperationalStatus %v", read(t, c, currentLift), read(t, c, operational))
	}
	if read(t, c, 0x0008) != uint64(70) {
		t.Errorf("CurrentPositionLiftPercentage %v", read(t, c, 0x0008))
	}
	// The settle for a move that has finished changes nothing.
	for _, f := range *settles {
		f()
	}
	if read(t, c, targetLift) != uint64(7000) {
		t.Errorf("target %v after the settle", read(t, c, targetLift))
	}
}

func TestABlindThatNeverGetsThereStopsMovingOnceTheMoveSettles(t *testing.T) {
	m, c, _, settles := coveringOn(t)
	m.Update(capability.Position, 0.0)
	invoke(t, c, clusters.CommandDownOrClose, nil)
	m.Update(capability.Position, 45.0)
	if read(t, c, operational) != uint64(closingStatus) {
		t.Errorf("OperationalStatus %v before the move settles", read(t, c, operational))
	}
	if len(*settles) != 1 {
		t.Fatalf("%d settles waiting, want one", len(*settles))
	}
	(*settles)[0]()
	if read(t, c, operational) != uint64(0) || read(t, c, targetLift) != uint64(4500) {
		t.Errorf("once settled: OperationalStatus %v, target %v, want stopped where it is",
			read(t, c, operational), read(t, c, targetLift))
	}
}

func TestStopMotionNeedsAReportSinceTheMove(t *testing.T) {
	m, c, calls, _ := coveringOn(t)
	m.Update(capability.Position, 100.0)
	invoke(t, c, clusters.CommandUpOrOpen, nil)
	stop := func() error {
		for _, x := range c.Commands {
			if x.ID == clusters.CommandStopMotion {
				_, err := x.Invoke(nil, nil)
				return err
			}
		}
		t.Fatal("no StopMotion")
		return nil
	}
	if err := stop(); err != im.StatusFailure {
		t.Errorf("StopMotion with no report since the move: %v, want Failure", err)
	}
	m.Update(capability.Position, 60.0)
	if err := stop(); err != nil {
		t.Errorf("StopMotion: %v", err)
	}
	want := []setCall{{capability.Position, 0.0}, {capability.Position, 60.0}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
	if read(t, c, operational) != uint64(0) || read(t, c, targetLift) != uint64(6000) {
		t.Errorf("after stopping: OperationalStatus %v, target %v", read(t, c, operational), read(t, c, targetLift))
	}
}

func TestARefusedMoveIsCancelled(t *testing.T) {
	m, c, _, _ := coveringOn(t)
	m.Update(capability.Position, 20.0)
	invoke(t, c, clusters.CommandGoToLiftPercentage, goToLift(9000))
	m.Cancel(capability.Position)
	if read(t, c, operational) != uint64(0) || read(t, c, targetLift) != uint64(2000) {
		t.Errorf("after a refusal: OperationalStatus %v, target %v", read(t, c, operational), read(t, c, targetLift))
	}
}

// GoToLiftPercentage's target is in hundredths: field 0, as controllers send it, or field 1, as
// Matter 1.3's data model has it.
func TestGoToLiftPercentageTakesHundredthsInEitherField(t *testing.T) {
	_, c, calls, _ := coveringOn(t)
	invoke(t, c, clusters.CommandGoToLiftPercentage, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 5000) })
	invoke(t, c, clusters.CommandGoToLiftPercentage, func(w *tlv.Writer) {
		w.PutUint(tlv.Context(0), 25)
		w.PutUint(tlv.Context(1), 2500)
	})
	if want := []setCall{{capability.Position, 50.0}, {capability.Position, 25.0}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
}

func TestGoToLiftPercentageRefusesPastClosed(t *testing.T) {
	_, c, calls, _ := coveringOn(t)
	for _, x := range c.Commands {
		if x.ID != clusters.CommandGoToLiftPercentage {
			continue
		}
		for name, f := range map[string]func(w *tlv.Writer){
			"field 0": func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 10001) },
			"field 1": goToLift(10001),
		} {
			var w tlv.Writer
			w.StartStructure(tlv.Anonymous)
			f(&w)
			w.End()
			r := tlv.NewReader(w.Bytes())
			if err := r.Next(); err != nil {
				t.Fatal(err)
			}
			if _, err := x.Invoke(nil, r); err != im.StatusConstraintError {
				t.Errorf("%s past closed: %v, want a constraint error", name, err)
			}
		}
	}
	if len(*calls) != 0 {
		t.Errorf("writes %v", *calls)
	}
}

// A blind that has never reported has no target once its move settles.
func TestAMoveThatNeverHeardFromTheBlindLeavesNoTarget(t *testing.T) {
	_, c, _, settles := coveringOn(t)
	invoke(t, c, clusters.CommandDownOrClose, nil)
	(*settles)[0]()
	if read(t, c, targetLift) != nil || read(t, c, operational) != uint64(0) {
		t.Errorf("target %v, OperationalStatus %v", read(t, c, targetLift), read(t, c, operational))
	}
}

// The same position again, as when the bridge refreshes an endpoint, isn't a report since the move,
// so StopMotion still refuses to send the blind back.
func TestAReplayedPositionIsntAReportSinceTheMove(t *testing.T) {
	m, c, _, _ := coveringOn(t)
	m.Update(capability.Position, 0.0)
	invoke(t, c, clusters.CommandDownOrClose, nil)
	m.Update(capability.Position, 0.0)
	for _, x := range c.Commands {
		if x.ID == clusters.CommandStopMotion {
			if _, err := x.Invoke(nil, nil); err != im.StatusFailure {
				t.Errorf("StopMotion: %v, want Failure", err)
			}
		}
	}
}

func TestAnAirConditionerIsARoomAirConditioner(t *testing.T) {
	m, calls := mapDevice(t, devicetype.OnOffPlugInUnit,
		capability.On, capability.Mode, capability.Setpoint, capability.Temperature)
	if got := ids(m.Types); !reflect.DeepEqual(got, []uint32{0x0072}) {
		t.Errorf("device types %#x, want only Room Air Conditioner", got)
	}
	if f := cluster(t, m, clusters.IDOnOff).FeatureMap; f != clusters.FeatureDeadFront {
		t.Errorf("On/Off features %#x, want only DeadFrontBehavior", f)
	}
	for _, c := range m.Clusters {
		if c.ID == clusters.IDTemperatureMeasurement {
			t.Error("an air conditioner has a Temperature Measurement cluster")
		}
	}
	th := cluster(t, m, clusters.IDThermostat)
	if th.FeatureMap != 0x23 {
		t.Errorf("features %#x, want Heating, Cooling and Auto", th.FeatureMap)
	}
	const local, cooling, heating, deadBand, control, mode = 0x0000, 0x0011, 0x0012, 0x0019, 0x001B, 0x001C
	if read(t, th, control) != uint64(4) || read(t, th, deadBand) != int64(0) {
		t.Errorf("ControlSequenceOfOperation %v, MinSetpointDeadBand %v, want CoolingAndHeating and 0",
			read(t, th, control), read(t, th, deadBand))
	}
	for attr, want := range map[uint32]int64{
		0x0003: 500, 0x0004: 3500, 0x0005: 500, 0x0006: 3500, 0x0015: 500, 0x0016: 3500, 0x0017: 500, 0x0018: 3500,
	} {
		if got := read(t, th, attr); got != want {
			t.Errorf("limit %#x is %v, want %d", attr, got, want)
		}
	}

	// The bus reaches the attributes: off is SystemMode Off whatever the mode, and on shows it.
	m.Update(capability.Temperature, 26.04)
	m.Update(capability.Setpoint, 23.0)
	m.Update(capability.Mode, "dry")
	if read(t, th, mode) != uint64(0) {
		t.Errorf("SystemMode while off %v, want Off", read(t, th, mode))
	}
	m.Update(capability.On, true)
	if read(t, th, mode) != uint64(8) {
		t.Errorf("SystemMode in dry %v, want Dry (8)", read(t, th, mode))
	}
	if read(t, th, local) != int64(2604) || read(t, th, heating) != int64(2300) || read(t, th, cooling) != int64(2300) {
		t.Errorf("LocalTemperature %v, setpoints %v and %v", read(t, th, local), read(t, th, heating), read(t, th, cooling))
	}
	for name, want := range map[string]uint64{"auto": 1, "cool": 3, "heat": 4, "fan": 7, "dry": 8} {
		m.Update(capability.Mode, name)
		if got := read(t, th, mode); got != want {
			t.Errorf("SystemMode in %s %v, want %d", name, got, want)
		}
	}

	// SystemMode is the mode, then on. Off leaves the mode alone, and a mode the unit can't do is
	// refused.
	if s := write(t, th, mode, putUint(3)); s != im.StatusSuccess {
		t.Errorf("SystemMode Cool: %v", s)
	}
	if s := write(t, th, mode, putUint(0)); s != im.StatusSuccess {
		t.Errorf("SystemMode Off: %v", s)
	}
	for _, v := range []uint64{2, 5, 6, 9} {
		if s := write(t, th, mode, putUint(v)); s != im.StatusConstraintError {
			t.Errorf("SystemMode %d: %v, want a constraint error", v, s)
		}
	}
	want := []setCall{{capability.Mode, "cool"}, {capability.On, true}, {capability.On, false}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
	if read(t, th, mode) != uint64(0) {
		t.Errorf("SystemMode after Off %v", read(t, th, mode))
	}
}

// Matter's Room Air Conditioner has an On/Off, so a unit with no on is a heater, whose mode the
// bridge doesn't show.
func TestAnAirConditionerWithNoOnIsAThermostat(t *testing.T) {
	m, _ := mapDevice(t, "", capability.Mode, capability.Setpoint)
	if got := ids(m.Types); !reflect.DeepEqual(got, []uint32{0x0301}) {
		t.Errorf("device types %#x, want only Thermostat", got)
	}
}

// Both setpoints are the one setpoint. In Cool, the heating setpoint's slider isn't the target, so
// a write to it is taken and not sent, and in Heat the cooling one's. In Auto either sets it.
func TestAnAirConditionerSendsOnlyTheSetpointInUse(t *testing.T) {
	m, calls := mapDevice(t, "", capability.On, capability.Mode, capability.Setpoint)
	th := cluster(t, m, clusters.IDThermostat)
	const cooling, heating = 0x0011, 0x0012
	m.Update(capability.On, true)
	m.Update(capability.Setpoint, 24.0)

	m.Update(capability.Mode, "cool")
	if s := write(t, th, heating, putInt(1800)); s != im.StatusSuccess {
		t.Errorf("heating setpoint in Cool: %v", s)
	}
	if read(t, th, heating) != int64(2400) {
		t.Errorf("the heating setpoint moved to %v in Cool", read(t, th, heating))
	}
	if s := write(t, th, cooling, putInt(2200)); s != im.StatusSuccess {
		t.Errorf("cooling setpoint in Cool: %v", s)
	}
	if read(t, th, heating) != int64(2200) {
		t.Errorf("the heating setpoint is %v, not the cooling one's", read(t, th, heating))
	}

	for _, w := range []struct {
		mode  string
		attr  uint32
		value int64
	}{{"heat", cooling, 2600}, {"heat", heating, 2100}, {"auto", cooling, 2300}} {
		m.Update(capability.Mode, w.mode)
		if s := write(t, th, w.attr, putInt(w.value)); s != im.StatusSuccess {
			t.Errorf("setpoint %#x in %s: %v", w.attr, w.mode, s)
		}
	}
	if s := write(t, th, cooling, putInt(3600)); s != im.StatusConstraintError {
		t.Errorf("a setpoint above the limits: %v", s)
	}
	invoke(t, th, 0x00, func(w *tlv.Writer) { // SetpointRaiseLower, Both, down 1 °C
		w.PutUint(tlv.Context(0), 2)
		w.PutInt(tlv.Context(1), -10)
	})
	want := []setCall{{capability.Setpoint, 22.0}, {capability.Setpoint, 21.0}, {capability.Setpoint, 23.0},
		{capability.Setpoint, 22.0}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
}

// One Home app change can be two writes. Through the Coalescer, SystemMode Cool sends the mode
// before turning the unit on, so it never starts in its old mode.
func TestSystemModeCoolWritesTheModeAndThenOn(t *testing.T) {
	c, rec, clk := coalescer(t, func(ident.ID, string) (any, bool) { return nil, false })
	ac := ident.ID{Name: "bedroom-ac"}
	m := Map(Device{Key: ac, Endpoint: 7, Name: "AC", Capabilities: []string{capability.On, capability.Mode, capability.Setpoint}},
		func(name string, value any) { c.Add(ac, name, value) })
	th := cluster(t, m, clusters.IDThermostat)
	if s := write(t, th, 0x001C, putUint(3)); s != im.StatusSuccess {
		t.Fatalf("SystemMode Cool: %v", s)
	}
	burstEnds(c, clk)
	if got, want := rec.list(), []string{"bedroom-ac mode=cool", "bedroom-ac on=true"}; !slices.Equal(got, want) {
		t.Errorf("writes %v, want %v", got, want)
	}
}
