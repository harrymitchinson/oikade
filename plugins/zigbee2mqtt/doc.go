// Package zigbee2mqtt is the plugin for a Zigbee2MQTT you run yourself, on a broker you run yourself.
// A home imports it, and the host's --broker says where the broker is:
//
//	framework.Home{
//		Plugins: []framework.Plugin{{Name: "zigbee", New: zigbee2mqtt.New(zigbee2mqtt.Config{})}},
//	}
//
// Oikade starts no Zigbee2MQTT, writes none of its configuration and holds none of its keys. Every
// paired device is reported by its IEEE address, so it's usable at once. Instance is the part that
// follows Zigbee2MQTT on the bus, which the operator also uses until it's removed.
package zigbee2mqtt
