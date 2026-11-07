package matterbridge

import (
	"reflect"
	"slices"
	"testing"
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
	m := Map(Device{Key: ident.ID{Namespace: "home", Name: "d"}, Endpoint: 7, Name: "D", Capabilities: caps, Type: typ},
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
	a, b := UniqueID(ident.ID{Namespace: "home", Name: "lamp"}), UniqueID(ident.ID{Namespace: "home", Name: "lamp2"})
	if len(a) != 32 || a == b || a != UniqueID(ident.ID{Namespace: "home", Name: "lamp"}) {
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

// A device you say is an on/off light is one, brightness or not: Level Control would make it dimmable.
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
