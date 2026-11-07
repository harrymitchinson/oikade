package zigbee2mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// earlyLimit bounds what's kept for the next device list.
const earlyLimit = 4096

// defaultGetInterval paces the /get requests after a device list, so that a large network doesn't
// flood the broker, or Zigbee2MQTT's queue behind it.
const defaultGetInterval = 200 * time.Millisecond

// Instance follows one Zigbee2MQTT instance on the bus.
type Instance struct {
	// Key is the Zigbee2MQTT object.
	Key       ident.ID
	BaseTopic string
	Bus       mqtt.Bus
	Sink      driver.Sink
	Now       func() time.Time
	Log       *slog.Logger
	// OnBridgeState, if set, hears <base>/bridge/state. OnListed, if set, runs after each device list
	// and whatever it resolved.
	OnBridgeState func(online bool)
	OnListed      func()
	// getInterval is the time between /get requests. Zero is defaultGetInterval.
	getInterval time.Duration
	// requested, if set, runs when a round of /get requests ends. Tests wait for it.
	requested func()

	mu          sync.Mutex
	ctx         context.Context
	unsubscribe func()
	stopGets    context.CancelFunc
	declared    map[string]ident.ID       // IEEE address -> Device
	paired      map[string]*paired        // IEEE address -> paired device
	byName      map[string]string         // friendly name -> IEEE address
	values      map[string]map[string]any // IEEE address -> capability -> value
	offline     map[string]bool           // IEEE address -> whether its last availability was offline
	// early holds the latest message on each topic that resolves to no device, until the next device
	// list, which may be what resolves it: a broker sends retained messages in any order, and a new
	// device can report before it's listed.
	early map[string][]byte
}

type paired struct {
	bindings []binding
	info     driver.Info
}

var _ driver.Setter = (*Instance)(nil)

// Start subscribes to the instance's topics.
func (i *Instance) Start(ctx context.Context) error {
	i.Log = logging.OrDiscard(i.Log)
	i.mu.Lock()
	i.ctx = ctx
	i.mu.Unlock()
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
	// A device list taken in meanwhile starts no round.
	i.ctx = nil
	if i.stopGets != nil {
		i.stopGets()
	}
	i.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
}

// SetDevices replaces the declared devices (IEEE address -> Device) and re-reports discovery.
func (i *Instance) SetDevices(declared map[string]ident.ID) {
	i.mu.Lock()
	i.declared = maps.Clone(declared)
	emit := i.discoveryLocked()
	i.mu.Unlock()
	emit()
}

// Set publishes a /set command for a capability.
func (i *Instance) Set(ctx context.Context, dev ident.ID, capability string, value any) error {
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
	// Address by IEEE: the friendly name is Zigbee2MQTT's own and may change at any time.
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
	case rest == "bridge/state":
		if i.OnBridgeState != nil {
			i.OnBridgeState(onlinePayload(m.Payload))
		}
	case strings.HasPrefix(rest, "bridge/"):
	case i.ownCommand(rest):
	case i.hold(rest, m.Payload):
	default:
		i.handleDevice(rest, m.Payload)
	}
}

// ownCommand says whether a topic is a /get or a /set sent to an IEEE address, as the plugin sends
// them: the broker echoes them back, and they mustn't be held for the next device list.
func (i *Instance) ownCommand(rest string) bool {
	ieee, ok := strings.CutSuffix(rest, "/get")
	if !ok {
		ieee, ok = strings.CutSuffix(rest, "/set")
	}
	return ok && isIEEE(ieee)
}

// isIEEE says whether s is an IEEE address as Zigbee2MQTT writes one, such as 0x00124b0000000004.
func isIEEE(s string) bool {
	hex, ok := strings.CutPrefix(s, "0x")
	return ok && len(hex) == 16 && strings.Trim(hex, "0123456789abcdef") == ""
}

// hold keeps a message that resolves to no device for the next device list, and says whether it did.
func (i *Instance) hold(rest string, payload []byte) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	name := strings.TrimSuffix(rest, "/availability")
	if _, ok := i.deviceLocked(name); ok {
		delete(i.early, rest) // This is newer, and a replay must not undo it.
		return false
	}
	if i.early == nil {
		i.early = map[string][]byte{}
	}
	if _, held := i.early[rest]; held || len(i.early) < earlyLimit {
		i.early[rest] = slices.Clone(payload)
	}
	return true
}

func (i *Instance) handleDevice(rest string, payload []byte) {
	if name, ok := strings.CutSuffix(rest, "/availability"); ok {
		i.handleAvailability(name, payload)
		return
	}
	// A friendly name set in the frontend may contain slashes, so any other topic is a device's
	// state if it resolves to one. Commands such as <name>/set don't.
	i.handleState(rest, payload)
}

func (i *Instance) handleDevices(payload []byte) {
	var list []bridgeDevice
	if err := json.Unmarshal(payload, &list); err != nil {
		i.Log.Error("Failed to parse Zigbee2MQTT device list", "err", err, "instance", i.Key)
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
		p := &paired{bindings: bs, info: driver.Info{
			Found: true, Manufacturer: d.Definition.Vendor, Model: d.Definition.Model,
			DeviceType: deviceType(d.Definition.Exposes),
		}}
		for _, b := range bs {
			p.info.Capabilities = append(p.info.Capabilities, b.capability)
		}
		i.paired[d.IEEEAddress] = p
		i.byName[d.FriendlyName] = d.IEEEAddress
		app := driver.AppDevice{
			ID: d.IEEEAddress, Manufacturer: p.info.Manufacturer, Model: p.info.Model,
			Capabilities: slices.Clone(p.info.Capabilities), DeviceType: p.info.DeviceType,
		}
		// Zigbee2MQTT names a device by its IEEE address until you rename it.
		if d.FriendlyName != d.IEEEAddress {
			app.Name = d.FriendlyName
		}
		inventory = append(inventory, app)
	}
	// Added after every name, so an address wins over a frontend name that equals it.
	for ieee := range i.paired {
		i.byName[ieee] = ieee
	}
	// What a device no longer has, or a device that left, has lost its value everywhere else, so
	// its next report isn't a repeat.
	maps.DeleteFunc(i.values, func(ieee string, values map[string]any) bool {
		p := i.paired[ieee]
		if p == nil {
			return true
		}
		maps.DeleteFunc(values, func(c string, _ any) bool {
			return !slices.ContainsFunc(p.bindings, func(b binding) bool { return b.capability == c })
		})
		return false
	})
	emit := i.discoveryLocked()
	early := i.early
	i.early = nil
	i.mu.Unlock()
	emit()
	if inventory == nil {
		inventory = []driver.AppDevice{}
	}
	slices.SortFunc(inventory, func(a, b driver.AppDevice) int { return strings.Compare(a.ID, b.ID) })
	i.Sink.Inventory(driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: i.Key}, inventory)
	for _, rest := range slices.Sorted(maps.Keys(early)) {
		i.handleDevice(rest, early[rest])
	}
	i.requestState()
	if i.OnListed != nil {
		i.OnListed()
	}
}

// request is one /get: a property to read from a device, for a capability.
type request struct {
	ieee, property, capability string
}

// requestState asks for each value a device can be asked for and that nothing has reported, such
// as a device that doesn't retain its state. A device Zigbee2MQTT says is offline isn't asked. It runs after the held messages are replayed, so a
// retained value is neither re-requested nor overwritten. A device list replaces the last round.
//
// A reply is a state message like any other, and the first value of a capability counts as no
// change, so it fires nothing a retained value wouldn't. A device that doesn't answer, such as one
// asleep, leaves its value unknown: Zigbee2MQTT says nothing, and neither does Oikade.
func (i *Instance) requestState() {
	i.mu.Lock()
	if i.stopGets != nil {
		i.stopGets()
	}
	if i.ctx == nil {
		i.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(i.ctx)
	i.stopGets = cancel
	var requests []request
	for _, ieee := range slices.Sorted(maps.Keys(i.paired)) {
		// The plugin declares every paired device as it takes in the list, before this runs.
		if _, ok := i.declared[ieee]; !ok || i.offline[ieee] {
			continue
		}
		for _, b := range i.paired[ieee].bindings {
			if b.get && !i.knownLocked(ieee, b.capability) {
				requests = append(requests, request{ieee, b.property, b.capability})
			}
		}
	}
	i.mu.Unlock()
	go i.get(ctx, requests)
}

// get sends requests one interval apart, skipping any value reported meanwhile.
func (i *Instance) get(ctx context.Context, requests []request) {
	if i.requested != nil {
		defer i.requested()
	}
	interval := i.getInterval
	if interval <= 0 {
		interval = defaultGetInterval
	}
	t := time.NewTimer(interval)
	defer t.Stop()
	for _, r := range requests {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(interval)
		i.mu.Lock()
		skip := i.knownLocked(r.ieee, r.capability) || i.offline[r.ieee]
		i.mu.Unlock()
		if skip {
			continue
		}
		// Zigbee2MQTT's documented payload names one property, with an empty value. It's addressed
		// by IEEE, like a /set.
		payload, _ := json.Marshal(map[string]string{r.property: ""})
		if err := i.Bus.Publish(ctx, i.BaseTopic+"/"+r.ieee+"/get", payload, false); err != nil {
			// The broker has gone. When it's back, it replays what's retained, and the next device
			// list asks again.
			i.Log.Debug("Stopped requesting device state", "instance", i.Key, "err", err)
			return
		}
	}
}

func (i *Instance) knownLocked(ieee, capability string) bool {
	_, ok := i.values[ieee][capability]
	return ok
}

// discoveryLocked computes discovery events; call the returned func after unlocking.
func (i *Instance) discoveryLocked() func() {
	type found struct {
		dev  ident.ID
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
			i.Sink.Undeclared(driver.InstanceKey{Driver: driver.Zigbee2MQTT, ID: i.Key}, undeclared)
		}
	}
}

func (i *Instance) handleAvailability(name string, payload []byte) {
	online := onlinePayload(payload)
	i.mu.Lock()
	dev, ok := i.deviceLocked(name)
	if ok {
		if i.offline == nil {
			i.offline = map[string]bool{}
		}
		i.offline[i.byName[name]] = !online
	}
	i.mu.Unlock()
	if ok {
		i.Sink.Availability(dev, online)
	}
}

func (i *Instance) handleState(name string, payload []byte) {
	now := i.Now()
	i.mu.Lock()
	// Most topics under the base, such as <name>/set, aren't a device's state: resolve before
	// decoding.
	dev, ok := i.deviceLocked(name)
	var state map[string]any
	if !ok || json.Unmarshal(payload, &state) != nil {
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
			i.Log.Debug("Ignored invalid value", "device", dev, "capability", b.capability, "err", err)
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

	if len(updates) > 0 {
		i.Sink.Changed(updates...)
	}
	i.Sink.Seen(dev, now)
}

// deviceLocked resolves a friendly name or IEEE address to a declared, paired Device.
func (i *Instance) deviceLocked(name string) (ident.ID, bool) {
	ieee, ok := i.byName[name]
	if !ok {
		return ident.ID{}, false
	}
	dev, ok := i.declared[ieee]
	return dev, ok
}
