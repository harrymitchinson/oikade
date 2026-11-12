// Package ewelinklan is the plugin for devices that speak the eWeLink LAN protocol. A home imports
// it, and it finds them on the network by itself:
//
//	framework.NewHome(framework.Options{},
//		framework.NewPlugin("ewelink-lan", ewelinklan.New(ewelinklan.Config{})),
//	)
//
// A device announces itself over mDNS as _ewelink._tcp, with its ID and what kind of device it is in
// the clear, and its state encrypted with its LAN key. So a device is reported as soon as it's heard,
// and needs its key, in a secret named for its ID, before it can be read or switched. A device with
// no key shows that as its problem, and is offline until oikade secret set adds it, with no deploy.
//
// What a kind of device can do is one entry in kinds. A kind the table doesn't have isn't reported.
//
// The README beside this file says how to set it up, and how to get each device's LAN key.
package ewelinklan
