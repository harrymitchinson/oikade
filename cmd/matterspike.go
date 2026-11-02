package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/node"
)

// spikeEndpoint is the bridged on/off endpoint's ID.
const spikeEndpoint = 2

// runMatterSpike is the `manager matter-spike` subcommand: a throwaway Matter bridge with one
// bridged on/off endpoint, to check that a real commissioner adds it (OIK-81). It has no API,
// controller or MQTT: the switch is only state in memory, and each change is logged.
func runMatterSpike(args []string) int {
	fs := flag.NewFlagSet("matter-spike", flag.ExitOnError)
	dataDir := fs.String("data-dir", "",
		"Directory for the fabric table and setup code. Losing it means re-commissioning.")
	port := fs.Uint("port", 5540, "Matter UDP port.")
	ifaces := fs.String("interfaces", "",
		"Comma-separated interfaces to advertise on over mDNS. Empty means every multicast interface.")
	openWindow := fs.Bool("open-window", false,
		"Open the commissioning window at start even when the node is already on a fabric.")
	var logLevel slog.Level
	fs.TextVar(&logLevel, "log-level", slog.LevelInfo, "Minimum log level: debug, info, warn, or error.")
	showVersion := versionFlag(fs)
	_ = fs.Parse(args)
	if *showVersion {
		printVersion(os.Stdout)
		return 0
	}
	configureLogging(logLevel)
	log := ctrl.Log.WithName("matter-spike")
	if *dataDir == "" || *port == 0 || *port > 0xFFFF {
		log.Error(fmt.Errorf("missing configuration"), "Missing --data-dir, or --port isn't a UDP port")
		return 2
	}

	var mdnsOpts mdns.Options
	if *ifaces != "" {
		list, err := node.Interfaces(strings.Split(*ifaces, ","))
		if err != nil {
			log.Error(err, "Failed to find interface")
			return 2
		}
		mdnsOpts.Interfaces = list
	}
	n, err := node.New(node.Config{
		DataDir: *dataDir, Port: uint16(*port), DeviceName: "Oikade spike", MDNS: mdnsOpts, Log: log,
		Info: clusters.Info{
			VendorName: "Oikade", ProductName: "Oikade Matter spike", NodeLabel: "Oikade spike",
			HardwareVersionString: "spike", SoftwareVersion: 1, SoftwareVersionString: version,
			UniqueID: "oikade-matter-spike",
		},
	})
	if err != nil {
		log.Error(err, "Failed to create Matter node")
		return 1
	}
	onoff := clusters.NewOnOff()
	onoff.OnChange = func(on bool) { log.Info("Switched bridged switch", "endpoint", spikeEndpoint, "on", on) }
	_, err = n.Bridge().AddDevice(spikeEndpoint, clusters.DeviceOnOffPlug, "Spike switch", "oikade-spike-switch",
		onoff.Cluster())
	if err != nil {
		log.Error(err, "Failed to add bridged device")
		return 1
	}

	p := n.Payload()
	qr, err := p.QRCode()
	if err != nil {
		log.Error(err, "Failed to encode QR code")
		return 1
	}
	manual, err := p.ManualCode()
	if err != nil {
		log.Error(err, "Failed to encode manual pairing code")
		return 1
	}
	log.Info("Starting Matter spike", "version", version, "port", *port, "dataDir", *dataDir,
		"fabrics", n.Fabrics().Count(), "manualCode", manual, "qrCode", qr,
		"passcode", p.Passcode, "discriminator", p.Discriminator)
	if err := n.Run(ctrl.SetupSignalHandler(), *openWindow); err != nil {
		log.Error(err, "Matter spike stopped")
		return 1
	}
	return 0
}
