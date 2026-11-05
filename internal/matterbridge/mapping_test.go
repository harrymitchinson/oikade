package matterbridge

import (
	"reflect"
	"testing"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

type setCall struct {
	name  string
	value any
}

func mapDevice(t *testing.T, typ homev1alpha1.MatterDeviceType, caps ...string) (*Mapped, *[]setCall) {
	t.Helper()
	var calls []setCall
	m := Map(Device{Key: types.NamespacedName{Namespace: "home", Name: "d"}, Endpoint: 7, Name: "D", Capabilities: caps, Type: typ},
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
		typ  homev1alpha1.MatterDeviceType
		caps []string
		want []uint32
	}{
		"on-off shown as a plug":   {"", []string{capability.OnOff}, []uint32{0x010A}},
		"dimmable shown as light":  {"", []string{capability.OnOff, capability.CurrentLevel}, []uint32{0x0101}},
		"on-off as a light":        {homev1alpha1.MatterDeviceTypeLight, []string{capability.OnOff}, []uint32{0x0100}},
		"on-off as a wall control": {homev1alpha1.MatterDeviceTypeMountedControl, []string{capability.OnOff}, []uint32{0x010F}},
		"dimmable as a plug":       {homev1alpha1.MatterDeviceTypePlugInUnit, []string{capability.OnOff, capability.CurrentLevel}, []uint32{0x010B}},
		"motion with temperature and battery": {"", []string{capability.Occupancy, capability.Temperature, capability.BatPercentRemaining, capability.LinkQuality},
			[]uint32{0x0107, 0x0302, 0x0011}},
		"contact":  {"", []string{capability.Contact}, []uint32{0x0015}},
		"humidity": {"", []string{capability.RelativeHumidity, capability.Temperature}, []uint32{0x0302, 0x0307}},
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
	for _, caps := range [][]string{nil, {capability.LinkQuality}, {capability.BatPercentRemaining, capability.LinkQuality}} {
		if Supported(caps) || Map(Device{Capabilities: caps}, nil) != nil {
			t.Errorf("%v mapped", caps)
		}
	}
}

func TestDimmableLight(t *testing.T) {
	m, calls := mapDevice(t, "", capability.OnOff, capability.CurrentLevel)
	onoff, level := cluster(t, m, clusters.IDOnOff), cluster(t, m, clusters.IDLevelControl)
	if onoff.FeatureMap&1 == 0 {
		t.Error("a light's On/Off has no Lighting feature")
	}

	// The bus reaches the attributes.
	m.Update(capability.OnOff, true)
	m.Update(capability.CurrentLevel, 50.0)
	if read(t, onoff, 0) != true || read(t, level, 0) != uint64(128) {
		t.Errorf("OnOff %v, CurrentLevel %v", read(t, onoff, 0), read(t, level, 0))
	}
	m.Update(capability.CurrentLevel, 100.0)
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
		{capability.OnOff, false},
		{capability.OnOff, false}, // the minimum level with On/Off turns the light off, at its old level
		{capability.CurrentLevel, 100.0},
		{capability.CurrentLevel, 1.0}, {capability.OnOff, true}, // not 0%, which would read as off
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("writes %v, want %v", *calls, want)
	}
}

func TestSwitchAsPlug(t *testing.T) {
	m, calls := mapDevice(t, "", capability.OnOff)
	onoff := cluster(t, m, clusters.IDOnOff)
	if onoff.FeatureMap != 0 {
		t.Errorf("a plug's On/Off has features %#x", onoff.FeatureMap)
	}
	m.Update(capability.OnOff, true)
	invoke(t, onoff, 0x02, nil) // Toggle
	if read(t, onoff, 0) != false || !reflect.DeepEqual(*calls, []setCall{{capability.OnOff, false}}) {
		t.Errorf("OnOff %v, writes %v", read(t, onoff, 0), *calls)
	}
}

func TestSensors(t *testing.T) {
	m, _ := mapDevice(t, "", capability.Occupancy, capability.Contact, capability.Temperature, capability.RelativeHumidity, capability.BatPercentRemaining)
	temp := cluster(t, m, clusters.IDTemperatureMeasurement)
	if read(t, temp, 0) != nil {
		t.Errorf("temperature before any report %v, want null", read(t, temp, 0))
	}
	m.Update(capability.Occupancy, true)
	m.Update(capability.Contact, true)
	m.Update(capability.Temperature, 19.55)
	m.Update(capability.RelativeHumidity, 61.2)
	m.Update(capability.BatPercentRemaining, 15.0)
	m.Update(capability.LinkQuality, 80.0) // not mapped: ignored

	for _, tc := range []struct {
		cluster, attr uint32
		want          any
	}{
		{clusters.IDOccupancySensing, 0, uint64(1)},
		{clusters.IDBooleanState, 0, true},
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
	a, b := UniqueID(types.NamespacedName{Namespace: "home", Name: "lamp"}), UniqueID(types.NamespacedName{Namespace: "home", Name: "lamp2"})
	if len(a) != 32 || a == b || a != UniqueID(types.NamespacedName{Namespace: "home", Name: "lamp"}) {
		t.Errorf("unique IDs %q, %q", a, b)
	}
}
