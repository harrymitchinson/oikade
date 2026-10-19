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

package main

import (
	"context"
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

// switches is the example-switch handler: one simulated relay per declared device.
type switches struct {
	mu sync.Mutex
	// order is the declared order of the relays, so they're reported deterministically.
	order   []*relay
	relays  map[string]*relay
	reports []contractv1.DeviceReport
}

var _ plugin.Handler = (*switches)(nil)

// newSwitches turns the declared devices into relays and the devices report. A device with a bad
// capability parameter is reported not found, with the reason.
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
			ID: d.ID, Found: true, Manufacturer: "Oikade", Model: "Simulated switch", Capabilities: []string{c},
		})
	}
	return s
}

// Serve reports every relay as available and off. Simulated relays never change by themselves,
// so there's nothing to follow afterwards.
func (s *switches) Serve(ctx context.Context, p *plugin.Plugin) error {
	if err := p.SetDevices(ctx, s.reports, 0); err != nil {
		return err
	}
	for _, r := range s.order {
		if err := p.SetAvailability(ctx, r.id, true); err != nil {
			return err
		}
		s.mu.Lock()
		state := r.state()
		s.mu.Unlock()
		if err := p.SetState(ctx, r.id, state); err != nil {
			return err
		}
	}
	return nil
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
