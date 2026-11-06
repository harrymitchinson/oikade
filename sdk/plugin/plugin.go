package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// ErrClosed is returned by a Plugin's methods once Close has started.
var ErrClosed = errors.New("plugin is closed")

// Options configures Start.
type Options struct {
	Environment Environment
	// OnSet applies a command, then reports the result with SetState or UpdateState. Calls for one
	// device never overlap, and their ctx ends when the plugin closes.
	OnSet func(ctx context.Context, p *Plugin, cmd contractv1.SetCommand) error
	// Logger defaults to JSON on stderr.
	Logger *slog.Logger
	// StartTimeout is how long Start waits to come online, so a wrong broker URL crashes the pod
	// instead of leaving it Running and silent. It defaults to 60s.
	StartTimeout time.Duration
	// CloseTimeout is how long Close waits to publish the offline status. It defaults to 2s.
	CloseTimeout time.Duration
}

// Plugin is a running plugin. It keeps the latest devices, availability and state, and
// republishes them, retained, whenever it reconnects, because a Last Will or a broker restart may
// have replaced them.
type Plugin struct {
	root         contractv1.Root
	clientID     string
	log          *slog.Logger
	onSet        func(ctx context.Context, p *Plugin, cmd contractv1.SetCommand) error
	closeTimeout time.Duration

	client link
	// ready is closed once client is set and subscribed, so announcing can start.
	ready chan struct{}
	// online is closed after the first announcement.
	online     chan struct{}
	onlineOnce sync.Once
	// life ends when the plugin closes. The connection and OnSet calls run under it.
	life context.Context
	stop context.CancelFunc

	// pub is held while publishing, together with the change that is published, so the last
	// retained message on every topic is the stored value. It also orders Close's offline after
	// any online.
	pub chan struct{}
	// closed is set when Close starts. Nothing but offline is published after that.
	closed atomic.Bool

	mu sync.Mutex
	// devices is nil until SetDevices.
	devices    []contractv1.DeviceReport
	undeclared int
	// discovered is nil until SetDiscovered.
	discovered   []contractv1.DiscoveredDevice
	states       map[string]contractv1.State
	availability map[string]bool
	queues       map[string][]contractv1.SetCommand
	// running says when each device's Set began, and setTimeout how long one may run before a
	// plugin under the host counts as hung. Zero never does.
	running    map[string]time.Time
	setTimeout time.Duration
}

// link is how a plugin reaches Oikade: a broker, or the host's socket.
type link interface {
	Publish(ctx context.Context, topic string, payload []byte, retain bool) error
	Disconnect(ctx context.Context) error
	Drop(ctx context.Context) error
}

// Start connects with the Last Will set and returns once the plugin is online. ctx bounds only
// starting; the plugin runs until Close.
func Start(ctx context.Context, o Options) (*Plugin, error) {
	if o.OnSet == nil {
		return nil, errors.New("options have no OnSet")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = 60 * time.Second
	}
	if o.CloseTimeout <= 0 {
		o.CloseTimeout = 2 * time.Second
	}
	root, err := contractv1.ParseRoot(string(o.Environment.Root))
	if err != nil {
		return nil, err
	}
	server, err := url.Parse(o.Environment.MQTTURL)
	if err != nil {
		return nil, fmt.Errorf("parsing MQTT URL: %w", err)
	}
	// Logged by internal/mqtt, which takes a logr.Logger from the context.
	life, stop := context.WithCancel(logr.NewContext(context.WithoutCancel(ctx), logr.FromSlogHandler(o.Logger.Handler())))
	p := newPlugin(root, o.Logger, o.OnSet, o.CloseTimeout, life, stop)

	startCtx, cancel := context.WithTimeout(ctx, o.StartTimeout)
	defer cancel()
	// Connect waits on life, which outlives startCtx, so a startup that runs out of time ends it.
	keep := context.AfterFunc(startCtx, stop)
	fail := func(err error) (*Plugin, error) {
		p.closed.Store(true)
		if p.client != nil {
			// The plugin may have announced itself online; the Will replaces that.
			dropCtx, cancel := context.WithTimeout(context.Background(), o.CloseTimeout)
			defer cancel()
			_ = p.client.Drop(dropCtx)
		}
		stop()
		return nil, err
	}
	timedOut := func() (*Plugin, error) {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		return fail(fmt.Errorf("could not come online at %s within %s", server.Redacted(), o.StartTimeout))
	}

	will, _ := json.Marshal(contractv1.Status{State: contractv1.Offline, Contract: contractv1.Contract})
	client, err := mqtt.Connect(life, server.String(), p.clientID,
		mqtt.WithWill(root.Status(), will), mqtt.WithOnConnect(p.announce))
	if err != nil {
		if startCtx.Err() != nil {
			return timedOut()
		}
		return fail(err)
	}
	p.client = client
	if _, err := client.Subscribe(startCtx, root.SetFilter(), p.receive); err != nil {
		if startCtx.Err() != nil {
			return timedOut()
		}
		return fail(fmt.Errorf("subscribing to %s: %w", root.SetFilter(), err))
	}
	close(p.ready)
	select {
	case <-p.online:
	case <-startCtx.Done():
		return timedOut()
	}
	if !keep() {
		return timedOut() // the timeout fired as the plugin came online
	}
	return p, nil
}

func newPlugin(root contractv1.Root, log *slog.Logger, onSet func(context.Context, *Plugin, contractv1.SetCommand) error,
	closeTimeout time.Duration, life context.Context, stop context.CancelFunc) *Plugin {
	return &Plugin{
		root:         root,
		clientID:     newClientID(),
		log:          log,
		onSet:        onSet,
		closeTimeout: closeTimeout,
		ready:        make(chan struct{}),
		online:       make(chan struct{}),
		life:         life,
		stop:         stop,
		pub:          make(chan struct{}, 1),
		states:       map[string]contractv1.State{},
		availability: map[string]bool{},
		queues:       map[string][]contractv1.SetCommand{},
		running:      map[string]time.Time{},
	}
}

func newClientID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	// Random: a takeover by a fixed id would fire the old session's Last Will after the new
	// session announced itself online.
	return "oikade-plugin-" + hex.EncodeToString(b)
}

// SetDevices publishes the devices this plugin serves. List every device from devices.json.
// Values a device no longer announces are dropped from its state, and that state is republished.
func (p *Plugin) SetDevices(ctx context.Context, devices []contractv1.DeviceReport, undeclared int) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()

	p.mu.Lock()
	p.devices = make([]contractv1.DeviceReport, len(devices))
	for i, d := range devices {
		d.Capabilities = slices.Clone(d.Capabilities)
		p.devices[i] = d
	}
	p.undeclared = undeclared
	// Availability is only republished for devices still reported.
	maps.DeleteFunc(p.availability, func(id string, _ bool) bool { return !p.reportedLocked(id) })
	// A hub that restarts would read and reject the values that are no longer announced.
	var changed []string
	for id, s := range p.states {
		announced := p.capabilitiesLocked(id)
		n := len(s)
		maps.DeleteFunc(s, func(c string, _ any) bool { return !slices.Contains(announced, c) })
		if len(s) != n {
			changed = append(changed, id)
		}
	}
	slices.Sort(changed)
	p.mu.Unlock()

	if err := p.publishDevices(ctx); err != nil {
		return err
	}
	for _, id := range changed {
		if err := p.publishState(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// SetDiscovered publishes every device the plugin sees in its app, declared or not, so Oikade
// can adopt the undeclared ones. Once it's called, Oikade counts undeclared devices from this list
// and ignores SetDevices' count. A device leaving the list means it was removed from its app, so
// keep offline devices in it. Each ID must stay the same across restarts of the plugin and of its
// hub or account, until the device is reset or re-paired: derive it from something that does,
// such as a MAC address or serial number, if the app's own ID doesn't.
//
// Pass nil to stop reporting the list, if the plugin can no longer see its app: an empty list
// means the app has no devices. It refuses an invalid or repeated ID and a capability Oikade
// doesn't know, and publishes nothing. The list is published with the devices from SetDevices, and waits for the first
// SetDevices if that hasn't been called yet.
func (p *Plugin) SetDiscovered(ctx context.Context, discovered []contractv1.DiscoveredDevice) error {
	var next []contractv1.DiscoveredDevice
	if discovered != nil {
		next = make([]contractv1.DiscoveredDevice, len(discovered))
	}
	seen := map[string]bool{}
	for i, d := range discovered {
		if !contractv1.ValidID(d.ID) {
			return fmt.Errorf("discovered device id %q is not valid", d.ID)
		}
		if seen[d.ID] {
			return fmt.Errorf("discovered device %s is listed twice", d.ID)
		}
		seen[d.ID] = true
		for _, c := range d.Capabilities {
			if _, ok := capability.Lookup(c); !ok {
				return fmt.Errorf("discovered device %s: unknown capability %s", d.ID, c)
			}
		}
		d.Capabilities = slices.Clone(d.Capabilities)
		next[i] = d
	}

	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	p.mu.Lock()
	p.discovered = next
	devicesSet := p.devices != nil
	p.mu.Unlock()
	if !devicesSet {
		return nil
	}
	return p.publishDevices(ctx)
}

// SetState publishes a device's full current values. The device must be found, and the state
// must hold a valid value for every capability announced in SetDevices and nothing else.
// Otherwise SetState returns an error and publishes nothing.
func (p *Plugin) SetState(ctx context.Context, id string, s contractv1.State) error {
	return p.writeState(ctx, id, s, false)
}

// UpdateState merges values into a device's state and publishes the full result, which must be
// complete as for SetState.
func (p *Plugin) UpdateState(ctx context.Context, id string, s contractv1.State) error {
	return p.writeState(ctx, id, s, true)
}

func (p *Plugin) writeState(ctx context.Context, id string, s contractv1.State, merge bool) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()

	p.mu.Lock()
	if !p.foundLocked(id) {
		p.mu.Unlock()
		return fmt.Errorf("device %s is not reported found in SetDevices", id)
	}
	announced := p.capabilitiesLocked(id)
	next := contractv1.State{}
	if merge {
		maps.Copy(next, p.states[id])
	}
	for c, v := range s {
		if !slices.Contains(announced, c) {
			p.mu.Unlock()
			return fmt.Errorf("device %s has not announced capability %s; add it in SetDevices first", id, c)
		}
		valid, err := capability.Validate(c, v)
		if err != nil {
			p.mu.Unlock()
			return fmt.Errorf("device %s: %w", id, err)
		}
		next[c] = valid
	}
	for _, c := range announced {
		if _, ok := next[c]; !ok {
			p.mu.Unlock()
			return fmt.Errorf("device %s: state has no value for announced capability %s", id, c)
		}
	}
	p.states[id] = next
	p.mu.Unlock()
	return p.publishState(ctx, id)
}

// SetAvailability publishes whether a device is reachable. The device must be reported in
// SetDevices, found or not.
func (p *Plugin) SetAvailability(ctx context.Context, id string, online bool) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	p.mu.Lock()
	if !p.reportedLocked(id) {
		p.mu.Unlock()
		return fmt.Errorf("device %s is not reported in SetDevices", id)
	}
	p.availability[id] = online
	p.mu.Unlock()
	return p.publishAvailability(ctx, id)
}

// State returns a copy of the last state set for a device, or nil.
func (p *Plugin) State(id string) contractv1.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.states[id])
}

// Close marks the plugin offline and disconnects. A clean disconnect doesn't trigger the Last
// Will, so the offline status is published first. If that takes longer than CloseTimeout, the
// connection is dropped instead, which leaves the Last Will to the broker. Calls after the first
// return nil.
func (p *Plugin) Close(ctx context.Context) error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	defer p.stop()
	ctx, cancel := context.WithTimeout(ctx, p.closeTimeout)
	defer cancel()
	offline, _ := json.Marshal(contractv1.Status{State: contractv1.Offline, Contract: contractv1.Contract})
	err := p.acquire(ctx)
	if err == nil {
		err = p.client.Publish(ctx, p.root.Status(), offline, true)
		p.unlock()
	}
	// Disconnecting gets its own time: ctx may have run out.
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), p.closeTimeout)
	defer dcancel()
	if err != nil {
		_ = p.client.Drop(dctx)
		return fmt.Errorf("could not publish the offline status, so left it to the Last Will: %w", err)
	}
	return p.client.Disconnect(dctx)
}

// announce runs on every connection: status online, then devices once set, then every
// availability and state.
func (p *Plugin) announce(ctx context.Context) {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return
	}
	if err := p.lock(ctx); err != nil {
		return
	}
	defer p.unlock()
	online, _ := json.Marshal(contractv1.Status{State: contractv1.Online, Contract: contractv1.Contract})
	if err := p.client.Publish(ctx, p.root.Status(), online, true); err != nil {
		p.log.Error("Could not announce plugin", "topic", string(p.root), "error", err.Error())
		return
	}

	p.mu.Lock()
	// Until the plugin reports its devices, whatever is retained stays: an empty list would mark
	// every Device not found while the plugin starts.
	devicesSet := p.devices != nil
	available := slices.Sorted(maps.Keys(p.availability))
	stated := slices.Sorted(maps.Keys(p.states))
	p.mu.Unlock()

	err := func() error {
		if devicesSet {
			if err := p.publishDevices(ctx); err != nil {
				return err
			}
		}
		for _, id := range available {
			if err := p.publishAvailability(ctx, id); err != nil {
				return err
			}
		}
		for _, id := range stated {
			if err := p.publishState(ctx, id); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		p.log.Error("Could not announce plugin", "topic", string(p.root), "error", err.Error())
		return
	}
	p.log.Info("Plugin came online", "topic", string(p.root))
	p.onlineOnce.Do(func() { close(p.online) })
}

// receive runs on the MQTT receive goroutine, so it only queues the command.
func (p *Plugin) receive(m mqtt.Message) {
	cmd, ok, err := p.root.ParseSet(m.Topic, m.Payload)
	if !ok {
		return
	}
	ignore := func(reason string) { p.log.Warn("Ignored set command", "topic", m.Topic, "reason", reason) }
	if m.Retained {
		// Nothing is retained on /set, so this is stale and would replay on every reconnect.
		ignore("retained")
		return
	}
	if err != nil {
		ignore(err.Error())
		return
	}
	value, err := capability.Validate(cmd.Capability, cmd.Value)
	if err != nil {
		ignore(err.Error())
		return
	}
	cmd.Value = value

	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Contains(p.capabilitiesLocked(cmd.ID), cmd.Capability) {
		ignore(fmt.Sprintf("device %s has not announced %s", cmd.ID, cmd.Capability))
		return
	}
	if p.closed.Load() {
		return
	}
	queue, running := p.queues[cmd.ID]
	p.queues[cmd.ID] = append(queue, cmd)
	if !running {
		go p.drain(cmd.ID)
	}
}

// drain applies a device's queued commands in order, then exits. Commands left when the plugin
// closes are dropped.
func (p *Plugin) drain(id string) {
	for {
		p.mu.Lock()
		queue := p.queues[id]
		if len(queue) == 0 || p.closed.Load() {
			delete(p.queues, id)
			p.mu.Unlock()
			return
		}
		cmd := queue[0]
		p.queues[id] = queue[1:]
		p.running[id] = time.Now()
		p.mu.Unlock()

		err := p.onSet(p.life, p, cmd)
		p.mu.Lock()
		delete(p.running, id)
		p.mu.Unlock()
		if err != nil {
			p.log.Error("Could not apply set command",
				"topic", p.root.Set(cmd.ID), "capability", cmd.Capability, "error", err.Error())
		}
	}
}

// stuck is whether a Set has run for longer than it may.
func (p *Plugin) stuck() bool {
	if p.setTimeout <= 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, began := range p.running {
		if time.Since(began) > p.setTimeout {
			return true
		}
	}
	return false
}

// lock takes pub for a publish. It fails once Close has started, or when ctx ends first.
func (p *Plugin) lock(ctx context.Context) error {
	if err := p.acquire(ctx); err != nil {
		return err
	}
	// Checked after acquiring: Close may have started while this waited.
	if p.closed.Load() {
		p.unlock()
		return ErrClosed
	}
	return nil
}

func (p *Plugin) acquire(ctx context.Context) error {
	select {
	case p.pub <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Plugin) unlock() { <-p.pub }

func (p *Plugin) publishDevices(ctx context.Context) error {
	p.mu.Lock()
	payload, err := json.Marshal(contractv1.Devices{
		Devices: p.devices, Undeclared: p.undeclared, Discovered: p.discovered,
	})
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return p.client.Publish(ctx, p.root.Devices(), payload, true)
}

func (p *Plugin) publishState(ctx context.Context, id string) error {
	p.mu.Lock()
	s := p.states[id]
	if s == nil {
		s = contractv1.State{}
	}
	payload, err := json.Marshal(s)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return p.client.Publish(ctx, p.root.State(id), payload, true)
}

func (p *Plugin) publishAvailability(ctx context.Context, id string) error {
	p.mu.Lock()
	value := contractv1.Offline
	if p.availability[id] {
		value = contractv1.Online
	}
	p.mu.Unlock()
	return p.client.Publish(ctx, p.root.Availability(id), []byte(value), true)
}

// capabilitiesLocked returns what a device announced, or nothing if it isn't found.
func (p *Plugin) capabilitiesLocked(id string) []string {
	for _, d := range p.devices {
		if d.ID == id && d.Found {
			return d.Capabilities
		}
	}
	return nil
}

func (p *Plugin) reportedLocked(id string) bool {
	return slices.ContainsFunc(p.devices, func(d contractv1.DeviceReport) bool { return d.ID == id })
}

func (p *Plugin) foundLocked(id string) bool {
	return slices.ContainsFunc(p.devices, func(d contractv1.DeviceReport) bool { return d.ID == id && d.Found })
}
