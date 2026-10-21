// Package runtime assembles the data plane: driver hub, Device status, metrics and rules.
package runtime

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/hap"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/metrics"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/status"
	"github.com/harrymitchinson/oikade/internal/sun"
)

// Options configures the runtime.
type Options struct {
	Location *time.Location
	// Site is where sun triggers are computed for. Nil means they never fire.
	Site              *sun.Site
	SeenInterval      time.Duration
	TriggeredInterval time.Duration
	// Registry receives the runtime's metrics.
	Registry prometheus.Registerer
}

// Runtime is the assembled data plane.
type Runtime struct {
	Store  *hub.Store
	Hub    *hub.Hub
	Engine *engine.Engine
}

// Setup wires the runtime into mgr. Its controllers start with the manager.
func Setup(ctx context.Context, mgr ctrl.Manager, bus mqtt.Bus, opts Options) (*Runtime, error) {
	log := logr.FromContextOrDiscard(ctx)
	writer := &status.Writer{Client: mgr.GetClient(), SeenInterval: opts.SeenInterval}
	triggered := &engine.Triggered{Client: mgr.GetClient(), Interval: opts.TriggeredInterval}
	scheduled := &engine.Scheduled{Client: mgr.GetClient()}
	controlStatus := &engine.ControlStatus{Client: mgr.GetClient()}
	skipped := &engine.Skipped{Client: mgr.GetClient()}
	eng := &engine.Engine{
		Location: opts.Location, Site: opts.Site, Log: log.WithName("engine"),
		Runs: engine.NewRunsCounter(), Dropped: engine.NewDroppedCounter(), NextChange: engine.NewNextChangeGauge(),
		Active: engine.NewActiveGauge(), ControlLevel: engine.NewControlLevelGauge(),
		OnStart: triggered.Record, OnSchedule: scheduled.Record, OnControl: controlStatus.Record, OnSkipped: skipped.Record,
	}
	holds := &engine.Retained[engine.HoldState]{Bus: bus, Root: engine.HoldStatesRoot, Log: log.WithName("holds")}
	controls := &engine.Retained[engine.ControlState]{Bus: bus, Root: engine.ControlStatesRoot, Log: log.WithName("control")}
	eng.States, eng.Controls = holds, controls
	eng.Init()
	store := hub.NewStore(log.WithName("store"), writer, eng)
	writer.Store, eng.Store = store, store
	h := &hub.Hub{Client: mgr.GetClient(), Bus: bus, Store: store}
	eng.Setter = h

	if err := h.SetupWithManager(ctx, mgr, "hub"); err != nil {
		return nil, err
	}
	if err := writer.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&engine.Loader{Client: mgr.GetClient(), Engine: eng}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := triggered.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	for _, r := range []interface{ SetupWithManager(ctrl.Manager) error }{scheduled, controlStatus, skipped} {
		if err := r.SetupWithManager(mgr); err != nil {
			return nil, err
		}
	}
	for _, c := range []prometheus.Collector{metrics.Collector{Store: store, Plugins: h}, eng.Runs, eng.Dropped, eng.NextChange, eng.Active, eng.ControlLevel} {
		if err := opts.Registry.Register(c); err != nil {
			return nil, err
		}
	}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		// The subscription stays registered if it fails, and is restored on reconnect. Until then,
		// Home app writes are recorded as from the device.
		if _, err := hap.SubscribeWrites(logr.NewContext(ctx, log.WithName("homekit")), bus, eng.HomeKitWrote); err != nil && ctx.Err() == nil {
			log.Error(err, "Failed to subscribe to HomeKit writes")
		}
		// Holds and arbiters continue from their saved state, so it must be in before any is
		// evaluated. Sync waits as long as the broker takes, and returns early only when ctx ends.
		for _, s := range []interface{ Sync(context.Context) error }{holds, controls} {
			if err := s.Sync(ctx); err != nil {
				return err
			}
		}
		eng.Run(ctx)
		return nil
	})); err != nil {
		return nil, err
	}
	return &Runtime{Store: store, Hub: h, Engine: eng}, nil
}
