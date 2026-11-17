package oikade

import (
	"errors"
	"fmt"
	"slices"

	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/registry"
)

// Declaration is a device your home's code declares, from Home.Device: what it's called, and the
// hardware, room and type you give it. A deploy applies it, so a rename, a move or a swap of
// hardware is a change to your code, reviewed and deployed with the code that uses it.
//
//	home.Device("porch-light").Hardware("zigbee", "0x00178801049a2b3c").Room("porch").Is(oikade.OnOffLight)
//	home.Device("greenhouse-fan").Room("greenhouse") // no hardware yet: the name is reserved
//
// Declaring is optional. A device you've paired and not declared works under its app's name.
type Declaration struct {
	name, plugin, id, room string
	deviceType             DeviceType
}

// Device declares a device by the name your code calls it.
func (h *Home) Device(name string) *Declaration {
	d := &Declaration{name: name}
	h.declared = append(h.declared, d)
	return d
}

// Hardware says which hardware it is: the name you imported its plugin by, with Home.Plugin, and the
// ID the plugin knows it by, as it is in oikade devices --json and the line oikade devices gives an
// undeclared device. The last part of its hardware/<plugin>/<id> ref is that ID, percent-encoded.
// Change the ID to swap the hardware: the name keeps its Matter endpoint, so the Home app keeps its
// room and scenes.
func (d *Declaration) Hardware(plugin, id string) *Declaration {
	d.plugin, d.id = plugin, id
	return d
}

// Room puts it in a room.
func (d *Declaration) Room(room string) *Declaration {
	d.room = room
	return d
}

// Is says what a device that's on or off is, which wins over what its plugin says.
func (d *Declaration) Is(t DeviceType) *Declaration {
	d.deviceType = t
	return d
}

// inventory is what the home declares, for the host.
func (h *Home) inventory() []wire.DeviceDeclaration {
	out := make([]wire.DeviceDeclaration, 0, len(h.declared))
	for _, d := range h.declared {
		out = append(out, wire.DeviceDeclaration{
			Name: d.name, Plugin: d.plugin, ID: d.id, Room: d.room, DeviceType: devicetype.Type(d.deviceType),
		})
	}
	return out
}

// checkInventory refuses declarations that disagree among themselves, or name a plugin the home
// doesn't import.
func (h *Home) checkInventory() error {
	errs := []error{registry.CheckDeclarations(wire.Declarations(h.inventory()))}
	for _, d := range h.declared {
		if d.plugin != "" && !slices.ContainsFunc(h.plugins, func(p *Plugin) bool { return p.name == d.plugin }) {
			errs = append(errs, fmt.Errorf("device %s: Hardware names plugin %s, which this home doesn't import with Plugin",
				d.name, d.plugin))
		}
	}
	return errors.Join(errs...)
}
