package hub

import (
	"slices"
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
)

func TestStoreRoom(t *testing.T) {
	s := NewStore(logging.Discard())
	dev := func(ns, name string) ident.ID { return ident.ID{Namespace: ns, Name: name} }
	z2m := func(ns string) driver.InstanceKey {
		return driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: dev(ns, "z2m")}
	}
	s.SetDevices(z2m("home"), map[ident.ID]Meta{
		dev("home", "lamp-b"):  {Room: "living-room"},
		dev("home", "lamp-a"):  {Room: "living-room"},
		dev("home", "hall"):    {Room: "hallway"},
		dev("home", "no-room"): {},
	})
	s.SetDevices(z2m("other"), map[ident.ID]Meta{
		dev("other", "lamp-c"): {Room: "living-room"},
	})

	if got, want := s.Room("home", "living-room"), []ident.ID{dev("home", "lamp-a"), dev("home", "lamp-b")}; !slices.Equal(got, want) {
		t.Errorf("Room(living-room) = %v, want %v", got, want)
	}
	if got := s.Room("home", "kitchen"); len(got) != 0 {
		t.Errorf("Room(kitchen) = %v, want none", got)
	}
	if got := s.Room("home", ""); len(got) != 0 {
		t.Errorf(`Room("") = %v, want none`, got)
	}
}

func TestInstanceKeySeparatesDrivers(t *testing.T) {
	s := NewStore(logging.Discard())
	name := ident.ID{Namespace: "home", Name: "garden"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, ID: name}
	lamp := ident.ID{Namespace: "home", Name: "lamp"}
	relay := ident.ID{Namespace: "home", Name: "relay"}

	s.SetDevices(z2m, map[ident.ID]Meta{lamp: {Instance: z2m}})
	s.SetDevices(plug, map[ident.ID]Meta{relay: {Instance: plug}})
	s.Undeclared(z2m, 3)
	s.Undeclared(plug, 1)

	if got := s.UndeclaredCounts(); got[z2m] != 3 || got[plug] != 1 || len(got) != 2 {
		t.Errorf("UndeclaredCounts() = %v, want %v=3 and %v=1", got, z2m, plug)
	}

	// An instance with no Devices declared still counts its paired devices.
	s.SetDevices(plug, nil)
	if got := s.UndeclaredCounts(); got[plug] != 1 {
		t.Errorf("UndeclaredCounts() = %v, want %v=1 kept while no Devices are declared", got, plug)
	}
	s.SetDevices(plug, map[ident.ID]Meta{relay: {Instance: plug}})

	// Forgetting the Zigbee2MQTT instance leaves the Plugin's alone.
	s.Forget(z2m)
	if _, ok := s.Get(lamp); ok {
		t.Errorf("lamp still declared after its instance dropped it")
	}
	if snap, ok := s.Get(relay); !ok || snap.Instance != plug {
		t.Errorf("Get(relay) = %+v, %v; want it kept on %v", snap.Instance, ok, plug)
	}
	if got := s.UndeclaredCounts(); got[plug] != 1 || len(got) != 1 {
		t.Errorf("UndeclaredCounts() = %v, want only %v=1", got, plug)
	}

	if got, want := z2m.Label(), "garden"; got != want {
		t.Errorf("Zigbee2MQTT Label() = %q, want %q", got, want)
	}
	if got, want := plug.Label(), "plugin/garden"; got != want {
		t.Errorf("Plugin Label() = %q, want %q", got, want)
	}
}

func TestDeviceMovingDriverStartsAfresh(t *testing.T) {
	s := NewStore(logging.Discard())
	name := ident.ID{Namespace: "home", Name: "porch"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, ID: name}
	light := ident.ID{Namespace: "home", Name: "porch-light"}

	s.SetDevices(z2m, map[ident.ID]Meta{light: {Instance: z2m}})
	s.Discovered(light, driver.Info{Found: true, Capabilities: []string{capability.On}})
	s.Changed(driver.Update{Device: light, Capability: capability.On, Value: true})
	s.Availability(light, true)

	// The Plugin reconciler can run before the Zigbee2MQTT one drops the device.
	s.SetDevices(plug, map[ident.ID]Meta{light: {Instance: plug}})
	s.SetDevices(z2m, nil)
	snap, ok := s.Get(light)
	if !ok || snap.Instance != plug {
		t.Fatalf("Get = %+v, %v; want it on %v", snap.Instance, ok, plug)
	}
	if snap.Discovered || len(snap.Values) != 0 || len(snap.Changes) != 0 || snap.Online != nil {
		t.Errorf("state from the old driver kept: %+v", snap)
	}

	// The same instance re-declaring keeps state, and isn't a reassignment.
	s.Changed(driver.Update{Device: light, Capability: capability.On, Value: false})
	s.SetDevices(plug, map[ident.ID]Meta{light: {Instance: plug, Room: "porch"}})
	if snap, _ := s.Get(light); len(snap.Values) != 1 || snap.Room != "porch" {
		t.Errorf("state lost on re-declaration: %+v", snap)
	}
}

type reassignments struct {
	NopSink
	got []ident.ID
}

func (r *reassignments) Reassigned(dev ident.ID) { r.got = append(r.got, dev) }

func TestSetDevicesReportsReassignments(t *testing.T) {
	r := &reassignments{}
	s := NewStore(logging.Discard(), r)
	name := ident.ID{Namespace: "home", Name: "porch"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, ID: name}
	light := ident.ID{Namespace: "home", Name: "porch-light"}

	s.SetDevices(plug, map[ident.ID]Meta{light: {Instance: plug}})                // claimed
	s.SetDevices(plug, map[ident.ID]Meta{light: {Instance: plug, Room: "porch"}}) // unchanged
	s.SetDevices(z2m, map[ident.ID]Meta{light: {Instance: z2m}})                  // moved
	s.SetDevices(z2m, nil)                                                        // dropped
	if want := []ident.ID{light, light, light}; !slices.Equal(r.got, want) {
		t.Errorf("Reassigned = %v, want %v", r.got, want)
	}
}

type controlChanges struct {
	reassignments
	got []ident.ID
}

func (c *controlChanges) ControlChanged(dev ident.ID) { c.got = append(c.got, dev) }

// An empty list of control entries is the same as none: only a real change restarts arbiters.
func TestSetDevicesReportsOnlyRealControlChanges(t *testing.T) {
	c := &controlChanges{}
	s := NewStore(logging.Discard(), c)
	name := ident.ID{Namespace: "home", Name: "landing"}
	plug := driver.InstanceKey{Driver: driver.Plugin, ID: name}
	lamp := ident.ID{Namespace: "home", Name: "landing-lamp"}
	entry := control.Entry{Capability: "on"}

	s.SetDevices(plug, map[ident.ID]Meta{lamp: {Instance: plug, Control: []control.Entry{}}})      // empty
	s.SetDevices(plug, map[ident.ID]Meta{lamp: {Instance: plug}})                                  // still none
	s.SetDevices(plug, map[ident.ID]Meta{lamp: {Instance: plug, Control: []control.Entry{entry}}}) // added
	s.SetDevices(plug, map[ident.ID]Meta{lamp: {Instance: plug, Control: []control.Entry{entry}}}) // unchanged
	if want := []ident.ID{lamp}; !slices.Equal(c.got, want) {
		t.Errorf("ControlChanged = %v, want %v", c.got, want)
	}
}

func TestRenameMovesLiveStateAsOneChange(t *testing.T) {
	s := NewStore(logging.Discard())
	instance := driver.InstanceKey{Driver: "test", Name: "a"}
	a, b := ident.ID{Name: "a"}, ident.ID{Name: "b"}
	s.SetDevices(instance, map[ident.ID]Meta{a: {Instance: instance}, b: {Instance: instance}})
	s.Changed(driver.Update{Device: a, Capability: "on", Value: true})
	s.Changed(driver.Update{Device: b, Capability: "on", Value: false})

	s.Rename(map[ident.ID]Move{
		a: {To: b, Meta: Meta{Room: "for-b", Instance: instance}},
		b: {To: a, Meta: Meta{Room: "for-a", Instance: instance}},
	})
	if snap, ok := s.Get(b); !ok || snap.Values["on"] != true || snap.Room != "for-b" {
		t.Errorf("b: %+v, %v", snap, ok)
	}
	if snap, ok := s.Get(a); !ok || snap.Values["on"] != false || snap.Room != "for-a" {
		t.Errorf("a: %+v, %v", snap, ok)
	}

	c := ident.ID{Name: "c"}
	s.Rename(map[ident.ID]Move{a: {To: c, Meta: Meta{Instance: instance}}})
	if _, ok := s.Get(a); ok {
		t.Error("the old name stayed")
	}
	if _, ok := s.Get(c); !ok {
		t.Error("the new name is missing")
	}
	s.Rename(map[ident.ID]Move{a: {To: b, Meta: Meta{Room: "nothing", Instance: instance}}})
	if snap, _ := s.Get(b); snap.Room == "nothing" {
		t.Error("nothing replaced a device")
	}
}
