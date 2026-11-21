// Package ewelinklan is the plugin for devices that speak the eWeLink LAN protocol. A home imports
// it, and it finds the devices on the network by itself:
//
//	home.Plugin("ewelink-lan", ewelinklan.New(ewelinklan.Config{
//		AppID:     oikade.Var("EWELINK_APP_ID"),
//		AppSecret: oikade.Var("EWELINK_APP_SECRET"),
//	}))
//
// It joins the mDNS group with mdns.ListenMulticast, asks for _ewelink._tcp, and follows the
// answers, which can arrive over several packets (records). A device's TXT record carries its ID,
// which is its ID here, and its type in the clear, and its report encrypted: AES-128-CBC, keyed
// with the MD5 of the device's LAN key. The type picks an entry in kinds (the command's path and
// body, the capabilities and how to read a report), so another type of device is another entry,
// once a real device has confirmed it. A type that isn't there isn't reported.
//
// A device is reported as soon as it's heard, and stays reported for the life of the process. Its
// LAN key is the secret Setup kept under its ID, read again whenever a report can't be decrypted,
// so a setup run again needs no restart. Until there's one, the device's problem says so and it's
// offline. A report that can't be decrypted while good ones still arrive is damage, and once
// they've stopped, a wrong key. Availability follows the age of the last relay report (relayAge).
// Everything goes out from Serve's goroutine, so what's published stays in order.
//
// A write is an encrypted POST to the device, and Set returns once a newer report shows the relay
// where it was asked to go, or at once if it was there already.
//
// Setup signs in to eWeLink's cloud through the browser, with the developer app in Config, and
// keeps each device's key as a secret. It keeps no token, so the plugin talks to the cloud only
// while setup runs.
//
// The README beside this file is the plugin's user docs.
package ewelinklan
