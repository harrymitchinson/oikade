// Package relays is simulated relays, one per device, as a plugin: the same code runs as an outside
// process speaking the MQTT contract and as a plugin a home imports.
package relays

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"oikade.dev/contract/v1"
	"oikade.dev/sdk/plugin"
)

// relay is a simulated relay. It starts off and follows /set, as a real relay follows its wall
// switch. One that simulates an air conditioner has a mode too, which starts at auto.
type relay struct {
	id     string
	values contract.State
}

func (r *relay) state() contract.State { return maps.Clone(r.values) }

// capabilities are the relay's, sorted.
func (r *relay) capabilities() []string { return slices.Sorted(maps.Keys(r.values)) }

// switches is the simswitch handler: one simulated relay per declared device.
type switches struct {
	mu sync.Mutex
	// order is the declared order of the relays, so they're reported deterministically.
	order   []*relay
	relays  map[string]*relay
	reports []contract.DeviceReport
	// discovered is what the simulated app holds: every relay, and one spare nobody declared.
	discovered []contract.DiscoveredDevice
	// problem is the plugin's own error, empty unless a device asks for one.
	problem string
	// needsSetup is true until a setup runs, if a device asks for it, and refuseSetup fails every
	// setup.
	needsSetup, refuseSetup bool
}

// manufacturer, model and firmware are what every simulated relay reports, and a serial number its
// own.
const (
	manufacturer = "Oikade"
	model        = "Simulated switch"
	firmware     = "1.0.0"
)

func serial(id string) string { return "SIM-" + strings.ToUpper(id) }

// spare is a relay in the simulated app that no Device declares.
var spare = contract.DiscoveredDevice{
	ID: "spare-relay", Manufacturer: manufacturer, Model: model, SerialNumber: serial("spare-relay"),
	FirmwareVersion: firmware,
	Capabilities:    []string{"on"}, Name: "Spare relay",
}

var (
	_ plugin.Handler = (*switches)(nil)
	_ plugin.Setup   = (*switches)(nil)
)

// PairingSecret is the secret simswitch's setup keeps.
const PairingSecret = "simswitch-pairing-code"

// New returns a plugin of simulated relays for the devices.
func New(devices []contract.DeclaredDevice) plugin.Handler { return newSwitches(devices) }

// fault is what a device's ID asks the relays to simulate: the part after its last dot. The
// conformance suite runs the fixture with an environment of its own, so the devices file is the only
// way in.
func fault(id string) string {
	if _, after, ok := strings.CutLast(id, "."); ok {
		return after
	}
	return ""
}

// discoveredAs is a relay as the simulated app lists it, under its name there.
func discoveredAs(d contract.DeclaredDevice, id string, capabilities ...string) contract.DiscoveredDevice {
	return contract.DiscoveredDevice{
		ID: id, Manufacturer: manufacturer, Model: model, SerialNumber: serial(d.ID), FirmwareVersion: firmware,
		Capabilities: capabilities,
		Name:         d.DisplayName, Room: d.Room,
	}
}

// newSwitches turns the declared devices into relays and the devices report. A device whose ID
// ends in .misconfigured is reported not found, with the reason, as a plugin reports
// configuration it can't use. One with .needs-key is found, and discovered, and can't be used
// until it's given something, as a device a plugin can see but not read. One with .refused-login is
// reported not found, and the plugin says in its status that its simulated account refused its
// login, as a cloud plugin would. One with .needs-setup makes the plugin say it needs its
// setup run, until it is. All four keep to the contract.
//
// One with .air-conditioner has a mode as well as an on, so the suite writes a name.
//
// Three break it, so the conformance suite's own tests can see it fail: one with .unstable-id
// is discovered under a new ID each start, and one with .old-name reports a capability by a
// name Oikade no longer knows, and one with .refused-setup makes every setup fail.
func newSwitches(declared []contract.DeclaredDevice) *switches {
	s := &switches{relays: map[string]*relay{}}
	for _, d := range declared {
		simulate := fault(d.ID)
		if simulate == "misconfigured" {
			s.reports = append(s.reports, contract.DeviceReport{ID: d.ID, Error: "this relay was told it's misconfigured"})
			continue
		}
		if simulate == "refused-login" {
			s.problem = "the simulated account refused the login"
			s.reports = append(s.reports, contract.DeviceReport{ID: d.ID, Error: "not seen: the plugin can't log in"})
			continue
		}
		if simulate == "needs-key" {
			s.reports = append(s.reports, contract.DeviceReport{
				ID: d.ID, Found: true, Capabilities: []string{contract.On}, Error: "no key: give this relay one",
			})
			s.discovered = append(s.discovered, discoveredAs(d, d.ID, contract.On))
			continue
		}
		switch simulate {
		case "needs-setup":
			s.needsSetup = true
		case "refused-setup":
			s.refuseSetup = true
		}
		r := &relay{id: d.ID, values: contract.State{contract.On: false}}
		switch simulate {
		case "old-name":
			// An old name, which Oikade doesn't know.
			r.values = contract.State{"switch.on": false}
		case "air-conditioner":
			r.values[contract.Mode] = contract.ModeAuto
		}
		s.order = append(s.order, r)
		s.relays[d.ID] = r
		// No DeviceType: a relay can't tell what it switches.
		s.reports = append(s.reports, contract.DeviceReport{
			ID: d.ID, Found: true, Manufacturer: manufacturer, Model: model, SerialNumber: serial(d.ID),
			FirmwareVersion: firmware, Capabilities: r.capabilities(),
		})
		id := d.ID
		if simulate == "unstable-id" {
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			id = d.ID + "-" + hex.EncodeToString(b)
		}
		s.discovered = append(s.discovered, discoveredAs(d, id, r.capabilities()...))
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
	if err := p.SetError(ctx, s.problem); err != nil {
		return err
	}
	s.mu.Lock()
	needs := s.needsSetup
	s.mu.Unlock()
	if err := p.SetNeedsSetup(ctx, needs); err != nil {
		return err
	}
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

// Set switches a relay, or changes its mode, and reports its new state.
func (s *switches) Set(ctx context.Context, p *plugin.Plugin, cmd contract.SetCommand) error {
	state, err := s.apply(cmd)
	if err != nil {
		return err
	}
	return p.SetState(ctx, cmd.ID, state)
}

func (s *switches) apply(cmd contract.SetCommand) (contract.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current any
	r, ok := s.relays[cmd.ID]
	if ok {
		current, ok = r.values[cmd.Capability]
	}
	if !ok {
		return nil, fmt.Errorf("no switch %s with %s", cmd.ID, cmd.Capability)
	}
	// The plugin SDK has checked the value against the capability. This is the relay's own check.
	if reflect.TypeOf(cmd.Value) != reflect.TypeOf(current) {
		return nil, fmt.Errorf("%s must be a %T", cmd.Capability, current)
	}
	r.values[cmd.Capability] = cmd.Value
	return r.state(), nil
}

// Setup goes through every kind of step a setup can take, as a cloud plugin's would: it says what's
// needed, asks for a name and a pairing code, opens a simulated sign-in, and keeps the code as a
// secret. Any answers will do.
func (s *switches) Setup(ctx context.Context, p *plugin.Plugin, session *plugin.Session) (string, error) {
	if err := session.Say(ctx, "Simulated relays pair with any code"); err != nil {
		return "", err
	}
	hub, err := session.Ask(ctx, "Hub name")
	if err != nil {
		return "", err
	}
	code, err := session.AskSecret(ctx, "Pairing code")
	if err != nil {
		return "", err
	}
	if _, err := session.Open(ctx, "https://example.com/simswitch/sign-in", "http://127.0.0.1:8765/callback"); err != nil {
		return "", err
	}
	s.mu.Lock()
	refuse := s.refuseSetup
	s.mu.Unlock()
	if refuse {
		return "", errors.New("the simulated hub refused the pairing code: run oikade setup again with another")
	}
	if err := session.KeepSecret(ctx, PairingSecret, code); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.needsSetup = false
	n := len(s.order)
	s.mu.Unlock()
	if err := p.SetNeedsSetup(ctx, false); err != nil {
		return "", err
	}
	if hub == "" {
		hub = "simswitch"
	}
	return fmt.Sprintf("%s is set up: %d relays", hub, n), nil
}
