package zigbee2mqtt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// DefaultBaseTopic is Zigbee2MQTT's own default mqtt.base_topic.
const DefaultBaseTopic = "zigbee2mqtt"

const (
	// disconnectTimeout bounds leaving the broker.
	disconnectTimeout = 2 * time.Second
	// connectTimeout bounds reaching the broker when the plugin starts. Going on waiting would leave
	// a plugin that looks well to the host, which restarts one that gives up, with a backoff.
	connectTimeout = time.Minute
)

// errBridgeOffline is a write while Zigbee2MQTT says it's offline, which nothing would act on.
var errBridgeOffline = errors.New("zigbee2mqtt is offline")

// Config configures the plugin.
type Config struct {
	// BaseTopic is Zigbee2MQTT's mqtt.base_topic. It defaults to DefaultBaseTopic.
	BaseTopic string
}

// New returns the plugin's Factory, for framework.Plugin. It fails if the host has no --broker.
func New(c Config) plugin.Factory {
	if c.BaseTopic == "" {
		c.BaseTopic = DefaultBaseTopic
	}
	return func(env plugin.Environment, declared []contractv1.DeclaredDevice) (plugin.Handler, error) {
		if env.MQTTURL == "" {
			return nil, errors.New("the zigbee2mqtt plugin needs the broker Zigbee2MQTT uses: run the host with --broker")
		}
		ids := make([]string, 0, len(declared))
		for _, d := range declared {
			ids = append(ids, d.ID)
		}
		return &bridge{
			config: c, broker: env.MQTTURL, declared: ids, up: true, online: map[string]bool{},
			connectTimeout: connectTimeout,
			log:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		}, nil
	}
}

// bridge is the plugin: it follows an Instance and reports what it hears through the SDK.
type bridge struct {
	config Config
	broker string
	// declared are the devices on record, which are reported even once they've left Zigbee2MQTT.
	declared       []string
	connectTimeout time.Duration
	log            *slog.Logger
	instance       *Instance

	// ctx and plugin are set before anything can be heard.
	ctx    context.Context
	plugin *plugin.Plugin

	mu sync.Mutex
	// up is whether Zigbee2MQTT can be reached, and online each device's last availability. A device
	// with none is taken as online, because Zigbee2MQTT reports availability only if it's turned on.
	up     bool
	online map[string]bool
	// reported is every device reported, and whether it's found.
	reported map[string]bool
	// heard is the values of the message being taken in.
	heard map[string]contractv1.State
}

var _ plugin.Handler = (*bridge)(nil)

// Serve implements plugin.Handler.
func (b *bridge) Serve(ctx context.Context, p *plugin.Plugin) error {
	b.ctx, b.plugin = ctx, p
	// The connection outlives ctx, so that it can be left cleanly: a connection cut when ctx ends
	// makes everything after it wait for a reply that isn't coming, and a deploy wait with it.
	life, cut := context.WithCancel(logging.NewContext(context.WithoutCancel(ctx), b.log))
	defer cut()
	dial, cancel := context.WithTimeout(ctx, b.connectTimeout)
	defer cancel()
	connecting := context.AfterFunc(dial, cut)
	client, err := mqtt.Connect(life, b.broker, clientID(), mqtt.WithOnDisconnect(b.connectionLost))
	if !connecting() {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("could not reach the broker within %s", b.connectTimeout)
	}
	if err != nil {
		return err
	}
	defer func() {
		leaving, done := context.WithTimeout(life, disconnectTimeout)
		defer done()
		_ = client.Disconnect(leaving)
	}()
	b.instance = &Instance{
		Key:           ident.ID{Name: b.config.BaseTopic},
		BaseTopic:     b.config.BaseTopic,
		Bus:           client,
		Sink:          b,
		Now:           time.Now,
		Log:           b.log,
		OnBridgeState: b.setUp,
		OnListed:      b.publishAllAvailability,
	}
	if err := b.instance.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// Set implements plugin.Handler. The result is reported when Zigbee2MQTT publishes it.
func (b *bridge) Set(ctx context.Context, _ *plugin.Plugin, cmd contractv1.SetCommand) error {
	b.mu.Lock()
	up := b.up
	b.mu.Unlock()
	if !up {
		return errBridgeOffline
	}
	return b.instance.Set(ctx, device(cmd.ID), cmd.Capability, cmd.Value)
}

// setUp follows <base>/bridge/state, which is Zigbee2MQTT's Last Will. With no process of its own to
// watch, this is how Oikade learns Zigbee2MQTT has stopped.
func (b *bridge) setUp(up bool) {
	b.mu.Lock()
	changed := up != b.up
	b.up = up
	b.mu.Unlock()
	if changed {
		b.log.Info("Zigbee2MQTT changed state", "online", up)
		b.publishAllAvailability()
	}
}

// connectionLost is the broker going, which hides whatever Zigbee2MQTT says: no message could say it
// has gone. A reconnect brings the retained state back. It doesn't wait, because the MQTT client
// waits for this.
func (b *bridge) connectionLost() {
	go b.setUp(false)
}

func (b *bridge) publishAllAvailability() {
	b.mu.Lock()
	ids := slices.Sorted(maps.Keys(b.reported))
	b.mu.Unlock()
	for _, id := range ids {
		b.publishAvailability(id)
	}
}

// publishAvailability reports a device as online only if it's paired, and it and Zigbee2MQTT are up.
func (b *bridge) publishAvailability(id string) {
	b.mu.Lock()
	online, known := b.online[id]
	online = b.up && b.reported[id] && (online || !known)
	b.mu.Unlock()
	b.check(b.plugin.SetAvailability(b.ctx, id, online), "Could not report availability", id)
}

func (b *bridge) check(err error, msg, id string) {
	if err != nil && !errors.Is(err, plugin.ErrClosed) && b.ctx.Err() == nil {
		b.log.Error(msg, "device", id, "error", err.Error())
	}
}

// Inventory implements driver.Sink: Zigbee2MQTT's list is the plugin's.
func (b *bridge) Inventory(_ driver.InstanceKey, devices []driver.AppDevice) {
	reports := make([]contractv1.DeviceReport, 0, len(devices)+len(b.declared))
	discovered := make([]contractv1.DiscoveredDevice, 0, len(devices))
	paired := make(map[string]ident.ID, len(devices))
	for _, d := range devices {
		paired[d.ID] = device(d.ID)
		reports = append(reports, contractv1.DeviceReport{
			ID: d.ID, Found: true, Manufacturer: d.Manufacturer, Model: d.Model, Capabilities: d.Capabilities,
		})
		discovered = append(discovered, contractv1.DiscoveredDevice{
			ID: d.ID, Manufacturer: d.Manufacturer, Model: d.Model, Capabilities: d.Capabilities, Name: d.Name,
		})
	}
	for _, id := range b.declared {
		if _, ok := paired[id]; !ok {
			reports = append(reports, contractv1.DeviceReport{ID: id})
		}
	}
	// Each of these publishes, so the host hears the list twice when it changes. It takes that in
	// the same way both times.
	b.check(b.plugin.SetDiscovered(b.ctx, discovered), "Could not report discovered devices", "")
	b.check(b.plugin.SetDevices(b.ctx, reports, 0), "Could not report devices", "")
	b.instance.SetDevices(paired)

	b.mu.Lock()
	b.reported = make(map[string]bool, len(reports))
	for _, r := range reports {
		b.reported[r.ID] = r.Found
	}
	b.mu.Unlock()
}

// Availability implements driver.Sink.
func (b *bridge) Availability(dev ident.ID, online bool) {
	b.mu.Lock()
	b.online[dev.Name] = online
	_, reported := b.reported[dev.Name]
	b.mu.Unlock()
	if reported {
		b.publishAvailability(dev.Name)
	}
}

// Changed implements driver.Sink. Seen ends the message, and reports it whole.
func (b *bridge) Changed(u driver.Update) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.heard == nil {
		b.heard = map[string]contractv1.State{}
	}
	if b.heard[u.Device.Name] == nil {
		b.heard[u.Device.Name] = contractv1.State{}
	}
	b.heard[u.Device.Name][u.Capability] = u.Value
}

// Seen implements driver.Sink. A message that changes nothing is published too: it's how Oikade
// knows a steady sensor is alive.
func (b *bridge) Seen(dev ident.ID, _ time.Time) {
	b.mu.Lock()
	values := b.heard[dev.Name]
	delete(b.heard, dev.Name)
	b.mu.Unlock()
	if values == nil {
		values = contractv1.State{}
	}
	b.check(b.plugin.ReportState(b.ctx, dev.Name, values), "Could not report state", dev.Name)
}

// Discovered implements driver.Sink. Inventory reports the same thing.
func (b *bridge) Discovered(ident.ID, driver.Info) {}

// Undeclared implements driver.Sink. Every device is reported, so none are.
func (b *bridge) Undeclared(driver.InstanceKey, int) {}

// device is how the plugin names a device to an Instance: by its ID, in no namespace.
func device(id string) ident.ID { return ident.ID{Name: id} }

// onlinePayload reads Zigbee2MQTT's states: {"state":"online"} from 2.x, and a bare string before.
func onlinePayload(payload []byte) bool {
	var obj struct {
		State string `json:"state"`
	}
	if json.Unmarshal(payload, &obj) == nil && obj.State != "" {
		return obj.State == "online"
	}
	return string(payload) == "online"
}

// clientID is random, so that a plugin's next process doesn't race the last one for its session.
func clientID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "oikade-zigbee2mqtt-" + hex.EncodeToString(b)
}
