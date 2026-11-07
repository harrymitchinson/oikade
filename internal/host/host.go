// Package host is the long-lived process that owns a home's state: the devices, arbitration, the
// store and the socket your process talks to. It stays up across deploys and supervises your code,
// which runs apart so that it can die without touching any of this (OIK-143).
package host

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/logging"
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
	// APIAddr is where the API is served over TLS, for clients with a token. Empty serves it only on
	// the socket. A missing port is DefaultAPIPort.
	APIAddr string
	// Matter serves a Matter bridge. Nil serves none.
	Matter *Matter
	// Broker is an MQTT broker of yours, for Outside and for imported plugins that use one.
	Broker string
	// SecretsDir holds one file per secret, which your process and plugins read. It defaults to
	// DefaultSecretsDir in DataDir. A mounted, read-only one works, and oikade secret set says so.
	SecretsDir string
	// Outside names plugins that run as processes of their own and speak the MQTT contract on
	// Broker, under oikade/v1/home/<name>.
	Outside []string
	// EventLog bounds what the host keeps for oikade replay. Zero fields take the defaults.
	EventLog store.Bound
	// PluginSetTimeout is how long a plugin's Set may run. It defaults to DefaultPluginSetTimeout.
	PluginSetTimeout time.Duration
	Log              *slog.Logger

	plugins *plugins
}

// Run serves until ctx ends, then stops your process and closes the store.
func (h *Host) Run(ctx context.Context) error {
	h.Log = logging.OrDiscard(h.Log)
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
	if h.SecretsDir == "" {
		h.SecretsDir = filepath.Join(h.DataDir, DefaultSecretsDir)
		if err := os.MkdirAll(h.SecretsDir, 0o700); err != nil {
			return fmt.Errorf("creating the secrets directory: %w", err)
		}
	}
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
		Log:          h.Log.With("logger", "arbitration"),
		Controls:     &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: h.Log},
		ControlLevel: engine.NewControlLevelGauge(),
	}
	srv := &server{
		log: h.Log.With("logger", "socket"), ctx: engineCtx, engine: eng,
		holds:   &engine.Stored[engine.HoldState]{Values: data.Namespace(engine.HoldStatesNamespace), Log: h.Log},
		fires:   &engine.Stored[time.Time]{Values: data.Namespace(engine.FireStatesNamespace), Log: h.Log},
		journal: data.Journal(),
		paused:  &engine.Stored[time.Time]{Values: data.Namespace(pausedNamespace), Log: h.Log},
		feed:    newFeed(), events: newEventLog(data.Events(), h.EventLog, h.Log.With("logger", "events")), pushed: &pushed{},
	}
	sinks := []driver.Sink{eng, srv}
	bridge, err := h.openMatter(data)
	if err != nil {
		return err
	}
	if bridge.working() {
		sinks = append(sinks, bridge)
	}
	devices := hub.NewStore(h.Log.With("logger", "devices"), sinks...)
	known := newInventory(devicesOnRecord, data.Namespace(homeSettingsNamespace), devices, eng, h.Log.With("logger", "registry"))
	if err := known.loadHome(); err != nil {
		return fmt.Errorf("reading the home's device settings: %w", err)
	}
	srv.settings, srv.confirmedSettings = known.takeHome, known.setHome
	sim := &simulator{store: devices, known: known, log: h.Log.With("logger", "simulator")}
	socket := filepath.Join(h.DataDir, SocketName)
	h.plugins = newPlugins(h, socket, devices, known)
	h.plugins.returned = eng.DriverReturned
	srv.plugins = h.plugins
	setter := claimedSetter{engine: eng, setter: routedSetter{devices: devices, simulated: sim, plugins: h.plugins}}
	recorded := recordedSetter{emit: srv.emit, setter: setter}
	eng.Store, eng.Setter, srv.devices, srv.setter, srv.outside = devices, recorded, devices, recorded, setter
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
		log: h.Log.With("logger", "deploy"), versions: kept, srv: srv, settle: h.Settle, home: h.Home,
		swaps: make(chan *swap), connected: make(chan struct{}, 1),
	}
	srv.connected = func() {
		select {
		case deploys.connected <- struct{}{}:
		default:
		}
	}
	calls := &apiServer{
		log: h.Log.With("logger", "api"), ctx: ctx, deploys: deploys, srv: srv, devices: known, bridge: bridge,
		data: data, dataDir: h.DataDir, secrets: secrets{dir: h.SecretsDir}, authFailures: newAuthFailures(),
	}
	network := h.listenAPI(data, calls)
	observing := h.observe(srv, eng, calls.authFailures)
	first := deploys.startup()
	var engineDone, serving sync.WaitGroup
	engineDone.Go(func() { eng.Run(engineCtx) })
	if err := deploys.removals(); err != nil {
		h.Log.Error("Failed to end what removed rules left", "err", err)
	}
	serving.Go(func() { h.plugins.run(ctx) })
	serving.Go(func() { srv.serve(listener) })
	if h.Simulate != nil {
		go sim.read(h.Simulate)
	}
	h.Log.Info("Started host", "dataDir", h.DataDir, "devices", len(h.Devices))
	serving.Go(func() { h.supervise(ctx, srv, socket, first, deploys) })
	serving.Go(func() { calls.serve(apiListener) })
	if network != nil {
		serving.Go(func() { calls.serveNetwork(network) })
	}
	serving.Go(func() { observing(ctx) })
	serving.Go(func() { srv.events.run(ctx) })
	if bridge.working() {
		serving.Go(func() { bridge.run(ctx) })
	}
	<-ctx.Done()
	_ = listener.Close()
	_ = apiListener.Close()
	if network != nil {
		_ = network.Close()
	}
	serving.Wait()
	srv.shutdown()
	stopEngine()
	engineDone.Wait()
	srv.events.flush()
	h.Log.Info("Stopped host")
	return nil
}

// listenAPI opens the network API's listener, if it's asked for. If it can't, the host carries on
// without it, as it does without metrics: your home doesn't need it.
func (h *Host) listenAPI(data *store.Store, calls *apiServer) net.Listener {
	if h.APIAddr == "" {
		return nil
	}
	addr := APIAddress(h.APIAddr)
	values := data.Namespace(apiNamespace)
	// Clients come first, and whatever happens to the listener: a token must be revocable while the
	// port is taken, or it would work again once it's free.
	known, err := loadClients(values)
	if err != nil {
		h.Log.Error("Failed to open the API listener", "err", err, "address", addr)
		return nil
	}
	calls.clients = known
	cert, pin, err := loadAPIIdentity(values)
	if err != nil {
		h.Log.Error("Failed to open the API listener", "err", err, "address", addr)
		return nil
	}
	calls.pin = pin
	l, err := net.Listen("tcp", addr)
	if err != nil {
		h.Log.Error("Failed to open the API listener", "err", err, "address", addr)
		return nil
	}
	calls.address = reachableAt(l.Addr())
	h.Log.Info("Serving API over TLS", "address", l.Addr().String(), "pin", pin)
	return tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
}

// reachableAt guesses where clients reach a listener: its address, or this machine's name for one on
// every address.
func reachableAt(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		if name, err := os.Hostname(); err == nil && name != "" {
			host = name
		}
	}
	return net.JoinHostPort(host, port)
}
