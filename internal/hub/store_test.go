package hub

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/testkit"
)

func TestStoreRoom(t *testing.T) {
	s := NewStore(logging.Discard())
	dev := func(name string) ident.ID { return ident.ID{Name: name} }
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: dev("z2m")}
	s.SetDevices(z2m, map[ident.ID]Meta{
		dev("lamp-b"):  {Room: "living-room"},
		dev("lamp-a"):  {Room: "living-room"},
		dev("hall"):    {Room: "hallway"},
		dev("no-room"): {},
	})

	if got, want := s.Room("living-room"), []ident.ID{dev("lamp-a"), dev("lamp-b")}; !slices.Equal(got, want) {
		t.Errorf("Room(living-room) = %v, want %v", got, want)
	}
	if got := s.Room("kitchen"); len(got) != 0 {
		t.Errorf("Room(kitchen) = %v, want none", got)
	}
	if got := s.Room(""); len(got) != 0 {
		t.Errorf(`Room("") = %v, want none`, got)
	}
}

func TestInstanceKeySeparatesDrivers(t *testing.T) {
	s := NewStore(logging.Discard())
	name := ident.ID{Name: "garden"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, ID: name}
	lamp := ident.ID{Name: "lamp"}
	relay := ident.ID{Name: "relay"}

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
	name := ident.ID{Name: "porch"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, ID: name}
	light := ident.ID{Name: "porch-light"}

	s.SetDevices(z2m, map[ident.ID]Meta{light: {Instance: z2m}})
	s.Discovered(light, driver.Info{Found: true, Capabilities: []string{capability.On}})
	s.Changed(driver.Update{Device: light, Capability: capability.On, Value: true})
	s.Availability(light, true)

	// One instance can take a device before the other has dropped it.
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
	name := ident.ID{Name: "porch"}
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: name}
	plug := driver.InstanceKey{Driver: driver.Plugin, ID: name}
	light := ident.ID{Name: "porch-light"}

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
	name := ident.ID{Name: "landing"}
	plug := driver.InstanceKey{Driver: driver.Plugin, ID: name}
	lamp := ident.ID{Name: "landing-lamp"}
	entry := control.Entry{Capability: "on"}

	s.SetDevices(plug, map[ident.ID]Meta{lamp: {Instance: plug, Control: []control.Entry{}}})      // empty
	s.SetDevices(plug, map[ident.ID]Meta{lamp: {Instance: plug}})                                  // still none
	s.SetDevices(plug, map[ident.ID]Meta{lamp: {Instance: plug, Control: []control.Entry{entry}}}) // added
	s.SetDevices(plug, map[ident.ID]Meta{lamp: {Instance: plug, Control: []control.Entry{entry}}}) // unchanged
	if want := []ident.ID{lamp}; !slices.Equal(c.got, want) {
		t.Errorf("ControlChanged = %v, want %v", c.got, want)
	}
}

// A device is its key, its hardware, so a rename is only its Meta: its live state stays, and the
// log names it as it's called now.
func TestARenameKeepsLiveStateAndLogsTheNewName(t *testing.T) {
	log := testkit.Logs(t)
	s := NewStore(log.Logger)
	instance := driver.InstanceKey{Driver: driver.Plugin, Name: "hue"}
	lamp := ident.ID{Name: "hue/0x00178801091863f2"}
	s.SetDevices(instance, map[ident.ID]Meta{lamp: {DisplayName: "desk-lamp", Instance: instance}})
	s.Changed(driver.Update{Device: lamp, Capability: "on", Value: true})

	s.SetDevices(instance, map[ident.ID]Meta{lamp: {DisplayName: "reading-lamp", Room: "study", Instance: instance}})
	if snap, ok := s.Get(lamp); !ok || snap.Values["on"] != true || snap.DisplayName != "reading-lamp" {
		t.Errorf("after the rename: %+v, %v", snap, ok)
	}
	s.Changed(driver.Update{Device: lamp, Capability: "on", Value: false})
	if !log.Has(`msg="Changed capability"`, "device=hardware/hue/0x00178801091863f2") {
		t.Errorf("no line naming the lamp by its ref:\n%s", strings.Join(log.Lines(), "\n"))
	}
}

// A device going offline, and coming back, is logged once each, and a first report online isn't.
// Went hears the same, for the event log.
func TestStoreLogsADeviceGoingOffline(t *testing.T) {
	log := testkit.Logs(t)
	s := NewStore(log.Logger)
	var went []string
	s.Went = func(dev ident.ID, online bool) { went = append(went, fmt.Sprint(dev.Name, " ", online)) }
	key := driver.InstanceKey{Driver: driver.Plugin, Name: "ewelink"}
	heater := ident.ID{Name: "ewelink/1000a1b2c3"}
	s.SetDevices(key, map[ident.ID]Meta{heater: {Instance: key}})
	s.Availability(heater, true)
	s.Availability(heater, false)
	s.Availability(heater, false)
	s.Availability(heater, true)
	s.Availability(heater, true)
	var got []string
	for _, line := range log.Lines() {
		if strings.Contains(line, "online") || strings.Contains(line, "offline") {
			got = append(got, line)
		}
	}
	if len(got) != 2 || !strings.Contains(got[0], `msg="Device went offline" device=hardware/ewelink/1000a1b2c3`) ||
		!strings.Contains(got[1], `msg="Device came back online" device=hardware/ewelink/1000a1b2c3`) {
		t.Errorf("logged %q", got)
	}

	// One that's offline from its first report says so.
	lamp := ident.ID{Name: "ewelink/1000d4e5f6"}
	s.SetDevices(key, map[ident.ID]Meta{heater: {Instance: key}, lamp: {Instance: key}})
	s.Availability(lamp, false)
	if !log.Has(`msg="Device went offline"`, "device=hardware/ewelink/1000d4e5f6") {
		t.Errorf("no line for the lamp:\n%s", strings.Join(log.Lines(), "\n"))
	}
	if want := []string{"ewelink/1000a1b2c3 false", "ewelink/1000a1b2c3 true", "ewelink/1000d4e5f6 false"}; !slices.Equal(went, want) {
		t.Errorf("Went heard %q, want %q", went, want)
	}
}

// A device offline says since when, which another report of the same doesn't move, and one back
// online has no such time.
func TestStoreSaysSinceWhenADeviceIsOffline(t *testing.T) {
	s := NewStore(testkit.Logs(t).Logger)
	key := driver.InstanceKey{Driver: driver.Plugin, Name: "ewelink"}
	fan := ident.ID{Name: "loft-fan"}
	s.SetDevices(key, map[ident.ID]Meta{fan: {Instance: key}})
	// Offline when the host first hears of it, it can't say since when.
	s.Availability(fan, false)
	if snap, _ := s.Get(fan); !snap.OfflineSince.IsZero() {
		t.Errorf("offline from the start, it's offline since %v", snap.OfflineSince)
	}
	s.Availability(fan, true)
	before := time.Now()
	s.Availability(fan, false)
	snap, _ := s.Get(fan)
	went := snap.OfflineSince
	// One that has reported something was there to go, even if it never said it was online.
	heater := ident.ID{Name: "hall-heater"}
	s.SetDevices(key, map[ident.ID]Meta{fan: {Instance: key}, heater: {Instance: key}})
	s.Seen(heater, time.Now())
	s.Availability(heater, false)
	if snap, _ := s.Get(heater); snap.OfflineSince.IsZero() {
		t.Error("a device seen and then offline isn't offline since a time")
	}
	if went.Before(before) || went.After(time.Now()) {
		t.Fatalf("offline since %v, want between %v and now", went, before)
	}
	s.Availability(fan, false)
	if snap, _ := s.Get(fan); !snap.OfflineSince.Equal(went) {
		t.Errorf("a second report moved it from %v to %v", went, snap.OfflineSince)
	}
	s.Availability(fan, true)
	if snap, _ := s.Get(fan); !snap.OfflineSince.IsZero() {
		t.Errorf("back online, it's still offline since %v", snap.OfflineSince)
	}
}
