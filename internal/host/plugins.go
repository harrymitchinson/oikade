package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/driver"
	pluginDriver "github.com/harrymitchinson/oikade/internal/driver/plugin"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// DefaultPluginSetTimeout is how long a plugin's Set may run before the plugin counts as hung.
const DefaultPluginSetTimeout = 30 * time.Second

// errPluginDown is a write to a plugin with no process to take it.
var errPluginDown = errors.New("its plugin isn't running")

// plugins runs your binary's plugins, one process each, and follows them and any outside ones. A
// plugin's messages are the oikade/v1 contract's on either path, so one decoder checks them all.
type plugins struct {
	host    *Host
	log     logr.Logger
	socket  string
	devices *hub.Store
	known   *inventory
	// wanted is the binary to run them from, latest first.
	wanted chan string
	// refresh asks that the plugins hear of a change to the registry.
	refresh chan struct{}

	mu      sync.Mutex
	running map[string]*hosted
}

// hosted is a plugin: what the host knows of it, which outlives its processes.
type hosted struct {
	name   string
	key    driver.InstanceKey
	root   contractv1.Root
	driver *pluginDriver.Instance
	// socket is nil for an outside plugin.
	socket *socketBus
}

func newPlugins(h *Host, socket string, devices *hub.Store, known *inventory) *plugins {
	p := &plugins{
		host: h, log: h.Log.WithName("plugins"), socket: socket, devices: devices, known: known,
		wanted: make(chan string, 1), refresh: make(chan struct{}, 1), running: map[string]*hosted{},
	}
	known.changed = append(known.changed, func() {
		select {
		case p.refresh <- struct{}{}:
		default:
		}
	})
	return p
}

// recordedPrefix is how the registry records this host's plugins.
var recordedPrefix = instanceName(pluginKey("")) // ends in the empty name

func pluginKey(name string) driver.InstanceKey {
	return driver.InstanceKey{Driver: driver.Plugin, NamespacedName: key(name)}
}

// attachRecorded brings in the plugins on record, so that their devices are known, and their claims
// enforced, before any process has connected.
func (p *plugins) attachRecorded() {
	for _, d := range p.known.registry.List().Devices {
		if name, ok := strings.CutPrefix(d.Driver, recordedPrefix); ok && !d.Removed {
			p.get(name)
		}
	}
}

// get returns a plugin, adding it if it's new.
func (p *plugins) get(name string) *hosted {
	p.mu.Lock()
	h, ok := p.running[name]
	p.mu.Unlock()
	if ok {
		return h
	}
	h = &hosted{
		name: name, key: pluginKey(name), root: contractv1.NewRoot(wire.Namespace, name),
		socket: &socketBus{log: p.log.WithValues("plugin", name)},
	}
	p.follow(context.Background(), h, h.socket)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running[name] = h
	return h
}

// follow starts decoding a plugin from bus.
func (p *plugins) follow(ctx context.Context, h *hosted, bus mqtt.Bus) {
	h.driver = &pluginDriver.Instance{
		Key: h.key.NamespacedName, Root: h.root, Bus: pairingBus{Bus: bus, devices: h.root.Devices(), pair: func() { p.pair(h) }},
		Sink: p.devices, Now: time.Now, Log: p.log,
	}
	p.known.attach(h.key)
	h.driver.SetDevices(p.onRecord(h))
	if err := h.driver.Start(ctx); err != nil {
		p.log.Error(err, "Failed to follow plugin", "plugin", h.name)
	}
}

// pairingBus tells the host of a plugin's devices report once the decoder has taken it in. Its
// Sink can't, because the decoder holds its locks through that.
type pairingBus struct {
	mqtt.Bus
	devices string
	pair    func()
}

func (b pairingBus) Subscribe(ctx context.Context, filter string, h mqtt.Handler) (func(), error) {
	return b.Bus.Subscribe(ctx, filter, func(m mqtt.Message) {
		h(m)
		if m.Topic == b.devices {
			b.pair()
		}
	})
}

// onRecord maps a plugin's device IDs to the devices they are.
func (p *plugins) onRecord(h *hosted) map[string]types.NamespacedName {
	devices := map[string]types.NamespacedName{}
	driverName := instanceName(h.key)
	for _, d := range p.known.registry.List().Devices {
		if d.Driver == driverName && !d.Removed && d.ID != "" {
			devices[d.ID] = key(d.Name)
		}
	}
	return devices
}

// pair records everything a plugin reports, so a paired device is usable at once.
func (p *plugins) pair(h *hosted) {
	apps := map[string]driver.AppDevice{}
	for id, info := range h.driver.Announced() {
		if info.Found {
			apps[id] = driver.AppDevice{
				ID: id, Name: id, Manufacturer: info.Manufacturer, Model: info.Model, Capabilities: info.Capabilities,
			}
		}
	}
	listed, _ := h.driver.Discovered()
	for _, d := range listed {
		if known, ok := apps[d.ID]; ok {
			d.Capabilities = known.Capabilities
		}
		apps[d.ID] = driver.AppDevice{
			ID: d.ID, Name: d.Name, Room: d.Room, Manufacturer: d.Manufacturer, Model: d.Model, Capabilities: d.Capabilities,
		}
	}
	for _, id := range slices.Sorted(maps.Keys(apps)) {
		if _, err := p.known.paired(h.key, apps[id], p.control(h.name, id)); err != nil {
			p.log.Error(err, "Failed to record device", "plugin", h.name, "id", id)
		}
	}
	h.driver.SetDevices(p.onRecord(h))
}

// control finds a plugin device's arbitration settings.
func (p *plugins) control(plugin, id string) []homev1alpha1.CapabilityControl {
	for _, d := range p.host.Devices {
		if d.Plugin == plugin && d.Name == id {
			return d.Control
		}
	}
	return nil
}

// use asks that the plugins run from this binary. It doesn't wait, so the plugins can't hold up your
// automations. The binary changing is the one thing that restarts a plugin that's working.
func (p *plugins) use(path string) {
	select {
	case <-p.wanted:
	default:
	}
	p.wanted <- path
}

// run starts the outside plugins, then keeps the imported ones running until ctx ends.
func (p *plugins) run(ctx context.Context) {
	go p.startOutside(ctx)
	var (
		current string
		stop    = func() {}
	)
	defer func() { stop() }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.refresh:
			p.mu.Lock()
			all := slices.Collect(maps.Values(p.running))
			p.mu.Unlock()
			for _, h := range all {
				h.driver.SetDevices(p.onRecord(h))
			}
		case path := <-p.wanted:
			if path == current {
				continue
			}
			stop()
			stop, current = func() {}, path
			manifest, err := runCheck(ctx, path)
			if err != nil {
				p.log.Info("Couldn't find this binary's plugins", "reason", err.Error())
				continue
			}
			generation, cancel := context.WithCancel(ctx)
			var running sync.WaitGroup
			for _, name := range manifest.Plugins {
				h := p.get(name)
				if h.socket == nil {
					p.log.Info("Ignored imported plugin; an outside plugin has its name", "plugin", name)
					continue
				}
				running.Go(func() { p.supervise(generation, h, path) })
			}
			stop = func() {
				cancel()
				running.Wait()
			}
		}
	}
}

// startOutside follows the plugins that run as processes of their own.
func (p *plugins) startOutside(ctx context.Context) {
	if len(p.host.Outside) == 0 {
		return
	}
	bus, err := mqtt.Connect(ctx, p.host.Broker, "oikade-host")
	if err != nil {
		p.log.Error(err, "Failed to connect to the broker", "broker", p.host.Broker)
		return
	}
	for _, name := range p.host.Outside {
		h := &hosted{name: name, key: pluginKey(name), root: contractv1.NewRoot(wire.Namespace, name)}
		p.follow(ctx, h, bus)
		p.mu.Lock()
		p.running[name] = h
		p.mu.Unlock()
	}
}

// supervise keeps a plugin's process running until ctx ends. A plugin's failures are its own.
func (p *plugins) supervise(ctx context.Context, h *hosted, path string) {
	log := p.log.WithValues("plugin", h.name)
	backoff := firstBackoff
	for ctx.Err() == nil {
		began := time.Now()
		err := p.runProcess(ctx, h, path, log)
		h.socket.end()
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) >= stableAfter {
			backoff = firstBackoff
		}
		log.Info("Restarting plugin process", "reason", reason(err), "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (p *plugins) runProcess(ctx context.Context, h *hosted, path string, log logr.Logger) error {
	cmd := exec.Command(path, wire.RolePlugin, h.name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), wire.SocketEnv+"="+p.socket)
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Info("Started plugin process", "pid", cmd.Process.Pid)
	_, err := p.host.watch(ctx, cmd, log, "plugin", h.socket.ping, nil)
	return err
}

// serve is a plugin's session, from its hello until it goes.
func (p *plugins) serve(conn *wire.Conn, hello wire.Message) {
	p.mu.Lock()
	h, ok := p.running[hello.Plugin]
	p.mu.Unlock()
	if !ok || h.socket == nil {
		p.log.Info("Refused plugin", "plugin", hello.Plugin)
		_ = conn.Write(wire.Message{Type: wire.TypeRejected, Error: fmt.Sprintf("no plugin %q here", hello.Plugin)})
		return
	}
	sess := &pluginSession{conn: conn, pong: time.Now(), finished: make(chan struct{})}
	defer close(sess.finished)
	if !h.socket.attach(sess) {
		p.log.Info("Refused plugin; one is already connected", "plugin", hello.Plugin)
		_ = conn.Write(wire.Message{Type: wire.TypeRejected, Error: "this plugin is already connected"})
		return
	}
	defer h.socket.detach(sess, h.root)
	err := conn.Write(wire.Message{
		Type: wire.TypeWelcome, Version: wire.Version, Topic: string(h.root), Declared: p.declared(h),
		SetTimeout: p.host.PluginSetTimeout,
	})
	if err != nil {
		return
	}
	p.log.Info("Connected plugin", "plugin", h.name)
	for {
		m, err := conn.Read()
		if err != nil {
			p.log.Info("Disconnected plugin", "plugin", h.name, "reason", err.Error())
			return
		}
		switch m.Type {
		case wire.TypePong:
			h.socket.pong(sess)
		case wire.TypePublish:
			if strings.HasPrefix(m.Topic, string(h.root)+"/") {
				h.socket.deliver(mqtt.Message{Topic: m.Topic, Payload: m.Data, Retained: m.Retain})
			}
		}
	}
}

// declared is what a plugin's Factory gets.
func (p *plugins) declared(h *hosted) []contractv1.DeclaredDevice {
	driverName := instanceName(h.key)
	var out []contractv1.DeclaredDevice
	for _, d := range p.known.registry.List().Devices {
		if d.Driver == driverName && !d.Removed && d.ID != "" {
			out = append(out, contractv1.DeclaredDevice{ID: d.ID, Name: d.Name, DisplayName: d.Name, Room: d.Room})
		}
	}
	return out
}

// Set writes to a plugin's device.
func (p *plugins) Set(ctx context.Context, instance driver.InstanceKey, dev types.NamespacedName, name string, value any) error {
	p.mu.Lock()
	h := p.running[instance.Name]
	p.mu.Unlock()
	if h == nil || h.driver == nil {
		return fmt.Errorf("device %s: %w", dev.Name, errPluginDown)
	}
	return h.driver.Set(ctx, dev, name, value)
}

// pluginSession is a connected plugin process.
type pluginSession struct {
	conn     *wire.Conn
	pong     time.Time
	finished chan struct{}
}

// socketBus is a plugin's messages on the host's socket, as a Bus for the decoder. There's nothing
// to retain: a process announces everything when it connects, and the decoder keeps what it heard.
type socketBus struct {
	log logr.Logger

	mu      sync.Mutex
	handler mqtt.Handler
	session *pluginSession
}

// Publish implements mqtt.Bus. With no process there's nobody to take a command, which is an error
// the writer sees, not a message lost.
func (b *socketBus) Publish(_ context.Context, topic string, payload []byte, retain bool) error {
	b.mu.Lock()
	sess := b.session
	b.mu.Unlock()
	if sess == nil {
		return errPluginDown
	}
	return sess.conn.Write(wire.Message{Type: wire.TypePublish, Topic: topic, Data: payload, Retain: retain})
}

// Subscribe implements mqtt.Bus. The decoder is its one subscriber, to everything.
func (b *socketBus) Subscribe(_ context.Context, _ string, h mqtt.Handler) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handler = h
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.handler = nil
	}, nil
}

func (b *socketBus) deliver(m mqtt.Message) {
	b.mu.Lock()
	h := b.handler
	b.mu.Unlock()
	if h != nil {
		h(m)
	}
}

func (b *socketBus) attach(sess *pluginSession) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		return false
	}
	b.session = sess
	return true
}

// detach ends a session. Its process can't say so itself after a crash, so this does what a Last
// Will does.
func (b *socketBus) detach(sess *pluginSession, root contractv1.Root) {
	b.mu.Lock()
	b.session = nil
	b.mu.Unlock()
	offline, _ := json.Marshal(contractv1.Status{State: contractv1.Offline, Contract: contractv1.Contract})
	b.deliver(mqtt.Message{Topic: root.Status(), Payload: offline, Retained: true})
}

// end closes the session, if there is one, and returns once it has gone, so the next process starts
// after the last one's offline.
func (b *socketBus) end() {
	b.mu.Lock()
	sess := b.session
	b.mu.Unlock()
	if sess != nil {
		_ = sess.conn.Close()
		<-sess.finished
	}
}

func (b *socketBus) ping(id uint64) (time.Time, bool) {
	b.mu.Lock()
	sess := b.session
	var last time.Time
	if sess != nil {
		last = sess.pong
	}
	b.mu.Unlock()
	if sess == nil {
		return time.Time{}, false
	}
	if err := sess.conn.Write(wire.Message{Type: wire.TypePing, ID: id}); err != nil {
		b.log.V(1).Info("Failed to ping plugin", "error", err.Error())
	}
	return last, true
}

func (b *socketBus) pong(sess *pluginSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess.pong = time.Now()
}

// routedSetter sends a write to the driver its device belongs to.
type routedSetter struct {
	devices   *hub.Store
	simulated driver.Setter
	plugins   *plugins
}

// Set implements driver.Setter.
func (r routedSetter) Set(ctx context.Context, dev types.NamespacedName, name string, value any) error {
	if snap, ok := r.devices.Get(dev); ok && snap.Instance.Driver == driver.Plugin {
		return r.plugins.Set(ctx, snap.Instance, dev, name, value)
	}
	return r.simulated.Set(ctx, dev, name, value)
}
