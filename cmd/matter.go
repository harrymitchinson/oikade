package main

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/matterbridge"
	"github.com/harrymitchinson/oikade/internal/mqtt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/node"
	"github.com/harrymitchinson/oikade/internal/render"
)

// runMatter is the `manager matter` subcommand: the Matter bridge for one Bridge object. It reads
// its setup code and attestation from files the operator mounts from the Bridge's Secret, keeps
// its fabric table in --data-dir, reads its Bridge and Devices from the API, and follows the bus
// itself, so controllers keep working while the runtime is down. It never writes to the API.
func runMatter(args []string) int {
	fs := flag.NewFlagSet("matter", flag.ExitOnError)
	bridge := fs.String("bridge", "", "Name of the Bridge to serve, in $POD_NAMESPACE.")
	mqttURL := fs.String("mqtt-url", "mqtt://oikade-mosquitto.oikade-system.svc:1883", "MQTT broker.")
	name := fs.String("name", "Oikade", "The bridge's name (NodeLabel), at most 32 bytes.")
	port := fs.Uint("port", 5540, "Matter UDP port.")
	dataDir := fs.String("data-dir", "/data", "Directory for the fabric table. Losing it means commissioning again.")
	setupDir := fs.String("setup-dir", render.MatterSetupPath,
		"Directory with the setup code, unique ID and attestation, one file per key of the Bridge's Secret.")
	ifaces := fs.String("interfaces", "",
		"Comma-separated interfaces to advertise on over mDNS. Empty means every multicast interface.")
	window := fs.Duration("window-timeout", node.DefaultWindowTimeout,
		"How long the initial commissioning window stays open while the bridge is on no fabric, from 3m to 15m.")
	var logLevel slog.Level
	fs.TextVar(&logLevel, "log-level", slog.LevelInfo, "Minimum log level: debug, info, warn, or error.")
	showVersion := versionFlag(fs)
	_ = fs.Parse(args)
	if *showVersion {
		printVersion(os.Stdout)
		return 0
	}
	configureLogging(logLevel)
	log := ctrl.Log.WithName("matter")
	badWindow := *window < node.MinWindowTimeout || *window > node.DefaultWindowTimeout
	if *port == 0 || *port > 0xFFFF || len(*name) > 32 || badWindow {
		log.Error(errors.New("invalid configuration"),
			"--port must be a UDP port, --name at most 32 bytes, and --window-timeout from 3m to 15m")
		return 2
	}
	ns := os.Getenv("POD_NAMESPACE")
	if *bridge == "" || ns == "" {
		log.Error(errors.New("missing configuration"), "--bridge and $POD_NAMESPACE are required")
		return 2
	}
	key := types.NamespacedName{Namespace: ns, Name: *bridge}
	cfg, err := readMatterSetup(*setupDir)
	if err != nil {
		log.Error(err, "Failed to read setup", "dir", *setupDir)
		return 2
	}

	if *ifaces != "" {
		list, err := node.Interfaces(strings.Split(*ifaces, ","))
		if err != nil {
			log.Error(err, "Failed to find interface")
			return 2
		}
		cfg.MDNS = mdns.Options{Interfaces: list}
	}
	cfg.DataDir, cfg.Port, cfg.WindowTimeout, cfg.DeviceName, cfg.Log = *dataDir, uint16(*port), *window, *name, log
	cfg.Info.VendorName, cfg.Info.ProductName, cfg.Info.NodeLabel = "Oikade", "Oikade bridge", *name
	cfg.Info.SoftwareVersion, cfg.Info.SoftwareVersionString = 1, version
	ctx, cancel := context.WithCancel(logr.NewContext(ctrl.SetupSignalHandler(), log))
	defer cancel()
	bus, err := mqtt.Connect(ctx, *mqttURL, clientID("oikade-matter"))
	if err != nil {
		log.Error(err, "Failed to connect to MQTT broker")
		return 1
	}
	var fabrics *matterbridge.FabricsPublisher
	cfg.FabricsChanged = func() { fabrics.Publish(ctx) }
	n, err := node.New(cfg)
	if err != nil {
		log.Error(err, "Failed to create Matter node")
		return 1
	}
	fabrics = &matterbridge.FabricsPublisher{Bus: bus, Bridge: key, Table: n.Fabrics()}

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
	b := &matterbridge.Bridge{Client: mgr.GetClient(), Key: key, Matter: n.Bridge()}
	store := hub.NewStore(log.WithName("store"), b)
	h := &hub.Hub{Client: mgr.GetClient(), Bus: bus, Store: store}
	b.Store = store
	b.Writes = &matterbridge.Coalescer{
		Setter: &matterbridge.Announcer{Setter: h, Bus: bus, Store: store, Bridge: key},
		Current: func(dev types.NamespacedName, name string) (any, bool) {
			snap, ok := store.Get(dev)
			if !ok {
				return nil, false
			}
			v, ok := snap.Values[name]
			return v, ok
		},
		Timeout: 5 * time.Second,
	}
	b.Writes.Start(ctx)
	if err := h.SetupWithManager(ctx, mgr, "hub"); err != nil {
		log.Error(err, "Failed to create controller", "controller", "hub")
		return 1
	}
	if err := b.SetupWithManager(mgr); err != nil {
		log.Error(err, "Failed to create controller", "controller", "matter-bridge")
		return 1
	}

	log.Info("Starting Matter bridge", "version", version, "bridge", key, "port", *port, "mqtt", *mqttURL,
		"fabrics", n.Fabrics().Count())
	go fabrics.Run(ctx, time.Minute)
	served := make(chan error, 1)
	go func() {
		// The node stopping, say because its port is taken, stops the role.
		served <- n.Run(ctx)
		cancel()
	}()
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "Matter bridge stopped")
		return 1
	}
	if err := <-served; err != nil {
		log.Error(err, "Matter node stopped")
		return 1
	}
	<-bus.Done()
	return 0
}

// readMatterSetup reads the files mounted from a Bridge's Secret.
func readMatterSetup(dir string) (node.Config, error) {
	return render.MatterSetup(func(key string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, key)) })
}
