/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hub

import (
	"slices"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harry/oikade/internal/driver"
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

	// Dropping the Zigbee2MQTT instance's devices leaves the Plugin's alone.
	s.SetDevices(z2m, nil)
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
