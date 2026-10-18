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
)

func TestStoreRoom(t *testing.T) {
	s := NewStore(logr.Discard())
	dev := func(ns, name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }
	s.SetDevices(dev("home", "z2m"), map[types.NamespacedName]Meta{
		dev("home", "lamp-b"):  {Room: "living-room"},
		dev("home", "lamp-a"):  {Room: "living-room"},
		dev("home", "hall"):    {Room: "hallway"},
		dev("home", "no-room"): {},
	})
	s.SetDevices(dev("other", "z2m"), map[types.NamespacedName]Meta{
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
