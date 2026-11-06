// Package host is the long-lived process that owns a home's state: the devices, arbitration, the
// store and the socket your process talks to. It stays up across deploys and supervises your code,
// which runs apart so that it can die without touching any of this (OIK-143).
package host

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/registry"
	"github.com/harrymitchinson/oikade/internal/store"
)

const (
	// SocketName is the socket your process uses, in the data directory.
	SocketName = "host.sock"
	// pausedNamespace is where the store keeps paused rules.
	pausedNamespace = "paused"
)

// Host configures a host.
type Host struct {
	// DataDir holds the store and the socket.
	DataDir string
	// Home is your binary until one is deployed, and may be empty. A deployed version takes over from
	// it, for good. The host runs either as "<binary> automations".
	Home string
	// Settle is how long a new version must stay up. It defaults to DefaultSettle.
	Settle time.Duration
	// Devices are simulated, until drivers come to the host.
	Devices []SimulatedDevice
	// Simulate, if set, changes simulated devices from lines of JSON.
	Simulate io.Reader
	// HeartbeatTimeout defaults to DefaultHeartbeatTimeout.
	HeartbeatTimeout time.Duration
	// MetricsAddr is where metrics and health are served over HTTP, read-only. Empty serves none.
	MetricsAddr string
	// Matter serves a Matter bridge. Nil serves none.
	Matter *Matter
	// Broker is an MQTT broker of yours, for Outside.
	Broker string
	// Outside names plugins that run as processes of their own and speak the MQTT contract on
	// Broker, under oikade/v1/home/<name>.
	Outside []string
	// PluginSetTimeout is how long a plugin's Set may run. It defaults to DefaultPluginSetTimeout.
	PluginSetTimeout time.Duration
	Log              logr.Logger

	plugins *plugins
}

// Run serves until ctx ends, then stops your process and closes the store.
func (h *Host) Run(ctx context.Context) error {
	if h.HeartbeatTimeout <= 0 {
		h.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if h.PluginSetTimeout <= 0 {
		h.PluginSetTimeout = DefaultPluginSetTimeout
	}
	data, err := store.Open(h.DataDir)
	if err != nil {
		return fmt.Errorf("opening the data directory: %w", err)
	}
	defer func() { _ = data.Close() }()
	if h.Settle <= 0 {
		h.Settle = DefaultSettle
	}
	kept, err := openVersions(h.DataDir, data)
	if err != nil {
		return err
	}
	devicesOnRecord, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		return err
	}

	// The engine outlives ctx: your process may claim until it has stopped, so that comes first.
	engineCtx, stopEngine := context.WithCancel(context.WithoutCancel(ctx))
	defer stopEngine()
	eng := &engine.Engine{
		Log:          h.Log.WithName("arbitration"),
		Controls:     &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: h.Log},
		ControlLevel: engine.NewControlLevelGauge(),
	}
	srv := &server{
		log: h.Log.WithName("socket"), ctx: engineCtx, engine: eng,
		holds:   &engine.Stored[engine.HoldState]{Values: data.Namespace(engine.HoldStatesNamespace), Log: h.Log},
		fires:   &engine.Stored[time.Time]{Values: data.Namespace(engine.FireStatesNamespace), Log: h.Log},
		journal: data.Journal(),
		paused:  &engine.Stored[time.Time]{Values: data.Namespace(pausedNamespace), Log: h.Log},
		feed:    newFeed(), pushed: &pushed{},
	}
	sinks := []driver.Sink{eng, srv}
	bridge, err := h.openMatter(data)
	if err != nil {
		return err
	}
	if bridge.working() {
		sinks = append(sinks, bridge)
	}
	devices := hub.NewStore(h.Log.WithName("devices"), sinks...)
	known := newInventory(devicesOnRecord, devices, eng, h.Log.WithName("registry"))
	sim := &simulator{store: devices, known: known, log: h.Log.WithName("simulator")}
	socket := filepath.Join(h.DataDir, SocketName)
	h.plugins = newPlugins(h, socket, devices, known)
	h.plugins.returned = eng.DriverReturned
	srv.plugins = h.plugins
	setter := claimedSetter{engine: eng, setter: routedSetter{devices: devices, simulated: sim, plugins: h.plugins}}
	eng.Store, eng.Setter, srv.devices, srv.setter = devices, setter, devices, setter
	eng.Init()
	if bridge.working() {
		bridge.connect(devices, known, eng, setter)
	}
	if err := sim.start(h.Devices); err != nil {
		return err
	}
	// Their claims hold from here on, even if the plugin never comes back.
	h.plugins.attachRecorded()

	listener, err := listen(socket)
	if err != nil {
		return fmt.Errorf("opening the socket: %w", err)
	}
	apiListener, err := listen(filepath.Join(h.DataDir, api.SocketName))
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("opening the API socket: %w", err)
	}
	deploys := &deployer{
		log: h.Log.WithName("deploy"), versions: kept, srv: srv, settle: h.Settle, home: h.Home,
		swaps: make(chan *swap), connected: make(chan struct{}, 1),
	}
	srv.connected = func() {
		select {
		case deploys.connected <- struct{}{}:
		default:
		}
	}
	observing := h.observe(srv, eng)
	first := deploys.startup()
	var engineDone, serving sync.WaitGroup
	engineDone.Go(func() { eng.Run(engineCtx) })
	if err := deploys.removals(); err != nil {
		h.Log.Error(err, "Failed to end what removed rules left")
	}
	serving.Go(func() { h.plugins.run(ctx) })
	serving.Go(func() { srv.serve(listener) })
	if h.Simulate != nil {
		go sim.read(h.Simulate)
	}
	h.Log.Info("Started host", "dataDir", h.DataDir, "devices", len(h.Devices))
	serving.Go(func() { h.supervise(ctx, srv, socket, first, deploys) })
	calls := &apiServer{log: h.Log.WithName("api"), ctx: ctx, deploys: deploys, srv: srv, devices: known, bridge: bridge}
	serving.Go(func() { calls.serve(apiListener) })
	serving.Go(func() { observing(ctx) })
	if bridge.working() {
		serving.Go(func() { bridge.run(ctx) })
	}
	<-ctx.Done()
	_ = listener.Close()
	_ = apiListener.Close()
	serving.Wait()
	srv.shutdown()
	stopEngine()
	engineDone.Wait()
	h.Log.Info("Stopped host")
	return nil
}
