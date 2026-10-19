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

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"
	_ "time/tzdata" // distroless images may lack zone data; automations need it

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/runtime"
)

// runRuntime is the `manager runtime` subcommand: the data plane. It follows every driver on the
// MQTT bus, writes Device status, exports metrics and runs Automations.
func runRuntime(args []string) int {
	fs := flag.NewFlagSet("runtime", flag.ExitOnError)
	mqttURL := fs.String("mqtt-url", "mqtt://oikade-mosquitto.oikade-system.svc:1883", "MQTT broker.")
	tz := fs.String("timezone", envOr("TZ", "UTC"), "IANA timezone automations see as tz, e.g. Europe/London.")
	metricsAddr := fs.String("metrics-bind-address", ":8080", "Address for the Prometheus endpoint.")
	probeAddr := fs.String("health-probe-bind-address", ":8081", "Address for health probes.")
	seen := fs.Duration("seen-interval", 5*time.Minute, "Minimum interval between Device lastSeen updates.")
	triggered := fs.Duration("triggered-interval", time.Minute,
		"Minimum interval between Automation lastTriggered updates.")
	var logLevel slog.Level
	fs.TextVar(&logLevel, "log-level", slog.LevelInfo, "Minimum log level: debug, info, warn, or error.")
	_ = fs.Parse(args)
	configureLogging(logLevel)
	log := ctrl.Log.WithName("runtime")

	loc, err := time.LoadLocation(*tz)
	if err != nil || loc == time.Local {
		log.Error(fmt.Errorf("invalid timezone %q", *tz), "--timezone must be an IANA name such as Europe/London")
		return 2
	}

	ctx := logr.NewContext(ctrl.SetupSignalHandler(), log)
	bus, err := mqtt.Connect(ctx, *mqttURL, clientID("oikade-runtime"))
	if err != nil {
		log.Error(err, "Failed to connect to MQTT broker")
		return 1
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
	})
	if err != nil {
		log.Error(err, "Failed to create manager")
		return 1
	}

	if _, err := runtime.Setup(ctx, mgr, bus, runtime.Options{
		Location: loc, SeenInterval: *seen, TriggeredInterval: *triggered, Registry: ctrlmetrics.Registry,
	}); err != nil {
		log.Error(err, "Failed to set up runtime")
		return 1
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "Failed to set up health check")
		return 1
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "Failed to set up ready check")
		return 1
	}

	log.Info("Starting runtime", "mqtt", *mqttURL, "timezone", loc.String())
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "Runtime stopped")
		return 1
	}
	<-bus.Done()
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// clientID returns a unique MQTT client ID. Brokers disconnect an older client when a new one
// connects with the same ID, so it includes the pod name.
func clientID(prefix string) string {
	host, err := os.Hostname()
	if err != nil {
		return prefix
	}
	return prefix + "-" + host
}
