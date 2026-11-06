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

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/store"
)

// SocketName is the host's socket in the data directory.
const SocketName = "host.sock"

// Host configures a host.
type Host struct {
	// DataDir holds the store and the socket.
	DataDir string
	// Home is your binary. The host runs it as "<Home> automations".
	Home string
	// Devices are simulated, until drivers come to the host.
	Devices []SimulatedDevice
	// Simulate, if set, changes simulated devices from lines of JSON.
	Simulate io.Reader
	// HeartbeatTimeout defaults to DefaultHeartbeatTimeout.
	HeartbeatTimeout time.Duration
	Log              logr.Logger
}

// Run serves until ctx ends, then stops your process and closes the store.
func (h *Host) Run(ctx context.Context) error {
	if h.HeartbeatTimeout <= 0 {
		h.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	data, err := store.Open(h.DataDir)
	if err != nil {
		return fmt.Errorf("opening the data directory: %w", err)
	}
	defer func() { _ = data.Close() }()

	// The engine outlives ctx: your process may claim until it has stopped, so that comes first.
	engineCtx, stopEngine := context.WithCancel(context.WithoutCancel(ctx))
	defer stopEngine()
	eng := &engine.Engine{
		Log:      h.Log.WithName("arbitration"),
		Controls: &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: h.Log},
	}
	srv := &server{
		log: h.Log.WithName("socket"), ctx: engineCtx, engine: eng,
		holds:   &engine.Stored[engine.HoldState]{Values: data.Namespace(engine.HoldStatesNamespace), Log: h.Log},
		journal: data.Journal(),
	}
	devices := hub.NewStore(h.Log.WithName("devices"), eng, srv)
	sim := &simulator{store: devices, log: h.Log.WithName("simulator")}
	eng.Store, eng.Setter, srv.devices, srv.setter = devices, sim, devices, sim
	eng.Init()
	if err := sim.start(h.Devices); err != nil {
		return err
	}

	socket := filepath.Join(h.DataDir, SocketName)
	listener, err := listen(socket)
	if err != nil {
		return fmt.Errorf("opening the socket: %w", err)
	}
	var engineDone, serving sync.WaitGroup
	engineDone.Go(func() { eng.Run(engineCtx) })
	serving.Go(func() { srv.serve(listener) })
	if h.Simulate != nil {
		go sim.read(h.Simulate)
	}
	h.Log.Info("Started host", "dataDir", h.DataDir, "devices", len(h.Devices))
	if h.Home != "" {
		serving.Go(func() { h.supervise(ctx, srv, socket) })
	}
	<-ctx.Done()
	_ = listener.Close()
	serving.Wait()
	srv.shutdown()
	stopEngine()
	engineDone.Wait()
	h.Log.Info("Stopped host")
	return nil
}
