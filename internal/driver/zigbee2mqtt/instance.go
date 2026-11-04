// Package zigbee2mqtt is the driver for Zigbee2MQTT instances.
package zigbee2mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// Instance follows one Zigbee2MQTT instance on the bus.
type Instance struct {
	// Key is the Zigbee2MQTT object.
	Key       types.NamespacedName
	BaseTopic string
	Bus       mqtt.Bus
	Sink      driver.Sink
	Now       func() time.Time
	Log       logr.Logger

	mu          sync.Mutex
	unsubscribe func()
	declared    map[string]types.NamespacedName // IEEE address -> Device
	paired      map[string]*paired              // IEEE address -> paired device
	byName      map[string]string               // friendly name -> IEEE address
	values      map[string]map[string]any       // IEEE address -> capability -> value
}

type paired struct {
	friendlyName string
	bindings     []binding
	info         driver.Info
}

var _ driver.Setter = (*Instance)(nil)

// Start subscribes to the instance's topics.
func (i *Instance) Start(ctx context.Context) error {
	unsubscribe, err := i.Bus.Subscribe(ctx, i.BaseTopic+"/#", i.handle)
	i.mu.Lock()
	i.unsubscribe = unsubscribe
	i.mu.Unlock()
	return err
}

// Stop unsubscribes. Devices are not reported as lost; the caller drops them.
func (i *Instance) Stop() {
	i.mu.Lock()
	unsubscribe := i.unsubscribe
	i.unsubscribe = nil
	i.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
}

// SetDevices replaces the declared devices (IEEE address -> Device) and re-reports discovery.
func (i *Instance) SetDevices(declared map[string]types.NamespacedName) {
	i.mu.Lock()
	i.declared = maps.Clone(declared)
	emit := i.discoveryLocked()
	i.mu.Unlock()
	emit()
}

// Set publishes a /set command for a capability.
func (i *Instance) Set(ctx context.Context, dev types.NamespacedName, capability string, value any) error {
	i.mu.Lock()
	var ieee string
	for addr, d := range i.declared {
		if d == dev {
			ieee = addr
		}
	}
	p := i.paired[ieee]
	i.mu.Unlock()
	if ieee == "" || p == nil {
		return fmt.Errorf("device %s is not paired with %s", dev, i.Key)
	}
	idx := slices.IndexFunc(p.bindings, func(b binding) bool { return b.capability == capability })
	if idx < 0 {
		return fmt.Errorf("device %s has no capability %s", dev, capability)
	}
	b := p.bindings[idx]
	payload, err := json.Marshal(map[string]any{b.property: b.encode(value)})
	if err != nil {
		return err
	}
	// Address by IEEE: it works before Zigbee2MQTT has picked up the operator's friendly name.
	return i.Bus.Publish(ctx, i.BaseTopic+"/"+ieee+"/set", payload, false)
}

func (i *Instance) handle(m mqtt.Message) {
	rest, ok := strings.CutPrefix(m.Topic, i.BaseTopic+"/")
	if !ok {
		return
	}
	switch {
	case rest == "bridge/devices":
		i.handleDevices(m.Payload)
	case strings.HasPrefix(rest, "bridge/"):
	case strings.HasSuffix(rest, "/availability"):
		i.handleAvailability(strings.TrimSuffix(rest, "/availability"), m.Payload)
	case !strings.Contains(rest, "/"):
		i.handleState(rest, m.Payload)
	}
}

func (i *Instance) handleDevices(payload []byte) {
	var list []bridgeDevice
	if err := json.Unmarshal(payload, &list); err != nil {
		i.Log.Error(err, "Failed to parse Zigbee2MQTT device list", "instance", i.Key)
		return
	}
	var inventory []driver.AppDevice
	i.mu.Lock()
	i.paired, i.byName = map[string]*paired{}, map[string]string{}
	for _, d := range list {
		if d.Type == "Coordinator" || d.Definition == nil {
			continue
		}
		bs := bindings(d.Definition.Exposes)
		p := &paired{friendlyName: d.FriendlyName, bindings: bs, info: driver.Info{
			Found: true, Manufacturer: d.Definition.Vendor, Model: d.Definition.Model,
		}}
		for _, b := range bs {
			p.info.Capabilities = append(p.info.Capabilities, b.capability)
		}
		i.paired[d.IEEEAddress] = p
		i.byName[d.FriendlyName] = d.IEEEAddress
		i.byName[d.IEEEAddress] = d.IEEEAddress
		app := driver.AppDevice{
			ID: d.IEEEAddress, Manufacturer: p.info.Manufacturer, Model: p.info.Model,
			Capabilities: slices.Clone(p.info.Capabilities),
		}
		// Zigbee2MQTT names a device by its IEEE address until you rename it.
		if d.FriendlyName != d.IEEEAddress {
			app.Name = d.FriendlyName
		}
		inventory = append(inventory, app)
	}
	emit := i.discoveryLocked()
	i.mu.Unlock()
	emit()
	if inventory == nil {
		inventory = []driver.AppDevice{}
	}
	slices.SortFunc(inventory, func(a, b driver.AppDevice) int { return strings.Compare(a.ID, b.ID) })
	i.Sink.Inventory(driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: i.Key}, inventory)
}

// discoveryLocked computes discovery events; call the returned func after unlocking.
func (i *Instance) discoveryLocked() func() {
	type found struct {
		dev  types.NamespacedName
		info driver.Info
	}
	events := make([]found, 0, len(i.declared))
	for ieee, dev := range i.declared {
		info := driver.Info{}
		if p, ok := i.paired[ieee]; ok {
			info = p.info
		}
		events = append(events, found{dev, info})
	}
	undeclared := 0
	for ieee := range i.paired {
		if _, ok := i.declared[ieee]; !ok {
			undeclared++
		}
	}
	known := i.paired != nil
	return func() {
		for _, e := range events {
			i.Sink.Discovered(e.dev, e.info)
		}
		if known {
			i.Sink.Undeclared(driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: i.Key}, undeclared)
		}
	}
}

func (i *Instance) handleAvailability(name string, payload []byte) {
	// Zigbee2MQTT 2.x publishes {"state":"online"}; older versions publish a bare string.
	state := string(payload)
	var obj struct {
		State string `json:"state"`
	}
	if json.Unmarshal(payload, &obj) == nil && obj.State != "" {
		state = obj.State
	}
	i.mu.Lock()
	dev, ok := i.deviceLocked(name)
	i.mu.Unlock()
	if ok {
		i.Sink.Availability(dev, state == "online")
	}
}

func (i *Instance) handleState(name string, payload []byte) {
	var state map[string]any
	if err := json.Unmarshal(payload, &state); err != nil {
		return
	}
	now := i.Now()

	i.mu.Lock()
	dev, ok := i.deviceLocked(name)
	if !ok {
		i.mu.Unlock()
		return
	}
	ieee := i.byName[name]
	var updates []driver.Update
	for _, b := range i.paired[ieee].bindings {
		raw, ok := state[b.property]
		if !ok {
			continue
		}
		v, err := b.decode(raw)
		if err != nil {
			i.Log.V(1).Info("Ignored invalid value", "device", dev, "capability", b.capability, "error", err.Error())
			continue
		}
		if i.values == nil {
			i.values = map[string]map[string]any{}
		}
		if i.values[ieee] == nil {
			i.values[ieee] = map[string]any{}
		}
		prev, had := i.values[ieee][b.capability]
		if had && prev == v {
			continue
		}
		i.values[ieee][b.capability] = v
		updates = append(updates, driver.Update{Device: dev, Capability: b.capability, Value: v, Previous: prev, Time: now})
	}
	i.mu.Unlock()

	i.Sink.Seen(dev, now)
	for _, u := range updates {
		i.Sink.Changed(u)
	}
}

// deviceLocked resolves a friendly name or IEEE address to a declared, paired Device.
func (i *Instance) deviceLocked(name string) (types.NamespacedName, bool) {
	ieee, ok := i.byName[name]
	if !ok {
		return types.NamespacedName{}, false
	}
	dev, ok := i.declared[ieee]
	return dev, ok
}
