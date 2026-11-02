package main

import (
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/node"
	"github.com/harrymitchinson/oikade/internal/render"
)

// runMatter is the `manager matter` subcommand: the Matter bridge for one Bridge object. It reads
// its setup code and attestation from files the operator mounts from the Bridge's Secret, keeps
// its fabric table in --data-dir, and doesn't use the Kubernetes API.
func runMatter(args []string) int {
	fs := flag.NewFlagSet("matter", flag.ExitOnError)
	bridge := fs.String("bridge", "", "The Bridge served, as namespace/name, for logs.")
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
	n, err := node.New(cfg)
	if err != nil {
		log.Error(err, "Failed to create Matter node")
		return 1
	}

	log.Info("Starting Matter bridge", "version", version, "bridge", *bridge, "port", *port,
		"fabrics", n.Fabrics().Count())
	if err := n.Run(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Matter bridge stopped")
		return 1
	}
	return 0
}

// readMatterSetup reads the files mounted from a Bridge's Secret.
func readMatterSetup(dir string) (node.Config, error) {
	return render.MatterSetup(func(key string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, key)) })
}
