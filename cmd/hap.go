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
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/harry/oikade/internal/hap"
	"github.com/harry/oikade/internal/hub"
	"github.com/harry/oikade/internal/mqtt"
)

// runHAP is the `manager hap` subcommand: the HomeKit bridge for one Bridge object. It follows the
// bus itself, so HomeKit keeps working if the runtime is down, and never writes to the API.
func runHAP(args []string) int {
	fs := flag.NewFlagSet("hap", flag.ExitOnError)
	bridge := fs.String("bridge", "", "Name of the Bridge to serve.")
	mqttURL := fs.String("mqtt-url", "mqtt://oikade-mosquitto.oikade-system.svc:1883", "MQTT broker.")
	dataDir := fs.String("data-dir", "/data", "Directory for pairings and keys. Losing it means re-pairing.")
	opts := zap.Options{}
	opts.BindFlags(fs)
	_ = fs.Parse(args)
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("hap")

	ns := os.Getenv("POD_NAMESPACE")
	if *bridge == "" || ns == "" {
		log.Error(fmt.Errorf("missing configuration"), "--bridge and $POD_NAMESPACE are required")
		return 2
	}

	ctx := logr.NewContext(ctrl.SetupSignalHandler(), log)
	bus, err := mqtt.Connect(ctx, *mqttURL, clientID("oikade-hap"))
	if err != nil {
		log.Error(err, "Failed to connect to MQTT broker")
		return 1
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		log.Error(err, "Failed to create manager")
		return 1
	}

	b := &hap.Bridge{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		Key: types.NamespacedName{Namespace: ns, Name: *bridge}, DataDir: *dataDir,
	}
	store := hub.NewStore(log.WithName("store"), b)
	h := &hub.Hub{Client: mgr.GetClient(), Bus: bus, Store: store}
	b.Store, b.Setter = store, h
	if err := h.SetupWithManager(ctx, mgr, "hub"); err != nil {
		log.Error(err, "Failed to create controller", "controller", "hub")
		return 1
	}
	if err := b.SetupWithManager(ctx, mgr); err != nil {
		log.Error(err, "Failed to create controller", "controller", "hap-bridge")
		return 1
	}

	log.Info("Starting HomeKit bridge", "bridge", b.Key, "mqtt", *mqttURL)
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "HomeKit bridge stopped")
		return 1
	}
	<-bus.Done()
	return 0
}
