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

package zigbee2mqtt

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harry/oikade/internal/capability"
	"github.com/harry/oikade/internal/driver"
	"github.com/harry/oikade/internal/mqtt"
)

type recorder struct {
	mu         sync.Mutex
	discovered map[types.NamespacedName]driver.Info
	changes    []driver.Update
	available  map[types.NamespacedName]bool
	seen       map[types.NamespacedName]time.Time
	undeclared int
}

func newRecorder() *recorder {
	return &recorder{
		discovered: map[types.NamespacedName]driver.Info{},
		available:  map[types.NamespacedName]bool{},
		seen:       map[types.NamespacedName]time.Time{},
	}
}

func (r *recorder) Discovered(d types.NamespacedName, i driver.Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discovered[d] = i
}
func (r *recorder) Changed(u driver.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, u)
}
func (r *recorder) Availability(d types.NamespacedName, online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.available[d] = online
}
func (r *recorder) Seen(d types.NamespacedName, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[d] = t
}
func (r *recorder) Undeclared(_ types.NamespacedName, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.undeclared = n
}

func dev(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "home", Name: name}
}

var (
	lamp   = dev("hallway-light")
	motion = dev("hallway-motion")
	door   = dev("front-door")
	plug   = dev("desk-plug")
	ghost  = dev("not-paired")
)

func setup(t *testing.T) (*Instance, *mqtt.Memory, *recorder) {
	t.Helper()
	ctx := context.Background()
	bus := mqtt.NewMemory()
	devices, err := os.ReadFile("testdata/bridge-devices.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = bus.Publish(ctx, "z2m/bridge/devices", devices, true)

	rec := newRecorder()
	now := time.Date(2026, 10, 17, 19, 0, 0, 0, time.UTC)
	i := &Instance{
		Key: dev("z2m"), BaseTopic: "z2m", Bus: bus, Sink: rec,
		Now: func() time.Time { return now }, Log: logr.Discard(),
	}
	i.SetDevices(map[string]types.NamespacedName{
		"0x000b57fffe000001": lamp,
		"0x00158d0000000002": motion,
		"0x00158d0000000003": door,
		"0x00124b0000000004": plug,
		"0x00124b00000000ff": ghost,
	})
	if err := i.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return i, bus, rec
}

func TestDiscovery(t *testing.T) {
	_, _, rec := setup(t)

	if got := rec.discovered[lamp]; !got.Found || got.Manufacturer != "IKEA" ||
		!slices.Equal(got.Capabilities, []string{capability.LightOn, capability.LightLevel, capability.LinkQuality}) {
		t.Errorf("lamp = %+v", got)
	}
	if got := rec.discovered[motion].Capabilities; !slices.Equal(got,
		[]string{capability.Occupancy, capability.Temperature, capability.Battery, capability.LinkQuality}) {
		t.Errorf("motion capabilities = %v", got)
	}
	if got := rec.discovered[door].Capabilities; !slices.Equal(got, []string{capability.ContactOpen, capability.Battery}) {
		t.Errorf("door capabilities = %v", got)
	}
	if got := rec.discovered[plug].Capabilities; !slices.Equal(got, []string{capability.SwitchOn}) {
		t.Errorf("plug capabilities = %v", got)
	}
	if rec.discovered[ghost].Found {
		t.Error("unpaired device reported as found")
	}
	// The ZBMINI is paired but not declared; the coordinator doesn't count.
	if rec.undeclared != 1 {
		t.Errorf("undeclared = %d, want 1", rec.undeclared)
	}
}

func TestStateChanges(t *testing.T) {
	ctx := context.Background()
	_, bus, rec := setup(t)

	// Addressed by IEEE because Zigbee2MQTT hasn't applied the friendly name yet.
	_ = bus.Publish(ctx, "z2m/0x000b57fffe000001", []byte(`{"state":"ON","brightness":127,"color_temp":300,"linkquality":80}`), false)
	_ = bus.Publish(ctx, "z2m/0x000b57fffe000001", []byte(`{"state":"ON","brightness":127,"linkquality":80}`), false)
	_ = bus.Publish(ctx, "z2m/front-door", []byte(`{"contact":false,"battery":91}`), false)
	_ = bus.Publish(ctx, "z2m/hallway-motion", []byte(`{"occupancy":true,"battery":"bogus"}`), false)

	type change struct {
		dev   types.NamespacedName
		cap   string
		value any
	}
	got := make([]change, 0, len(rec.changes))
	for _, u := range rec.changes {
		got = append(got, change{u.Device, u.Capability, u.Value})
	}
	want := []change{
		{lamp, capability.LightOn, true},
		{lamp, capability.LightLevel, 50.0},
		{lamp, capability.LinkQuality, 80.0},
		// The repeat message changes nothing.
		{door, capability.ContactOpen, true}, // Zigbee2MQTT contact=false means open.
		{door, capability.Battery, 91.0},
		{motion, capability.Occupancy, true}, // The invalid battery value is dropped.
	}
	if !slices.Equal(got, want) {
		t.Errorf("changes:\n got %v\nwant %v", got, want)
	}
	if _, ok := rec.seen[lamp]; !ok {
		t.Error("lamp not marked seen")
	}

	_ = bus.Publish(ctx, "z2m/hallway-motion", []byte(`{"occupancy":false}`), false)
	last := rec.changes[len(rec.changes)-1]
	if last.Value != false || last.Previous != true {
		t.Errorf("last change = %+v, want false with previous true", last)
	}
}

func TestAvailability(t *testing.T) {
	ctx := context.Background()
	_, bus, rec := setup(t)
	_ = bus.Publish(ctx, "z2m/desk-plug/availability", []byte(`{"state":"online"}`), true)
	_ = bus.Publish(ctx, "z2m/front-door/availability", []byte(`offline`), true)
	if !rec.available[plug] {
		t.Error("plug should be online")
	}
	if online, ok := rec.available[door]; !ok || online {
		t.Error("door should be offline")
	}
}

func TestSet(t *testing.T) {
	ctx := context.Background()
	i, bus, _ := setup(t)
	for _, tc := range []struct {
		dev   types.NamespacedName
		cap   string
		value any
		want  string
	}{
		{lamp, capability.LightOn, true, `{"state":"ON"}`},
		{lamp, capability.LightOn, false, `{"state":"OFF"}`},
		{lamp, capability.LightLevel, 40.0, `{"brightness":102}`},
		{plug, capability.SwitchOn, true, `{"state":"ON"}`},
	} {
		if err := i.Set(ctx, tc.dev, tc.cap, tc.value); err != nil {
			t.Fatal(err)
		}
		topic := "z2m/" + map[types.NamespacedName]string{lamp: "0x000b57fffe000001", plug: "0x00124b0000000004"}[tc.dev] + "/set"
		sent := bus.Sent(topic)
		if len(sent) == 0 || sent[len(sent)-1] != tc.want {
			t.Errorf("Set(%s, %s, %v) sent %v, want %s", tc.dev.Name, tc.cap, tc.value, sent, tc.want)
		}
	}
	if err := i.Set(ctx, door, capability.LightOn, true); err == nil {
		t.Error("Set accepted a capability the device doesn't have")
	}
	if err := i.Set(ctx, ghost, capability.LightOn, true); err == nil {
		t.Error("Set accepted an unpaired device")
	}
}
