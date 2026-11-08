package matterbridge

import (
	"slices"
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
)

// device is a Device on endpoint id.
func device(name string, id uint16, caps ...string) Device {
	return Device{Key: ident.ID{Name: name}, Endpoint: id, Name: name, Capabilities: caps}
}

func newBridgeRig() *Bridge {
	b := &Bridge{Matter: clusters.NewBridge(clusters.Info{})}
	b.Store = hub.NewStore(logging.Discard(), b)
	b.Writes = &Coalescer{Setter: &recorder{}, Current: func(ident.ID, string) (any, bool) { return nil, false }}
	return b
}

// apply puts these devices on the bridge.
func apply(t *testing.T, b *Bridge, devices ...Device) {
	t.Helper()
	want := map[uint16]Device{}
	for _, d := range devices {
		want[d.Endpoint] = d
	}
	if err := b.Apply(logging.Discard(), want); err != nil {
		t.Fatal(err)
	}
}

func endpointIDs(b *Bridge) []uint16 {
	var ids []uint16
	b.Matter.Node.View(func() {
		for _, e := range b.Matter.Node.Endpoints() {
			ids = append(ids, e.ID)
		}
	})
	return ids
}

func TestBridgeFollowsDevices(t *testing.T) {
	lamp := device("desk-lamp", 2, capability.On, capability.Brightness)
	sensor := device("hall-sensor", 3, capability.Occupied)
	b := newBridgeRig()
	apply(t, b, lamp, sensor)
	if got := endpointIDs(b); len(got) != 4 || got[2] != 2 || got[3] != 3 {
		t.Fatalf("endpoints %v, want 0 1 2 3", got)
	}

	// The bus reaches the clusters, and availability is Reachable.
	b.Store.SetDevices(driver.InstanceKey{}, map[ident.ID]hub.Meta{
		{Name: "desk-lamp"}: {},
	})
	b.Changed(driver.Update{Device: ident.ID{Name: "desk-lamp"}, Capability: capability.On, Value: true})
	b.Availability(ident.ID{Name: "desk-lamp"}, false)
	b.Matter.Node.View(func() {
		e := b.Matter.Node.Endpoint(2)
		var on, reachable bool
		on = readAttr(t, e, clusters.IDOnOff, 0).(bool)
		reachable = readAttr(t, e, clusters.IDBridgedDeviceBasicInformation, 0x11).(bool)
		if !on || reachable {
			t.Errorf("OnOff %v, Reachable %v", on, reachable)
		}
	})

	// A Device that changes is rebuilt on its endpoint; one that goes away is removed.
	lamp.Capabilities = lamp.Capabilities[:1]
	apply(t, b, lamp)
	if got := endpointIDs(b); len(got) != 3 || got[2] != 2 {
		t.Fatalf("endpoints %v, want 0 1 2", got)
	}
	b.Matter.Node.View(func() {
		if b.Matter.Node.Endpoint(2).Cluster(clusters.IDLevelControl) != nil {
			t.Error("the lamp kept Level Control after it lost brightness")
		}
	})
}

// A displayName change renames the bridged device in place: the same endpoint, keeping its state,
// with a new NodeLabel.
func TestBridgeRenamesInPlace(t *testing.T) {
	sensor := device("hall-sensor", 3, capability.Occupied)
	sensor.Name = "Hall sensor"
	b := newBridgeRig()
	apply(t, b, sensor)
	key := ident.ID{Name: "hall-sensor"}
	b.Changed(driver.Update{Device: key, Capability: capability.Occupied, Value: true})
	var before *im.Endpoint
	b.Matter.Node.View(func() { before = b.Matter.Node.Endpoint(3) })
	if got := readLabel(t, b, 3); got != "Hall sensor" {
		t.Fatalf("NodeLabel %q", got)
	}

	sensor.Name = "Landing sensor"
	apply(t, b, sensor)
	b.Matter.Node.View(func() {
		e := b.Matter.Node.Endpoint(3)
		if e != before {
			t.Error("the endpoint was rebuilt")
		}
		if occupied := readAttr(t, e, clusters.IDOccupancySensing, 0); occupied != uint64(1) {
			t.Errorf("Occupied %v after the rename, want it kept", occupied)
		}
	})
	if got := readLabel(t, b, 3); got != "Landing sensor" {
		t.Errorf("NodeLabel %q, want Landing sensor", got)
	}
}

func readLabel(t *testing.T, b *Bridge, id uint16) string {
	t.Helper()
	var label string
	b.Matter.Node.View(func() {
		label, _ = readAttr(t, b.Matter.Node.Endpoint(id), clusters.IDBridgedDeviceBasicInformation, 0x05).(string)
	})
	return label
}

func readAttr(t *testing.T, e *im.Endpoint, cluster, attr uint32) any {
	t.Helper()
	return read(t, e.Cluster(cluster), attr)
}

// Only changes that can move endpoints wake the bridge, not each reading's status update.
func onOff(t *testing.T, b *Bridge, id uint16) bool {
	t.Helper()
	var on bool
	b.Matter.Node.View(func() {
		for _, e := range b.Matter.Node.Endpoints() {
			if e.ID == id {
				on = readAttr(t, e, clusters.IDOnOff, 0).(bool)
			}
		}
	})
	return on
}

// TestApplyFollowsNamesThatSwap keeps both endpoints, and sends each one's writes and values to the
// device that has its name now.
func TestApplyFollowsNamesThatSwap(t *testing.T) {
	b := &Bridge{Matter: clusters.NewBridge(clusters.Info{})}
	b.Store = hub.NewStore(logging.Discard(), b)
	first, second := ident.ID{Name: "first"}, ident.ID{Name: "second"}
	dev := func(key ident.ID, id uint16) Device {
		return Device{Key: key, Endpoint: id, Name: key.Name, Capabilities: []string{capability.On}, Unique: "u"}
	}
	if err := b.Apply(logging.Discard(), map[uint16]Device{2: dev(first, 2), 3: dev(second, 3)}); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(logging.Discard(), map[uint16]Device{2: dev(second, 2), 3: dev(first, 3)}); err != nil {
		t.Fatal(err)
	}
	if got := endpointIDs(b); !slices.Contains(got, 2) || !slices.Contains(got, 3) {
		t.Fatalf("endpoints %v", got)
	}
	b.Changed(driver.Update{Device: first, Capability: capability.On, Value: true})
	if !onOff(t, b, 3) || onOff(t, b, 2) {
		t.Errorf("first is on endpoint 3 now: 2 is %v, 3 is %v", onOff(t, b, 2), onOff(t, b, 3))
	}
}

// TestAbsentIsUnreachableUntilTheDeviceReports keeps a device on the bridge while its driver is away.
func TestAbsentIsUnreachableUntilTheDeviceReports(t *testing.T) {
	b := &Bridge{Matter: clusters.NewBridge(clusters.Info{})}
	b.Store = hub.NewStore(logging.Discard(), b)
	key := ident.ID{Name: "lamp"}
	d := Device{Key: key, Endpoint: 2, Name: "lamp", Capabilities: []string{capability.On}, Unique: "u", Absent: true}
	reachable := func() bool {
		var r bool
		b.Matter.Node.View(func() {
			for _, e := range b.Matter.Node.Endpoints() {
				if e.ID == 2 {
					r = readAttr(t, e, clusters.IDBridgedDeviceBasicInformation, 0x11).(bool)
				}
			}
		})
		return r
	}
	if err := b.Apply(logging.Discard(), map[uint16]Device{2: d}); err != nil {
		t.Fatal(err)
	}
	if reachable() {
		t.Error("an absent device is reachable")
	}
	d.Absent = false
	if err := b.Apply(logging.Discard(), map[uint16]Device{2: d}); err != nil {
		t.Fatal(err)
	}
	if !reachable() {
		t.Error("the device stayed unreachable")
	}
}
