package hub

import (
	"slices"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
)

func TestStoreRoom(t *testing.T) {
	s := NewStore(logr.Discard())
	dev := func(ns, name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }
	z2m := func(ns string) driver.InstanceKey {
		return driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: dev(ns, "z2m")}
	}
	s.SetDevices(z2m("home"), map[types.NamespacedName]Meta{
		dev("home", "lamp-b"):  {Room: "living-room"},
		dev("home", "lamp-a"):  {Room: "living-room"},
		dev("home", "hall"):    {Room: "hallway"},
		dev("home", "no-room"): {},
	})
	s.SetDevices(z2m("other"), map[types.NamespacedName]Meta{
		dev("other", "lamp-c"): {Room: "living-room"},
	})

	if got, want := s.Room("home", "living-room"), []types.NamespacedName{dev("home", "lamp-a"), dev("home", "lamp-b")}; !slices.Equal(got, want) {
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
	s := NewStore(logr.Discard())
	name := types.NamespacedName{Namespace: "home", Name: "garden"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, NamespacedName: name}
	lamp := types.NamespacedName{Namespace: "home", Name: "lamp"}
	relay := types.NamespacedName{Namespace: "home", Name: "relay"}

	s.SetDevices(z2m, map[types.NamespacedName]Meta{lamp: {Instance: z2m}})
	s.SetDevices(plug, map[types.NamespacedName]Meta{relay: {Instance: plug}})
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
	s.SetDevices(plug, map[types.NamespacedName]Meta{relay: {Instance: plug}})

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
	s := NewStore(logr.Discard())
	name := types.NamespacedName{Namespace: "home", Name: "porch"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, NamespacedName: name}
	light := types.NamespacedName{Namespace: "home", Name: "porch-light"}

	s.SetDevices(z2m, map[types.NamespacedName]Meta{light: {Instance: z2m}})
	s.Discovered(light, driver.Info{Found: true, Capabilities: []string{capability.OnOff}})
	s.Changed(driver.Update{Device: light, Capability: capability.OnOff, Value: true})
	s.Availability(light, true)

	// The Plugin reconciler can run before the Zigbee2MQTT one drops the device.
	s.SetDevices(plug, map[types.NamespacedName]Meta{light: {Instance: plug}})
	s.SetDevices(z2m, nil)
	snap, ok := s.Get(light)
	if !ok || snap.Instance != plug {
		t.Fatalf("Get = %+v, %v; want it on %v", snap.Instance, ok, plug)
	}
	if snap.Discovered || len(snap.Values) != 0 || len(snap.Changes) != 0 || snap.Online != nil {
		t.Errorf("state from the old driver kept: %+v", snap)
	}

	// The same instance re-declaring keeps state, and isn't a reassignment.
	s.Changed(driver.Update{Device: light, Capability: capability.OnOff, Value: false})
	s.SetDevices(plug, map[types.NamespacedName]Meta{light: {Instance: plug, Room: "porch"}})
	if snap, _ := s.Get(light); len(snap.Values) != 1 || snap.Room != "porch" {
		t.Errorf("state lost on re-declaration: %+v", snap)
	}
}

type reassignments struct {
	NopSink
	got []types.NamespacedName
}

func (r *reassignments) Reassigned(dev types.NamespacedName) { r.got = append(r.got, dev) }

func TestSetDevicesReportsReassignments(t *testing.T) {
	r := &reassignments{}
	s := NewStore(logr.Discard(), r)
	name := types.NamespacedName{Namespace: "home", Name: "porch"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, NamespacedName: name}
	light := types.NamespacedName{Namespace: "home", Name: "porch-light"}

	s.SetDevices(plug, map[types.NamespacedName]Meta{light: {Instance: plug}})                // claimed
	s.SetDevices(plug, map[types.NamespacedName]Meta{light: {Instance: plug, Room: "porch"}}) // unchanged
	s.SetDevices(z2m, map[types.NamespacedName]Meta{light: {Instance: z2m}})                  // moved
	s.SetDevices(z2m, nil)                                                                    // dropped
	if want := []types.NamespacedName{light, light, light}; !slices.Equal(r.got, want) {
		t.Errorf("Reassigned = %v, want %v", r.got, want)
	}
}
