// Package hue is the plugin for a Philips Hue bridge, which it reads over the bridge's local API,
// CLIP v2. A home imports it once for each bridge:
//
//	home.Plugin("hue", hue.New(hue.Config{})).
//		HideFromMatter() // the Hue bridge puts these in the Home app itself
//
// The bridge's ID and the key it gave are one host secret, <plugin>.hue-bridge, as JSON. The plugin
// finds the bridge over mDNS, as the _hue._tcp instance whose TXT bridgeid is that ID, or at
// Config.Address. bridgeTLS checks the bridge's certificate against Philips Hue's two roots,
// embedded, and its CN against the ID, since the certificate has no SANs.
//
// A session opens the event stream, /eventstream/clip/v2, then reads /clip/v2/resource. A change
// during the read waits in the stream, and applying every change in order over the read leaves each
// value at its latest. The stream is server-sent events whose data is a list of the bridge's events,
// each a list of resources with only what changed: home.apply decodes each into the resource it
// changes, which keeps the rest. An event that adds or removes a resource the plugin reads, or
// changes a device or a room, has the plugin read everything again. The bridge sends nothing while
// nothing changes, so HTTP/2 pings show a connection that's gone.
//
// A device's ID is its Zigbee MAC, from its zigbee_connectivity, as Zigbee2MQTT writes it, such as
// 0x7cc6b6fffecb1814, so it survives a new bridge and a device that moves to Zigbee2MQTT. A device
// is online while its zigbee_connectivity is connected. build skips a device with no MAC, or with
// more than one light or motion sensor, and the log names it once. A light is on and, if it dims,
// brightness, in percent as the bridge has it. A motion sensor is occupied, temperature and battery.
// A value its sensor hasn't got yet is left out, and one turned off in the Hue app isn't occupied.
// A value its capability doesn't take is left out and logged once, rather than refused by the SDK.
//
// A session that lasted at least retryMax and whose stream then ends reads the bridge again at once,
// with its devices online. Anything else makes every device offline, says why in the status, and
// backs off from retry to retryMax: a refused key waits refused, and too many requests limited.
//
// The README beside this file is the plugin's user docs.
package hue
