package oikade

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/wire"
)

// declaring gives a test the program's declarations to itself, and puts back what was there.
func declaring(t *testing.T) {
	t.Helper()
	declared.mu.Lock()
	devices, rooms, mistakes := declared.devices, declared.rooms, declared.mistakes
	declared.devices, declared.rooms, declared.mistakes = nil, nil, nil
	declared.mu.Unlock()
	t.Cleanup(func() {
		declared.mu.Lock()
		defer declared.mu.Unlock()
		declared.devices, declared.rooms, declared.mistakes = devices, rooms, mistakes
	})
}

// sensor is a device with capabilities beyond its type's.
type sensor = struct {
	OccupancySensor
	Temperature, Battery Number
}

func TestADeviceHasItsHardwareInEveryCapability(t *testing.T) {
	declaring(t)
	cuisine := Room("cuisine")
	capteur := Device[sensor]("hue/0x1", cuisine)
	plafond := Device[DimmableLight]("hue/0x2", cuisine, Label("Plafond"))
	prise := Device[OnOffPlugInUnit]("hue/0x3")

	if capteur.Key != "hue/0x1" {
		t.Errorf("its key is %q", capteur.Key)
	}
	if capteur.Occupied.Capability != Occupied || capteur.Temperature.Capability != Temperature ||
		capteur.Battery != (Number{Device: "hue/0x1", Capability: Battery}) {
		t.Errorf("got %+v", capteur)
	}
	if plafond.On != (Bool{Device: "hue/0x2", Capability: On}) ||
		plafond.Brightness != (Number{Device: "hue/0x2", Capability: Brightness}) {
		t.Errorf("got %+v", plafond)
	}

	// The room has the devices placed in it, and is a group of them.
	members := cuisine.g.members
	if len(members) != 2 || members[0].name != "hue/0x1" || members[1].name != "hue/0x2" {
		t.Fatalf("the room has %+v", members)
	}
	if !slices.Equal(members[0].capabilities, []string{"battery", "occupied", "temperature"}) {
		t.Errorf("the sensor has %v in the room", members[0].capabilities)
	}
	if Room("cuisine").Occupied != cuisine.Occupied {
		t.Error("a room made twice is two rooms")
	}
	if ref := cuisine.Occupied.String(); ref != "room/cuisine/occupied" {
		t.Errorf("the room's occupied is %s", ref)
	}

	h := decide(func(h *Home) {
		h.Plugin("hue", zigbee)
		h.Group("lamps", plafond, prise)
		h.Decide(plafond.On).Case("someone-in", true, cuisine.Occupied.Any()).Default(false)
	})
	if _, _, err := h.compile(); err != nil {
		t.Fatal(err)
	}
	cuisineRoom, none := "cuisine", ""
	want := []wire.DeviceDeclaration{
		{
			Hardware: "hue/0x1", Room: &cuisineRoom, DeviceType: devicetype.OccupancySensor,
			Capabilities: []string{"battery", "occupied", "temperature"},
		},
		{
			Hardware: "hue/0x2", Room: &cuisineRoom, DeviceType: devicetype.DimmableLight,
			Capabilities: []string{"brightness", "on"}, Label: "Plafond",
		},
		{Hardware: "hue/0x3", Room: &none, DeviceType: devicetype.OnOffPlugInUnit, Capabilities: []string{"on"}},
	}
	g, _ := json.Marshal(h.manifest().Inventory)
	if w, _ := json.Marshal(want); string(g) != string(w) {
		t.Errorf("got %s, want %s", g, w)
	}
	if r := wire.Declarations(want); r[0].Driver != "plugin/hue" || r[0].ID != "0x1" || r[0].Key() != "hue/0x1" {
		t.Errorf("the registry gets %+v", r[0])
	}
}

func TestHomeRoomIsTheRoomOrSomeKindsOfIt(t *testing.T) {
	declaring(t)
	salon := Room("salon")
	h := NewHome(Options{})
	if h.Room(salon).On != salon.On || h.Room(salon, Everything).On != salon.On {
		t.Error("the whole room isn't the room")
	}
	lights := h.Room(salon, Lights)
	want := []devicetype.Type{devicetype.OnOffLight, devicetype.DimmableLight}
	if g := lights.On.g; g == salon.On.g || g.room != "salon" || !slices.Equal(g.types, want) {
		t.Errorf("the lights are %+v", lights.On.g)
	}
	if h.Room(salon, Lights).On != lights.On {
		t.Error("the lights made twice are two groups")
	}
}

func TestDevicesAreChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		add  func(*Home)
		want string
	}{
		"no hardware":   {func(*Home) { Device[OnOffLight]("") }, "it needs its hardware"},
		"escaped wrong": {func(*Home) { Device[OnOffLight]("relays/io://1/2") }, `is relays/io:%2F%2F1%2F2`},
		"no ID":         {func(*Home) { Device[OnOffLight]("relays/") }, "has no ID"},
		"no plugin":     {func(*Home) { Device[OnOffLight]("hue/0x1") }, "plugin hue's, which this home doesn't import"},
		"twice": {
			func(*Home) { Device[OnOffLight]("relays/a"); Device[OnOffPlugInUnit]("relays/a") }, "declared twice",
		},
		"both ways": {
			func(h *Home) { Device[OnOffLight]("relays/a"); h.Device("lamp").Hardware("relays", "a") },
			"Home.Device declares it too, as lamp",
		},
		"not a capability": {func(*Home) {
			Device[struct {
				OnOffLight
				Note string
			}]("relays/a")
		}, "its field Note isn't a capability"},
		"misnamed": {func(*Home) {
			Device[struct {
				OnOffLight
				Warmth Number
			}]("relays/a")
		}, "its field Warmth isn't named as a capability is"},
		"of the wrong kind": {func(*Home) {
			Device[struct {
				OnOffLight
				Temperature Bool
			}]("relays/a")
		},
			"its field Temperature is an oikade.Bool, and temperature is an oikade.Number"},
		"two rooms":   {func(*Home) { Device[OnOffLight]("relays/a", Room("a"), Room("b")) }, "it's placed in a and b"},
		"no room":     {func(*Home) { Device[OnOffLight]("relays/a", RoomGroup{}) }, "isn't one from oikade.Room"},
		"empty label": {func(*Home) { Device[OnOffLight]("relays/a", Label(" ")) }, "its Label is empty"},
		"a label on two lines": {
			func(*Home) { Device[OnOffLight]("relays/a", Label("Clim\nsalon")) }, "has a control character",
		},
		"by pointer": {
			func(*Home) { Device[struct{ *OnOffLight }]("relays/a") }, "it embeds OnOffLight by pointer or interface",
		},
		"a nil kind": {func(h *Home) { h.Room(Room("salon"), Kinds(nil)) }, "oikade.Kinds has a nil device type"},
		"a bad room": {func(*Home) { Device[OnOffLight]("relays/a", Room("Le Salon")) }, `"Le Salon" isn't a room name`},
		"not a struct": {func(*Home) { Device[DeviceType]("relays/a") },
			"its type, oikade.DeviceType, isn't a device type"},
	} {
		t.Run(name, func(t *testing.T) {
			declaring(t)
			h := decide(func(h *Home) { h.Plugin("relays", zigbee); tc.add(h) })
			if _, _, err := h.compile(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

type extras struct{ Temperature Number }

func TestADeviceCanEmbedAStructThatIsntExported(t *testing.T) {
	declaring(t)
	lamp := Device[struct {
		OnOffLight
		extras
	}]("relays/a")
	if lamp.Temperature != (Number{Device: "relays/a", Capability: Temperature}) {
		t.Errorf("got %+v", lamp)
	}
	h := decide(func(h *Home) { h.Plugin("relays", zigbee); h.Group("warm", lamp) })
	if _, _, err := h.compile(); err != nil {
		t.Fatal(err)
	}
	if got := h.groups[0].members[0].capabilities; !slices.Equal(got, []string{"on", "temperature"}) {
		t.Errorf("in a group, it has %v", got)
	}
}

// Each device type is one of devicetype's, under its name, and every one there has one.
func TestEveryDeviceTypeIsInGo(t *testing.T) {
	types := []DeviceType{
		OnOffLight{}, DimmableLight{}, OnOffPlugInUnit{}, DimmablePlugInUnit{}, MountedOnOffControl{},
		MountedDimmableLoadControl{}, OccupancySensor{}, ContactSensor{}, TemperatureSensor{}, HumiditySensor{},
		DoorLock{}, WindowCovering{}, Thermostat{}, RoomAirConditioner{},
	}
	got := make([]devicetype.Type, 0, len(types))
	for _, typ := range types {
		if name := reflect.TypeOf(typ).Name(); name != string(typ.deviceType()) {
			t.Errorf("oikade.%s is a %s", name, typ.deviceType())
		}
		got = append(got, typ.deviceType())
	}
	all := slices.Clone(devicetype.All)
	slices.Sort(all)
	slices.Sort(got)
	if !slices.Equal(got, all) {
		t.Errorf("Go has %v, and devicetype %v", got, all)
	}
}
