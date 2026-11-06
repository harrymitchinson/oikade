// Package relays is simulated relays, one per device, as a plugin: the same code runs as an outside
// process speaking the MQTT contract and as a plugin a home imports.
package relays

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// relay is a simulated relay. It starts off and follows /set, as a real relay follows its wall
// switch.
type relay struct {
	id         string
	capability string
	on         bool
}

func (r *relay) state() contractv1.State { return contractv1.State{r.capability: r.on} }

// switches is the simswitch handler: one simulated relay per declared device.
type switches struct {
	mu sync.Mutex
	// order is the declared order of the relays, so they're reported deterministically.
	order   []*relay
	relays  map[string]*relay
	reports []contractv1.DeviceReport
	// discovered is what the simulated app holds: every relay, and one spare nobody declared.
	discovered []contractv1.DiscoveredDevice
}

// manufacturer and model are what every simulated relay reports.
const (
	manufacturer = "Oikade"
	model        = "Simulated switch"
)

// spare is a relay in the simulated app that no Device declares.
var spare = contractv1.DiscoveredDevice{
	ID: "spare-relay", Manufacturer: manufacturer, Model: model,
	Capabilities: []string{"switch.on"}, Name: "Spare relay",
}

var _ plugin.Handler = (*switches)(nil)

// New returns a plugin of simulated relays for the devices.
func New(devices []contractv1.DeclaredDevice) plugin.Handler { return newSwitches(devices) }

// newSwitches turns the declared devices into relays and the devices report. A device with a bad
// capability parameter is reported not found, with the reason. A device with the parameter
// simulate: unstable-id is discovered under a new ID each start, which breaks the contract, so the
// conformance suite's own tests can see it fail.
func newSwitches(declared []contractv1.DeclaredDevice) *switches {
	s := &switches{relays: map[string]*relay{}}
	for _, d := range declared {
		c, err := plugin.RelayCapability(d.Parameters)
		if err != nil {
			s.reports = append(s.reports, contractv1.DeviceReport{ID: d.ID, Error: err.Error()})
			continue
		}
		r := &relay{id: d.ID, capability: c}
		s.order = append(s.order, r)
		s.relays[d.ID] = r
		s.reports = append(s.reports, contractv1.DeviceReport{
			ID: d.ID, Found: true, Manufacturer: manufacturer, Model: model, Capabilities: []string{c},
		})
		id := d.ID
		if d.Parameters["simulate"] == "unstable-id" {
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			id = d.ID + "-" + hex.EncodeToString(b)
		}
		s.discovered = append(s.discovered, contractv1.DiscoveredDevice{
			ID: id, Manufacturer: manufacturer, Model: model, Capabilities: []string{c},
			Name: d.DisplayName, Room: d.Room,
		})
	}
	// A host records what it's shown, so the spare can come back declared.
	if s.relays[spare.ID] == nil {
		s.discovered = append(s.discovered, spare)
	}
	return s
}

// Serve reports what the simulated app holds, then every relay as available and off. Simulated
// relays never change by themselves, so there's nothing to follow afterwards.
func (s *switches) Serve(ctx context.Context, p *plugin.Plugin) error {
	if err := p.SetDiscovered(ctx, s.discovered); err != nil {
		return err
	}
	if err := p.SetDevices(ctx, s.reports, 0); err != nil {
		return err
	}
	for _, r := range s.order {
		if err := p.SetAvailability(ctx, r.id, true); err != nil {
			return err
		}
		if err := s.publishInitial(ctx, p, r); err != nil {
			return err
		}
	}
	return nil
}

// publishInitial publishes a relay's state holding the lock, so a command applied meanwhile
// can't be overwritten by the state read before it.
func (s *switches) publishInitial(ctx context.Context, p *plugin.Plugin, r *relay) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return p.SetState(ctx, r.id, r.state())
}

// Set switches a relay and reports its new state.
func (s *switches) Set(ctx context.Context, p *plugin.Plugin, cmd contractv1.SetCommand) error {
	state, err := s.apply(cmd)
	if err != nil {
		return err
	}
	return p.SetState(ctx, cmd.ID, state)
}

func (s *switches) apply(cmd contractv1.SetCommand) (contractv1.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.relays[cmd.ID]
	if !ok || r.capability != cmd.Capability {
		return nil, fmt.Errorf("no switch %s with %s", cmd.ID, cmd.Capability)
	}
	on, ok := cmd.Value.(bool)
	if !ok {
		return nil, fmt.Errorf("%s must be a bool", cmd.Capability)
	}
	r.on = on
	return r.state(), nil
}
