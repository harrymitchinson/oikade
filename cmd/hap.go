package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/harrymitchinson/oikade/internal/hap"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// runHAP is the `manager hap` subcommand: the HomeKit bridge for one Bridge object. It follows the
// bus itself, so HomeKit keeps working if the runtime is down, and never writes to the API.
func runHAP(args []string) int {
	fs := flag.NewFlagSet("hap", flag.ExitOnError)
	bridge := fs.String("bridge", "", "Name of the Bridge to serve.")
	mqttURL := fs.String("mqtt-url", "mqtt://oikade-mosquitto.oikade-system.svc:1883", "MQTT broker.")
	dataDir := fs.String("data-dir", "/data", "Directory for pairings and keys. Losing it means re-pairing.")
	var logLevel slog.Level
	fs.TextVar(&logLevel, "log-level", slog.LevelInfo, "Minimum log level: debug, info, warn, or error.")
	showVersion := versionFlag(fs)
	_ = fs.Parse(args)
	if *showVersion {
		printVersion(os.Stdout)
		return 0
	}
	configureLogging(logLevel)
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
	b.Store, b.Setter = store, &hap.Announcer{Setter: h, Bus: bus, Store: store, Bridge: b.Key}
	if err := h.SetupWithManager(ctx, mgr, "hub"); err != nil {
		log.Error(err, "Failed to create controller", "controller", "hub")
		return 1
	}
	if err := b.SetupWithManager(ctx, mgr); err != nil {
		log.Error(err, "Failed to create controller", "controller", "hap-bridge")
		return 1
	}

	log.Info("Starting HomeKit bridge", "version", version, "bridge", b.Key, "mqtt", *mqttURL)
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "HomeKit bridge stopped")
		return 1
	}
	<-bus.Done()
	return 0
}
