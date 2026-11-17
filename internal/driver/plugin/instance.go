// Package plugin is the driver for plugins speaking the oikade/v1 contract (docs/plugin-contract.md).
package plugin

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
	"unicode"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// Reasons a plugin message is rejected, as counted in Stats.
const (
	ReasonMalformed             = "malformed"
	ReasonWrongContract         = "wrong_contract"
	ReasonUnknownCapability     = "unknown_capability"
	ReasonInvalidValue          = "invalid_value"
	ReasonUndeclaredDevice      = "undeclared_device"
	ReasonUnannouncedCapability = "unannounced_capability"
	ReasonUnknownDeviceType     = "unknown_device_type"
)

// Instance follows one plugin on the bus.
type Instance struct {
	// Key is the Plugin object.
	Key  ident.ID
	Root contractv1.Root
	Bus  mqtt.Bus
	Sink driver.Sink
	Now  func() time.Time
	Log  *slog.Logger

	// counting is held from computing the undeclared count until it's sent, so an older count
	// can't overtake a newer one. Take it before mu.
	counting    sync.Mutex
	mu          sync.Mutex
	unsubscribe func()
	declared    map[string]ident.ID // id -> Device
	// removed are the ids of devices you removed. A plugin that hears devices on the network can't
	// tell one from a device it hasn't been told of, so their messages are dropped but not counted.
	removed map[string]bool
	// status is nil until the plugin's status topic is seen.
	status *contractv1.Status
	// announced is nil until the devices topic is seen.
	announced map[string]driver.Info
	// discovered is nil unless the plugin sends a discovered list.
	discovered []contractv1.DiscoveredDevice
	// undeclared is the plugin's own count, used while it sends no discovered list.
	undeclared int
	// last holds each id's latest state and availability, so they can be replayed once the id is
	// declared and announced: the hub subscribes before it declares devices, and relays rarely re-report.
	last     map[string]*lastSeen
	values   map[string]map[string]any // id -> capability -> value
	rejected map[string]uint64
	logged   map[string]bool // reason/id pairs already logged
	// needsSetup is the devices report's, and sessions the setup sessions running, by name.
	needsSetup bool
	sessions   map[string]chan contractv1.SetupStep
}

type lastSeen struct {
	state     []byte
	stateAt   time.Time
	online    bool
	hasOnline bool
	hasState  bool
}

// Stats is the plugin's health as the decoder sees it.
type Stats struct {
	// Up is true while the retained status is online with a supported contract.
	Up bool
	// Rejected counts dropped messages by reason.
	Rejected map[string]uint64
}

var _ driver.Setter = (*Instance)(nil)

// Start subscribes to the plugin's topics.
func (i *Instance) Start(ctx context.Context) error {
	i.Log = logging.OrDiscard(i.Log)
	unsubscribe, err := i.Bus.Subscribe(ctx, i.Root.Filter(), i.handle)
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

// SetDevices replaces the declared devices (plugin-local id -> Device) and the ids of removed ones,
// and re-reports discovery.
func (i *Instance) SetDevices(declared map[string]ident.ID, removed map[string]bool) {
	i.counting.Lock()
	defer i.counting.Unlock()
	i.mu.Lock()
	// A device that leaves, or moves to another id, starts empty where it lands, so the values it
	// had must not hold back the next report that repeats them.
	for id, dev := range i.declared {
		if declared[id] != dev {
			delete(i.values, id)
		}
	}
	i.declared, i.removed = maps.Clone(declared), maps.Clone(removed)
	emit := i.discoveryLocked()
	// Only a discovered list's count depends on what's declared.
	counted := i.discovered != nil
	undeclared := i.undeclaredLocked()
	i.mu.Unlock()
	emit()
	if counted {
		i.sendUndeclared(undeclared)
	}
}

func (i *Instance) key() driver.InstanceKey {
	return driver.InstanceKey{Driver: driver.Plugin, ID: i.Key}
}

func (i *Instance) sendUndeclared(n int) { i.Sink.Undeclared(i.key(), n) }

// inventoryLocked is the discovered list for the Sink, sorted by ID, or nil.
func (i *Instance) inventoryLocked() []driver.AppDevice {
	if i.discovered == nil {
		return nil
	}
	out := make([]driver.AppDevice, 0, len(i.discovered))
	for _, d := range i.discovered {
		out = append(out, driver.AppDevice{
			ID: d.ID, Manufacturer: d.Manufacturer, Model: d.Model, Capabilities: slices.Clone(d.Capabilities),
			DeviceType: devicetype.Type(d.DeviceType), Name: d.Name, Room: d.Room,
		})
	}
	slices.SortFunc(out, func(a, b driver.AppDevice) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Discovered returns a copy of the devices the plugin sees in its app, validated, and false if it
// sends no discovered list.
func (i *Instance) Discovered() ([]contractv1.DiscoveredDevice, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.discovered == nil {
		return nil, false
	}
	out := make([]contractv1.DiscoveredDevice, len(i.discovered))
	for n, d := range i.discovered {
		d.Capabilities = slices.Clone(d.Capabilities)
		out[n] = d
	}
	return out, true
}

// Announced returns what the plugin last said about each device it reported, by its ID.
func (i *Instance) Announced() map[string]driver.Info {
	i.mu.Lock()
	defer i.mu.Unlock()
	return maps.Clone(i.announced)
}

// Stats returns a snapshot of the plugin's health.
func (i *Instance) Stats() Stats {
	i.mu.Lock()
	defer i.mu.Unlock()
	return Stats{Up: i.upLocked(), Rejected: maps.Clone(i.rejected)}
}

// maxProblem is the most of a plugin's error Oikade keeps, so an error page passed on whole can't
// flood oikade status.
const maxProblem = 200

// Problem is what the plugin says is wrong with it as a whole, while its status is online: one line,
// cut to maxProblem runes.
func (i *Instance) Problem() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.problemLocked()
}

func (i *Instance) problemLocked() string {
	if !i.upLocked() {
		return ""
	}
	return oneLine(i.status.Error)
}

// oneLine is s with control characters, newlines among them, as spaces, and cut to maxProblem runes.
func oneLine(s string) string {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsControl), " ")
	if r := []rune(s); len(r) > maxProblem {
		return string(r[:maxProblem-1]) + "…"
	}
	return s
}

// Forget drops what the plugin's last process announced. That process's devices are what it knew,
// and the next must say for itself: until it does, a write has nobody to take it. Devices aren't
// reported as lost, because a restart doesn't lose them.
func (i *Instance) Forget() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.announced = nil
}

// Set publishes a one-capability /set command. The value must already be validated.
func (i *Instance) Set(ctx context.Context, dev ident.ID, name string, value any) error {
	i.mu.Lock()
	var id string
	for k, d := range i.declared {
		if d == dev {
			id = k
		}
	}
	info, announced := i.announced[id]
	status := i.status
	i.mu.Unlock()
	switch {
	case id == "":
		return fmt.Errorf("device %s is not declared on plugin %s", dev, i.Key)
	case status != nil && status.Contract != contractv1.Contract:
		return fmt.Errorf("plugin %s speaks contract %d, not %d", i.Key, status.Contract, contractv1.Contract)
	case status != nil && status.State != contractv1.Online:
		return fmt.Errorf("plugin %s is offline", i.Key)
	case !announced || !info.Found:
		return fmt.Errorf("device %s is not found by plugin %s", dev, i.Key)
	case !slices.Contains(info.Capabilities, name):
		return fmt.Errorf("device %s has no capability %s", dev, name)
	}
	payload, err := contractv1.SetCommand{ID: id, Capability: name, Value: value}.Payload()
	if err != nil {
		return err
	}
	return i.Bus.Publish(ctx, i.Root.Set(id), payload, false)
}

func (i *Instance) handle(m mqtt.Message) {
	rest, ok := strings.CutPrefix(m.Topic, string(i.Root)+"/")
	if !ok {
		return
	}
	switch rest {
	case "status":
		i.handleStatus(m.Topic, m.Payload)
		return
	case "_setup/out":
		i.handleSetup(m.Topic, m.Payload, m.Retained)
		return
	case "_setup/in":
		// Our own requests.
		return
	}
	id, sub, _ := strings.Cut(rest, "/")
	if sub == "set" {
		// Our own commands.
		return
	}
	i.mu.Lock()
	wrong := i.status != nil && i.status.Contract != contractv1.Contract
	if wrong {
		i.rejectLocked(ReasonWrongContract, "", m.Topic)
	}
	i.mu.Unlock()
	if wrong {
		return
	}
	switch {
	case rest == "devices":
		i.handleDevices(m.Topic, m.Payload)
	case sub == "state":
		i.handleState(id, m.Topic, m.Payload)
	case sub == "availability":
		i.handleAvailability(id, m.Topic, m.Payload)
	}
	// Other topics are reserved for later minor revisions of the contract and ignored.
}

func (i *Instance) handleStatus(topic string, payload []byte) {
	var s contractv1.Status
	if err := json.Unmarshal(payload, &s); err != nil || (s.State != contractv1.Online && s.State != contractv1.Offline) {
		i.mu.Lock()
		i.rejectLocked(ReasonMalformed, "", topic)
		i.mu.Unlock()
		return
	}
	i.counting.Lock()
	defer i.counting.Unlock()
	i.mu.Lock()
	wasUp, was := i.upLocked(), i.problemLocked()
	i.status = &s
	up, problem := i.upLocked(), i.problemLocked()
	switch problem {
	case was:
	case "":
		i.Log.Info("Plugin no longer reports a problem", "plugin", i.Key)
	default:
		i.Log.Warn("Plugin reported a problem", "plugin", i.Key, "problem", problem)
	}
	var events []func()
	switch {
	case s.Contract != contractv1.Contract:
		i.rejectLocked(ReasonWrongContract, "", topic)
		// Forget everything learned under a contract we may have misread. An empty, non-nil
		// announcement reports every device as not found.
		i.announced, i.values, i.last, i.discovered, i.undeclared = map[string]driver.Info{}, nil, nil, nil, 0
		i.needsSetup = false
		i.endSessionsLocked("the plugin speaks another contract")
		events = append(events, i.discoveryLocked(), func() {
			i.sendUndeclared(0)
			i.Sink.Inventory(i.key(), nil)
		})
		events = append(events, i.allAvailabilityLocked(false))
	case !up:
		i.endSessionsLocked("the plugin went offline")
		events = append(events, i.allAvailabilityLocked(false))
	case !wasUp:
		events = append(events, i.replayLocked())
	}
	if up && !s.Setup {
		i.endSessionsLocked("the plugin no longer has a setup")
	}
	i.mu.Unlock()
	switch {
	case wasUp && !up:
		i.Log.Info("Plugin went offline", "plugin", i.Key)
	case !wasUp && up:
		i.Log.Info("Plugin came online", "plugin", i.Key)
	}
	for _, e := range events {
		e()
	}
}

func (i *Instance) handleDevices(topic string, payload []byte) {
	var msg contractv1.Devices
	if err := json.Unmarshal(payload, &msg); err != nil {
		i.mu.Lock()
		i.rejectLocked(ReasonMalformed, "", topic)
		i.mu.Unlock()
		return
	}
	i.counting.Lock()
	defer i.counting.Unlock()
	i.mu.Lock()
	i.announced = map[string]driver.Info{}
	for _, d := range msg.Devices {
		if _, seen := i.announced[d.ID]; seen || !contractv1.ValidID(d.ID) {
			// The first entry for an ID wins. Logged once per plugin, not per id: an invalid id may
			// be anything, and new each time.
			i.rejectLocked(ReasonMalformed, "", topic)
			continue
		}
		info := driver.Info{Found: d.Found, Manufacturer: d.Manufacturer, Model: d.Model, Error: d.Error}
		if d.Found {
			for _, c := range d.Capabilities {
				if _, ok := capability.Lookup(c); !ok {
					i.rejectLocked(ReasonUnknownCapability, d.ID, topic)
					continue
				}
				if !slices.Contains(info.Capabilities, c) {
					info.Capabilities = append(info.Capabilities, c)
				}
			}
			info.DeviceType = i.deviceTypeLocked(d.ID, d.DeviceType, topic)
		}
		i.announced[d.ID] = info
	}
	i.undeclared = max(msg.Undeclared, 0)
	i.needsSetup = msg.NeedsSetup
	i.discovered = i.validDiscoveredLocked(msg.Discovered, topic)
	undeclared := i.undeclaredLocked()
	inventory := i.inventoryLocked()
	// Values of capabilities a device no longer announces must not linger.
	for id, vals := range i.values {
		maps.DeleteFunc(vals, func(c string, _ any) bool {
			return !slices.Contains(i.announced[id].Capabilities, c)
		})
	}
	emit := i.discoveryLocked()
	i.mu.Unlock()
	emit()
	i.sendUndeclared(undeclared)
	i.Sink.Inventory(i.key(), inventory)
}

func (i *Instance) handleState(id, topic string, payload []byte) {
	now := i.Now()
	i.mu.Lock()
	var state contractv1.State
	if err := json.Unmarshal(payload, &state); err != nil {
		i.rejectLocked(ReasonMalformed, id, topic)
		i.mu.Unlock()
		return
	}
	l := i.lastLocked(id)
	l.state, l.stateAt, l.hasState = payload, now, true
	var emit func()
	if _, ok := i.declared[id]; ok {
		emit = i.applyStateLocked(id, state, now, topic)
	} else {
		i.rejectUndeclaredLocked(id, topic)
	}
	i.mu.Unlock()
	if emit != nil {
		emit()
	}
}

func (i *Instance) handleAvailability(id, topic string, payload []byte) {
	state := string(payload)
	i.mu.Lock()
	if state != contractv1.Online && state != contractv1.Offline {
		i.rejectLocked(ReasonMalformed, id, topic)
		i.mu.Unlock()
		return
	}
	l := i.lastLocked(id)
	l.online, l.hasOnline = state == contractv1.Online, true
	dev, declared := i.declared[id]
	if !declared {
		i.rejectUndeclaredLocked(id, topic)
	}
	// A device can't be online while its plugin is known to be down.
	online := l.online && (i.status == nil || i.upLocked())
	i.mu.Unlock()
	if declared {
		i.Sink.Availability(dev, online)
	}
}

// applyStateLocked diffs a state object against the last values and returns the events to send
// after unlocking. topic is empty for replays, which neither count rejections again nor log.
func (i *Instance) applyStateLocked(id string, state contractv1.State, at time.Time, topic string) func() {
	dev := i.declared[id]
	info, announced := i.announced[id]
	if !announced {
		// Wait for the devices topic; the state is replayed when it arrives.
		return nil
	}
	var updates []driver.Update
	for _, name := range slices.Sorted(maps.Keys(state)) {
		if _, ok := capability.Lookup(name); !ok {
			i.rejectLocked(ReasonUnknownCapability, id, topic)
			continue
		}
		if !slices.Contains(info.Capabilities, name) {
			i.rejectLocked(ReasonUnannouncedCapability, id, topic)
			continue
		}
		v, err := capability.Validate(name, state[name])
		if err != nil {
			i.rejectLocked(ReasonInvalidValue, id, topic)
			continue
		}
		if i.values == nil {
			i.values = map[string]map[string]any{}
		}
		if i.values[id] == nil {
			i.values[id] = map[string]any{}
		}
		prev, had := i.values[id][name]
		if had && prev == v {
			continue
		}
		i.values[id][name] = v
		updates = append(updates, driver.Update{Device: dev, Capability: name, Value: v, Previous: prev, Time: at})
	}
	return func() {
		i.Sink.Seen(dev, at)
		if len(updates) > 0 {
			i.Sink.Changed(updates...)
		}
	}
}

// replayLocked re-applies the last state and availability of every declared device.
func (i *Instance) replayLocked() func() {
	var events []func()
	for _, id := range slices.Sorted(maps.Keys(i.declared)) {
		l := i.last[id]
		if l == nil {
			continue
		}
		if l.hasOnline {
			dev, online := i.declared[id], l.online && (i.status == nil || i.upLocked())
			events = append(events, func() { i.Sink.Availability(dev, online) })
		}
		if l.hasState {
			var state contractv1.State
			if json.Unmarshal(l.state, &state) == nil {
				if e := i.applyStateLocked(id, state, l.stateAt, ""); e != nil {
					events = append(events, e)
				}
			}
		}
	}
	return func() {
		for _, e := range events {
			e()
		}
	}
}

// discoveryLocked computes discovery events, followed by replays; call the returned func after unlocking.
// Until the devices topic is seen there is nothing to report, so devices stay waiting.
func (i *Instance) discoveryLocked() func() {
	type found struct {
		dev  ident.ID
		info driver.Info
	}
	var events []found
	if i.announced != nil {
		for id, dev := range i.declared {
			events = append(events, found{dev, i.announced[id]})
		}
	}
	replay := i.replayLocked()
	return func() {
		for _, e := range events {
			i.Sink.Discovered(e.dev, e.info)
		}
		replay()
	}
}

// validDiscoveredLocked drops discovered devices with an invalid or repeated ID, and capabilities
// not in the registry, counting each as rejected. It keeps nil apart from empty.
func (i *Instance) validDiscoveredLocked(discovered []contractv1.DiscoveredDevice, topic string) []contractv1.DiscoveredDevice {
	if discovered == nil {
		return nil
	}
	valid := make([]contractv1.DiscoveredDevice, 0, len(discovered))
	seen := map[string]bool{}
	for _, d := range discovered {
		if !contractv1.ValidID(d.ID) || seen[d.ID] {
			// Logged once per plugin, not per id: an invalid id may be anything, and new each time.
			i.rejectLocked(ReasonMalformed, "", topic)
			continue
		}
		seen[d.ID] = true
		var caps []string
		for _, c := range d.Capabilities {
			if _, ok := capability.Lookup(c); !ok {
				i.rejectLocked(ReasonUnknownCapability, d.ID, topic)
				continue
			}
			if !slices.Contains(caps, c) {
				caps = append(caps, c)
			}
		}
		d.Capabilities = caps
		d.DeviceType = string(i.deviceTypeLocked(d.ID, d.DeviceType, topic))
		valid = append(valid, d)
	}
	return valid
}

// deviceTypeLocked is a device type the contract allows, or empty, counting any other as rejected.
// The device itself stays: only what it is falls back to what its capabilities say.
func (i *Instance) deviceTypeLocked(id, t, topic string) devicetype.Type {
	if !contractv1.ValidDeviceType(t) {
		i.rejectLocked(ReasonUnknownDeviceType, id, topic)
		return ""
	}
	return devicetype.Type(t)
}

// undeclaredLocked counts the discovered devices neither declared nor removed, or returns the plugin's own
// count if it sends no discovered list.
func (i *Instance) undeclaredLocked() int {
	if i.discovered == nil {
		return i.undeclared
	}
	n := 0
	for _, d := range i.discovered {
		if _, ok := i.declared[d.ID]; !ok && !i.removed[d.ID] {
			n++
		}
	}
	return n
}

func (i *Instance) allAvailabilityLocked(online bool) func() {
	devs := slices.Collect(maps.Values(i.declared))
	return func() {
		for _, d := range devs {
			i.Sink.Availability(d, online)
		}
	}
}

func (i *Instance) upLocked() bool {
	return i.status != nil && i.status.State == contractv1.Online && i.status.Contract == contractv1.Contract
}

// maxUnseen bounds what's kept per ID that no Device has, since anything on the broker can publish
// under the plugin's root with an ID it makes up: the latest state and availability of that many,
// and the rejections logged for that many reason and ID pairs.
const maxUnseen = 1024

// lastLocked is where id's latest state and availability go. Past maxUnseen IDs, an undeclared
// one's are kept nowhere.
func (i *Instance) lastLocked(id string) *lastSeen {
	if i.last == nil {
		i.last = map[string]*lastSeen{}
	}
	if i.last[id] == nil {
		if _, declared := i.declared[id]; !declared && len(i.last) >= maxUnseen+len(i.declared) {
			return &lastSeen{}
		}
		i.last[id] = &lastSeen{}
	}
	return i.last[id]
}

// rejectUndeclaredLocked rejects a message for an id no Device has, unless it's a removed device's, or
// no devices have been declared yet.
func (i *Instance) rejectUndeclaredLocked(id, topic string) {
	if i.declared != nil && !i.removed[id] {
		i.rejectLocked(ReasonUndeclaredDevice, id, topic)
	}
}

// rejectLocked counts a dropped message and logs the first one per reason and id, or past
// maxUnseen pairs, per reason. An empty topic marks a replay, which was counted when it first
// arrived.
func (i *Instance) rejectLocked(reason, id, topic string) {
	if topic == "" {
		return
	}
	if i.rejected == nil {
		i.rejected, i.logged = map[string]uint64{}, map[string]bool{}
	}
	i.rejected[reason]++
	k := reason + "/" + id
	if !i.logged[k] && len(i.logged) >= maxUnseen {
		k = reason + "/"
	}
	if !i.logged[k] {
		i.logged[k] = true
		i.Log.Info("Rejected plugin message", "plugin", i.Key, "reason", reason, "topic", topic)
	}
}
