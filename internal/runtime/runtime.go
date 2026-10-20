/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
	eng := &engine.Engine{
		Location: opts.Location, Site: opts.Site, Log: log.WithName("engine"),
		Runs: engine.NewRunsCounter(), Dropped: engine.NewDroppedCounter(), NextChange: engine.NewNextChangeGauge(),
		OnStart: triggered.Record, OnSchedule: scheduled.Record,
	}
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
	if err := scheduled.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	for _, c := range []prometheus.Collector{metrics.Collector{Store: store, Plugins: h}, eng.Runs, eng.Dropped, eng.NextChange} {
		if err := opts.Registry.Register(c); err != nil {
			return nil, err
		}
	}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		eng.Run(ctx)
		return nil
	})); err != nil {
		return nil, err
	}
	return &Runtime{Store: store, Hub: h, Engine: eng}, nil
}
