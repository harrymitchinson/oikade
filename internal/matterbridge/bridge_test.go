package matterbridge

import (
	"cmp"
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/testkit"
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

// A blind's move stops showing as moving once it has had position's settle, on the bridge's clock,
// even if the blind never reports getting there.
func TestABlindsMoveSettlesOnTheBridgesClock(t *testing.T) {
	b := newBridgeRig()
	fake := clock.NewFake(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	b.Clock = fake
	apply(t, b, device("blind", 2, capability.Position))
	b.Changed(driver.Update{Device: ident.ID{Name: "blind"}, Capability: capability.Position, Value: 10.0})
	status := func() any {
		var v any
		b.Matter.Node.View(func() { v = readAttr(t, b.Matter.Node.Endpoint(2), clusters.IDWindowCovering, 0x000A) })
		return v
	}
	b.Matter.Node.Update(func() {
		c := b.Matter.Node.Endpoint(2).Cluster(clusters.IDWindowCovering)
		for _, x := range c.Commands {
			if x.ID == clusters.CommandDownOrClose {
				if _, err := x.Invoke(nil, nil); err != nil {
					t.Error(err)
				}
			}
		}
	})
	if status() == uint64(0) {
		t.Fatal("the blind isn't moving after DownOrClose")
	}
	fake.Step(capability.SettleOf(capability.Position) - time.Second)
	time.Sleep(20 * time.Millisecond)
	if status() == uint64(0) {
		t.Fatal("the move settled early")
	}
	fake.Step(time.Second)
	for deadline := time.Now().Add(5 * time.Second); status() != uint64(0); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("OperationalStatus %v after the settle, want stopped", status())
		}
	}
}

// Each Window Covering command a controller sends is logged with its target, so a real host's log
// says what the Home app asked for (OIK-337).
func TestABlindsCommandsAreLogged(t *testing.T) {
	log := testkit.Logs(t)
	b := newBridgeRig()
	if err := b.Apply(log.Logger, map[uint16]Device{2: device("blind", 2, capability.Position)}); err != nil {
		t.Fatal(err)
	}
	blind := ident.ID{Name: "blind"}
	b.Changed(driver.Update{Device: blind, Capability: capability.Position, Value: 10.0})
	send := func(cmd uint32, f func(w *tlv.Writer)) {
		b.Matter.Node.Update(func() { invoke(t, b.Matter.Node.Endpoint(2).Cluster(clusters.IDWindowCovering), cmd, f) })
	}
	send(clusters.CommandGoToLiftPercentage, goToLift0(7000))
	b.Changed(driver.Update{Device: blind, Capability: capability.Position, Value: 40.0})
	send(clusters.CommandStopMotion, nil)
	send(clusters.CommandStopMotion, nil) // nothing moving
	for _, want := range [][]string{
		{"command=GoToLiftPercentage", "target=70"},
		{"command=StopMotion", "target=40"},
		{"command=StopMotion", "target=none"},
	} {
		if !log.Has(append([]string{`msg="Took Matter command"`, "device=blind"}, want...)...) {
			t.Errorf("no line with %v", want)
		}
	}
}

// A blind's moves gather for the travel window, and a stop goes at once, with the position the blind
// last reported (OIK-337).
func TestABlindsStopDoesntWaitForItsMove(t *testing.T) {
	b := newBridgeRig()
	fake := clock.NewFake(time.Date(2026, 11, 18, 15, 30, 0, 0, time.UTC))
	rec := &recorder{}
	b.Writes = &Coalescer{Setter: rec, Current: func(ident.ID, string) (any, bool) { return nil, false }, Clock: fake}
	b.Writes.Start(t.Context())
	apply(t, b, device("blind", 2, capability.Position))
	blind := ident.ID{Name: "blind"}
	b.Changed(driver.Update{Device: blind, Capability: capability.Position, Value: 10.0})
	send := func(cmd uint32, f func(w *tlv.Writer)) {
		b.Matter.Node.Update(func() { invoke(t, b.Matter.Node.Endpoint(2).Cluster(clusters.IDWindowCovering), cmd, f) })
	}
	send(clusters.CommandGoToLiftPercentage, goToLift0(7000))
	fake.Step(time.Second)
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("wrote %v while the move was gathering", got)
	}
	b.Changed(driver.Update{Device: blind, Capability: capability.Position, Value: 20.0})
	send(clusters.CommandStopMotion, nil)
	// The fake clock stays put, so only the stop can end the burst.
	testkit.Eventually(t, "the stop's write", func() bool { return len(rec.list()) > 0 })
	b.Writes.flushes.Wait()
	if got := rec.list(); !slices.Equal(got, []string{"blind position=20"}) {
		t.Errorf("writes %v, want only the stop's", got)
	}
}

// A stop before the blind has been sent its move works, and leaves it where it is: it hasn't
// started, so the position it last reported is where it is. Without the gathering it would fail,
// as a stop with no report since the move does, and the Home app would show No Response.
func TestABlindStoppedBeforeItsMoveIsSentStaysPut(t *testing.T) {
	b := newBridgeRig()
	fake := clock.NewFake(time.Date(2026, 11, 18, 15, 30, 0, 0, time.UTC))
	rec := &recorder{}
	b.Writes = &Coalescer{Setter: rec, Current: func(ident.ID, string) (any, bool) { return nil, false }, Clock: fake}
	b.Writes.Start(t.Context())
	apply(t, b, device("blind", 2, capability.Position))
	b.Changed(driver.Update{Device: ident.ID{Name: "blind"}, Capability: capability.Position, Value: 10.0})
	var stopErr error
	b.Matter.Node.Update(func() {
		c := b.Matter.Node.Endpoint(2).Cluster(clusters.IDWindowCovering)
		invoke(t, c, clusters.CommandGoToLiftPercentage, goToLift0(6000))
		for _, x := range c.Commands {
			if x.ID == clusters.CommandStopMotion {
				_, stopErr = x.Invoke(nil, nil)
			}
		}
	})
	if stopErr != nil {
		t.Fatalf("StopMotion before the move was sent: %v", stopErr)
	}
	testkit.Eventually(t, "the stop's write", func() bool { return len(rec.list()) > 0 })
	b.Writes.flushes.Wait()
	if got := rec.list(); !slices.Equal(got, []string{"blind position=10"}) {
		t.Errorf("writes %v, want only where the blind is", got)
	}
}

// What a device is, its manufacturer and model, is its VendorName and ProductName, while it's away
// too, and a new one is set in place.
func TestABridgedDeviceSaysWhatItIs(t *testing.T) {
	b := newBridgeRig()
	plug := device("desk-plug", 2, capability.On)
	plug.Manufacturer, plug.Model, plug.Absent = "Shelly", "Plug S", true
	apply(t, b, plug)
	var before *im.Endpoint
	product := func() (vendor, product any) {
		b.Matter.Node.View(func() {
			e := b.Matter.Node.Endpoint(2)
			before = cmp.Or(before, e)
			if e != before {
				t.Error("the endpoint was rebuilt")
			}
			vendor = readAttr(t, e, clusters.IDBridgedDeviceBasicInformation, 0x01)
			product = readAttr(t, e, clusters.IDBridgedDeviceBasicInformation, 0x03)
		})
		return vendor, product
	}
	if vendor, product := product(); vendor != "Shelly" || product != "Plug S" {
		t.Errorf("VendorName %v and ProductName %v, want Shelly Plug S", vendor, product)
	}
	plug.Model, plug.Absent = "Plug S Gen3", false
	apply(t, b, plug)
	if vendor, product := product(); vendor != "Shelly" || product != "Plug S Gen3" {
		t.Errorf("VendorName %v and ProductName %v, want Shelly Plug S Gen3", vendor, product)
	}
}
