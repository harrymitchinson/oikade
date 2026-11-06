package host

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
)

// SimulatedDevice is a device with no hardware behind it, from the file --simulate names.
type SimulatedDevice struct {
	Name         string   `json:"name"`
	Room         string   `json:"room,omitempty"`
	Capabilities []string `json:"capabilities"`
	// Values are its first values, by capability.
	Values map[string]any `json:"values,omitempty"`
	// Control arbitrates capabilities, as a Device's spec does.
	Control []control.Entry `json:"control,omitempty"`
}

// ReadSimulated reads the devices from a JSON file.
func ReadSimulated(path string) ([]SimulatedDevice, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var devices []SimulatedDevice
	if err := json.Unmarshal(raw, &devices); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return devices, nil
}

// simInstance is the driver instance simulated devices belong to. A simulated device's name in the
// file is its ID, and its name in its app.
var simInstance = driver.InstanceKey{Driver: "simulated", NamespacedName: types.NamespacedName{Name: "simulator"}}

// simulator is a driver with no hardware: writes take effect at once, and lines of JSON on its
// input change a device as if from outside.
type simulator struct {
	store *hub.Store
	known *inventory
	log   logr.Logger
	mu    sync.Mutex
}

// simChange is one line of the simulator's input: a change, or a device being paired.
type simChange struct {
	Device     string           `json:"device"`
	Capability string           `json:"capability"`
	Value      any              `json:"value"`
	Pair       *SimulatedDevice `json:"pair,omitempty"`
}

// start reports the devices, and with them everything the registry says it has here.
func (s *simulator) start(devices []SimulatedDevice) error {
	s.known.attach(simInstance)
	for _, d := range devices {
		if err := s.pair(d); err != nil {
			return err
		}
	}
	return nil
}

// pair reports a device as a driver would when it's paired.
func (s *simulator) pair(d SimulatedDevice) error {
	dev, err := s.known.paired(simInstance, driver.AppDevice{
		ID: d.Name, Name: d.Name, Room: d.Room, Capabilities: d.Capabilities,
	}, d.Control)
	if err != nil {
		return fmt.Errorf("device %s: %w", d.Name, err)
	}
	for name, v := range d.Values {
		if err := s.report(dev, name, v); err != nil {
			return fmt.Errorf("device %s: %w", d.Name, err)
		}
	}
	s.store.Discovered(dev, driver.Info{Found: true, Capabilities: d.Capabilities})
	return nil
}

// read applies changes from r until it ends.
func (s *simulator) read(r io.Reader) {
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		var c simChange
		if err := json.Unmarshal(lines.Bytes(), &c); err != nil {
			s.log.Error(err, "Ignored unreadable simulated change")
			continue
		}
		if c.Pair != nil {
			if err := s.pair(*c.Pair); err != nil {
				s.log.Error(err, "Ignored simulated device", "device", c.Pair.Name)
			}
			continue
		}
		dev, ok := s.known.key(simInstance, c.Device)
		if !ok {
			s.log.Info("Ignored simulated change to a device not on record", "device", c.Device)
			continue
		}
		if err := s.report(dev, c.Capability, c.Value); err != nil {
			s.log.Error(err, "Ignored simulated change", "device", c.Device, "capability", c.Capability)
		}
	}
}

// report records a device's new value.
func (s *simulator) report(dev types.NamespacedName, name string, value any) error {
	v, err := capability.Validate(name, value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.store.Get(dev)
	if !ok {
		return fmt.Errorf("unknown device %s", dev.Name)
	}
	now := time.Now()
	s.store.Seen(dev, now)
	if previous, known := snap.Values[name]; known && previous == v {
		return nil
	}
	s.store.Changed(driver.Update{Device: dev, Capability: name, Value: v, Previous: snap.Values[name], Time: now})
	return nil
}

// Set implements driver.Setter.
func (s *simulator) Set(_ context.Context, dev types.NamespacedName, name string, value any) error {
	if err := s.report(dev, name, value); err != nil {
		return err
	}
	s.log.Info("Wrote simulated device", "device", dev.Name, "capability", name, "value", value)
	return nil
}
