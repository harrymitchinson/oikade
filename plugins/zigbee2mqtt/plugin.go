package zigbee2mqtt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// DefaultBaseTopic is Zigbee2MQTT's own default mqtt.base_topic.
const DefaultBaseTopic = "zigbee2mqtt"

// disconnectTimeout bounds leaving the broker.
const disconnectTimeout = 2 * time.Second

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
			log: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		}, nil
	}
}

// bridge is the plugin: it follows an Instance and reports what it hears through the SDK.
type bridge struct {
	config Config
	broker string
	// declared are the devices on record, which are reported even once they've left Zigbee2MQTT.
	declared []string
	log      *slog.Logger
	instance *Instance

	// ctx and plugin are set before anything can be heard.
	ctx    context.Context
	plugin *plugin.Plugin

	mu sync.Mutex
	// up is Zigbee2MQTT's own state, and online each device's last availability. A device with none
	// is taken as online, because Zigbee2MQTT reports availability only if it's turned on.
	up       bool
	online   map[string]bool
	reported map[string]bool
}

var _ plugin.Handler = (*bridge)(nil)

// Serve implements plugin.Handler.
func (b *bridge) Serve(ctx context.Context, p *plugin.Plugin) error {
	b.ctx, b.plugin = ctx, p
	// The connection outlives ctx, so that it can be left cleanly: a connection cut when ctx ends
	// makes everything after it wait for a reply that isn't coming, and a deploy wait with it.
	life, cut := context.WithCancel(logr.NewContext(context.WithoutCancel(ctx), logr.FromSlogHandler(b.log.Handler())))
	defer cut()
	connecting := context.AfterFunc(ctx, cut)
	client, err := mqtt.Connect(life, b.broker, clientID())
	if !connecting() || err != nil {
		return err
	}
	defer func() {
		leaving, cancel := context.WithTimeout(life, disconnectTimeout)
		defer cancel()
		_ = client.Disconnect(leaving)
	}()
	b.instance = &Instance{
		Key:       types.NamespacedName{Name: b.config.BaseTopic},
		BaseTopic: b.config.BaseTopic,
		Bus:       client,
		Sink:      b,
		Now:       time.Now,
		Log:       logr.FromSlogHandler(b.log.Handler()),
	}
	if _, err := client.Subscribe(ctx, b.config.BaseTopic+"/bridge/state", b.bridgeState); err != nil {
		return err
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

// bridgeState follows <base>/bridge/state, which is Zigbee2MQTT's Last Will. With no process of its
// own to watch, this is how Oikade learns Zigbee2MQTT has stopped.
func (b *bridge) bridgeState(m mqtt.Message) {
	up := onlinePayload(m.Payload)
	b.mu.Lock()
	changed := up != b.up
	b.up = up
	ids := slices.Sorted(maps.Keys(b.reported))
	b.mu.Unlock()
	if !changed {
		return
	}
	b.log.Info("Zigbee2MQTT changed state", "online", up)
	for _, id := range ids {
		b.publishAvailability(id)
	}
}

// publishAvailability reports a device as online only if it and Zigbee2MQTT are.
func (b *bridge) publishAvailability(id string) {
	b.mu.Lock()
	online, known := b.online[id]
	online = b.up && (online || !known)
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
	paired := make(map[string]types.NamespacedName, len(devices))
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
	// Discovered first: it's published with the devices, so the list arrives whole.
	b.check(b.plugin.SetDiscovered(b.ctx, discovered), "Could not report discovered devices", "")
	b.check(b.plugin.SetDevices(b.ctx, reports, 0), "Could not report devices", "")
	b.instance.SetDevices(paired)

	b.mu.Lock()
	b.reported = make(map[string]bool, len(reports))
	for _, r := range reports {
		b.reported[r.ID] = true
	}
	up := b.up
	b.mu.Unlock()
	if !up {
		for _, r := range reports {
			b.publishAvailability(r.ID)
		}
	}
}

// Availability implements driver.Sink.
func (b *bridge) Availability(dev types.NamespacedName, online bool) {
	b.mu.Lock()
	b.online[dev.Name] = online
	b.mu.Unlock()
	b.publishAvailability(dev.Name)
}

// Changed implements driver.Sink.
func (b *bridge) Changed(u driver.Update) {
	b.check(b.plugin.ReportState(b.ctx, u.Device.Name, contractv1.State{u.Capability: u.Value}),
		"Could not report state", u.Device.Name)
}

// Discovered implements driver.Sink. Inventory reports the same thing.
func (b *bridge) Discovered(types.NamespacedName, driver.Info) {}

// Seen implements driver.Sink. Oikade sees it in the state.
func (b *bridge) Seen(types.NamespacedName, time.Time) {}

// Undeclared implements driver.Sink. Every device is reported, so none are.
func (b *bridge) Undeclared(driver.InstanceKey, int) {}

// device is how the plugin names a device to an Instance: by its ID, in no namespace.
func device(id string) types.NamespacedName { return types.NamespacedName{Name: id} }

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
