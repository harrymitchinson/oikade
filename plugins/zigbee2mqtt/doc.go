// Package zigbee2mqtt is the plugin for a Zigbee2MQTT you run yourself, on a broker you run yourself.
// A home imports it, and its Config says where the broker is:
//
//	home.Plugin("zigbee", zigbee2mqtt.New(zigbee2mqtt.Config{
//		Broker: "mqtt://oikade@broker:1883", Password: oikade.Secret("broker"),
//	}))
//
// Oikade starts no Zigbee2MQTT, writes none of its configuration and holds none of its keys. Every
// paired device is reported by its IEEE address, so it's usable at once. Instance is the part that
// follows Zigbee2MQTT on the bus.
//
// The README beside this file says how to set it up, and what to set in Zigbee2MQTT.
package zigbee2mqtt
